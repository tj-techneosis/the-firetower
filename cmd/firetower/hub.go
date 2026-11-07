package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// A running board is this machine's hub: it already follows every source,
// holds the key and routes actions to the right machine. Everything else on
// the machine (the bar, a bar plugin, `firetower seen` for another machine's
// lookout) asks it first, over loopback with the key, and only does the work
// itself when no board is up. So a bar refreshing every few seconds costs a
// request, not an ssh login per source.

// boardAsk is how long a board on loopback gets to answer. A healthy one
// answers in milliseconds; one that doesn't is treated as not there.
const boardAsk = 2 * time.Second

// errNoBoard: no board of ours is up on this machine (none on the port, or
// one that isn't ours: another person's, whose key this isn't).
var errNoBoard = errors.New("no board is running")

// savedBoardKey is the board's key if one was ever made: read only, so asking
// never makes one.
func savedBoardKey(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, ".board-key"))
	if k := strings.TrimSpace(string(b)); err == nil && len(k) >= 32 {
		return k
	}
	return ""
}

// proofOf is what a board holding key, on port, answers to nonce. The port
// is in it, so a stranger on this port can't pass the question on to the
// person's board on another port and hand back its answer.
func proofOf(key string, port int, nonce string) string {
	m := hmac.New(sha256.New, []byte(key))
	fmt.Fprintf(m, "firetower-board-proof:%d:%s", port, nonce)
	return hex.EncodeToString(m.Sum(nil))
}

// pinned is a client that talks over one connection to the port, and never
// opens a second: what it proved is who it then sends the key to. If that
// connection goes, the next request fails rather than reach whoever holds
// the port by then.
func pinned(port int, wait time.Duration) *http.Client {
	var dialed bool
	var mu sync.Mutex
	tr := &http.Transport{
		MaxConnsPerHost: 1,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			mu.Lock()
			defer mu.Unlock()
			if dialed {
				return nil, errors.New("the board's connection closed")
			}
			dialed = true
			var d net.Dialer
			return d.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
		},
	}
	return &http.Client{Transport: tr, Timeout: wait}
}

// proveOn: the board at the other end of c holds key. It asks with a fresh
// nonce and checks the answer, so the key itself is never sent to find out.
func proveOn(c *http.Client, port int, key string) bool {
	if key == "" {
		return false
	}
	b := make([]byte, 16)
	rand.Read(b)
	nonce := hex.EncodeToString(b)
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/api/proof?nonce=%s", port, nonce))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var got struct {
		Proof string `json:"proof"`
	}
	ok := resp.StatusCode == http.StatusOK && json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&got) == nil
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096)) // read to the end, so the connection is reused
	return ok && hmac.Equal([]byte(got.Proof), []byte(proofOf(key, port, nonce)))
}

// proveBoard: the board on the port holds key.
func proveBoard(port int, key string, wait time.Duration) bool {
	return proveOn(pinned(port, wait), port, key)
}

// proofWait is how long a board on loopback gets to prove itself: one that
// can't in this long is busy enough to be passed over (the bar dials the
// sources itself instead), and a stranger that hangs costs no more.
const proofWait = 500 * time.Millisecond

// ourBoard: this person's board is up on the port.
func ourBoard(port int, key string) bool { return proveBoard(port, key, proofWait) }

// boardRequest is a request to this person's board, with the key: on the
// one connection the board proved itself on, so a stranger never sees the
// key, and only then is its answer believed. errNoBoard when no board of
// ours answers; once one has, any failure is a real error.
func boardRequest(cfg *Config, method, path string, body []byte) (*http.Response, error) {
	return boardRequestProving(cfg, method, path, body, proofWait)
}

// boardRequestProving is boardRequest with the board given prove to prove
// itself (a stop waits longer for a busy board than a bar does).
func boardRequestProving(cfg *Config, method, path string, body []byte, prove time.Duration) (*http.Response, error) {
	key := savedBoardKey(stateDir())
	wait := boardAsk
	if method == "POST" {
		wait = 40 * time.Second // an act over ssh may take a while; the board gives it 30s
	}
	c := pinned(cfg.Board.Port, wait)
	pc := *c
	pc.Timeout = prove
	if !proveOn(&pc, cfg.Board.Port, key) {
		return nil, errNoBoard
	}
	req, err := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", cfg.Board.Port, path), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the board didn't answer (%v); it may have done it anyway, so look before trying again", err)
	}
	return resp, nil
}

// onceURL is a one-time address for the board: it lets one browser in, once,
// within a minute, and is worthless after. For whatever can't keep a secret
// off a command line (a browser's, a launcher's).
func onceURL(port int) (string, error) {
	b := make([]byte, 24)
	rand.Read(b)
	tok := hex.EncodeToString(b)
	dir := stateDir()
	unlock, err := lockDir(dir)
	if err != nil {
		return "", err
	}
	defer unlock()
	toks := readOnce(dir)
	toks[tok] = time.Now().Add(time.Minute).Unix()
	if err := writeOnce(dir, toks); err != nil {
		return "", err
	}
	return fmt.Sprintf("http://127.0.0.1:%d/?once=%s", port, tok), nil
}

// claimOnce spends a one-time token: ok once, for a token made in the last
// minute. stolen says the token was spent already, by someone else: the
// real window arriving after a thief who read the address off a command
// line. The board answers that by changing its key.
func claimOnce(dir, tok string) (ok, stolen bool) {
	if len(tok) != 48 || strings.Trim(tok, "0123456789abcdef") != "" {
		return false, false
	}
	unlock, err := lockDir(dir)
	if err != nil {
		return false, false
	}
	defer unlock()
	toks := readOnce(dir)
	exp, known := toks[tok]
	if !known {
		return false, false // nothing to write for a token that was never made
	}
	if exp < 0 {
		return false, true
	}
	if time.Now().Unix() > exp {
		return false, false
	}
	toks[tok] = -time.Now().Unix() // spent, and remembered a while as spent
	if writeOnce(dir, toks) != nil {
		return false, false // not written down as spent: not given out
	}
	return true, false
}

func readOnce(dir string) map[string]int64 {
	toks := map[string]int64{}
	b, _ := os.ReadFile(filepath.Join(dir, ".board-once"))
	json.Unmarshal(b, &toks)
	now := time.Now().Unix()
	for t, exp := range toks {
		// Live tokens until they expire; spent ones (negative: when) for ten
		// minutes, to know a second use when it comes.
		if (exp >= 0 && exp < now) || (exp < 0 && -exp < now-600) {
			delete(toks, t)
		}
	}
	return toks
}

func writeOnce(dir string, toks map[string]int64) error {
	b, _ := json.Marshal(toks)
	path := filepath.Join(dir, ".board-once")
	if err := os.WriteFile(path+".tmp", b, 0o600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

// askBoard is a running board's whole picture, as its own page gets it.
func askBoard(cfg *Config) (*State, error) {
	resp, err := boardRequest(cfg, "GET", "/api/state", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the board said %s", resp.Status)
	}
	var st State
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&st); err != nil {
		return nil, fmt.Errorf("the board's answer didn't read: %v", err)
	}
	if st.Lookouts == nil {
		st.Lookouts = []View{}
	}
	if st.Sources == nil {
		st.Sources = []SourceStatus{}
	}
	return &st, nil
}

// tellBoard does what a board's button does (seen, done, rm, expect) through
// the running board, which routes another machine's lookout to it. errNoBoard
// when none is up; any other error is the board's answer.
func tellBoard(cfg *Config, verb, id string) error {
	body, _ := json.Marshal(map[string]string{"id": id})
	resp, err := boardRequest(cfg, "POST", "/api/"+verb, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	var e struct {
		Error string `json:"error"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&e)
	if e.Error == "" {
		e.Error = fmt.Sprintf("the board said %s", resp.Status)
	}
	return errors.New(cleanText(e.Error, maxText))
}

// actElsewhere is seen, done or rm on another machine's lookout
// ("source:district/tower/name"): through the running board when one is up,
// which knows the source and routes it, or else straight through the
// source's act command from the config, as the board would.
func actElsewhere(verb, id string) error {
	// A broken config still names the board's port (a mistyped pattern
	// mustn't stop acting through a board that has a good one); only the
	// way round the board needs the sources.
	cfg, cerr := loadConfig()
	origin, orig, _ := strings.Cut(id, ":")
	if err := tellBoard(cfg, verb, id); !errors.Is(err, errNoBoard) {
		return err
	}
	if cerr != nil {
		return cerr
	}
	for _, s := range cfg.Sources {
		if s.Name == origin {
			parts := strings.Split(orig, "/")
			if len(parts) != 3 {
				return fmt.Errorf("no lookout %q: another machine's is source:district/tower/name", id)
			}
			for _, x := range parts {
				if err := checkName("name", x); err != nil {
					return err
				}
			}
			if !s.shows(parts[0], parts[1]) {
				return fmt.Errorf("no lookout %q", id) // hidden here, as on the board
			}
			return runAct(s, verb, orig)
		}
	}
	return fmt.Errorf("no source %q in %s", origin, cfg.path)
}
