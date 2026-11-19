package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Passing on. A board that reads other machines can pass on what it shows:
// `firetower watch --json --pass-on` is the running board's picture (its own
// lookouts and its sources', after its hide and keep), as a feed. The
// reader asks for it; a plain `watch --json` is the machine's own lookouts,
// raw, as it always was.
//
// A lookout passed on keeps the word its machine goes by on the board that
// passes it ("origin"), and its id there ("work-mac:d/t/n"), so the reader's
// id for it ("hub:work-mac:d/t/n") is the way back: seen and done go to the
// board that passed it on, which sends them on to the machine, each board on
// the way holding them to its own rules and writing them down.
//
// Loops: every Firetower has an id (.firetower-id, random). A pass-on feed
// says whose it is and the id of every machine it carries, and every
// lookout and source on it says its machine's id where known. A reader
// drops what it carries of the reader itself, and a lookout goes through at
// most two boards (its own machine's, then the one passing it on): a board
// passes on its own lookouts and its sources' own, never what was passed on
// to it.

// maxHops: a lookout reaches a reader through at most this many boards.
const maxHops = 2

var boardIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// boardID is this Firetower's id, made the first time it's asked for when
// create is set. It is not a secret: it only tells two Firetowers apart.
func boardID(dir string, create bool) string {
	path := filepath.Join(dir, ".firetower-id")
	if b, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(b)); boardIDRE.MatchString(id) {
			return id
		}
	}
	if !create {
		return ""
	}
	unlock, err := lockDir(dir)
	if err != nil {
		return ""
	}
	defer unlock()
	if b, err := os.ReadFile(path); err == nil { // made while we waited
		if id := strings.TrimSpace(string(b)); boardIDRE.MatchString(id) {
			return id
		}
	}
	r := make([]byte, 16)
	rand.Read(r)
	id := hex.EncodeToString(r)
	if os.WriteFile(path+".tmp", []byte(id+"\n"), 0o600) != nil || os.Rename(path+".tmp", path) != nil {
		return ""
	}
	return id
}

// passOnFeed is `watch --json --pass-on`: the running board's picture, read
// over loopback once a second on the one connection it proved itself on.
// With no board of this person's up there is nothing to pass on, and it
// says so (the reader shows this machine out of contact with that reason):
// it never dials the sources itself, so every reader shares the board's one
// stream per source.
func passOnFeed(once bool) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	dir := stateDir()
	self := boardID(dir, true)
	key := savedBoardKey(dir)
	c := pinned(cfg.Board.Port, boardAsk)
	pc := *c
	pc.Timeout = proofWait
	if !proveOn(&pc, cfg.Board.Port, key) {
		return fmt.Errorf("--pass-on passes on what this machine's board shows, and no board of this person's is running here: start one (firetower board --start)")
	}
	take := func() (feedSnap, error) {
		req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/api/state", cfg.Board.Port), nil)
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := c.Do(req)
		if err != nil {
			return feedSnap{}, fmt.Errorf("the board stopped answering: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return feedSnap{}, fmt.Errorf("the board said %s", resp.Status)
		}
		var st State
		if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&st); err != nil {
			return feedSnap{}, fmt.Errorf("the board's answer didn't read: %v", err)
		}
		return passOnSnap(st, self), nil
	}
	first, err := take()
	if err != nil {
		return err
	}
	hello := Event{Type: "hello", V: feedVersion, Version: versionString(), PassOn: true, FiretowerID: self, Path: first.path}
	return runFeed(hello, func(time.Time) (feedSnap, error) {
		if first.views != nil {
			s := first
			first = feedSnap{}
			return s, nil
		}
		return take()
	}, true, once)
}

// passOnSnap is what a board passes on from its picture: its own lookouts
// (marked with its id) and its sources' own, never one passed on to it (that
// would be a third hop); its sources' contact, the same way.
func passOnSnap(st State, self string) feedSnap {
	s := feedSnap{views: []View{}, sources: []SourceStatus{}, path: []string{self}}
	for _, v := range st.Lookouts {
		if len(v.Via) > 0 {
			continue
		}
		if v.Origin == "" {
			v.OriginID = self
		}
		s.views = append(s.views, v)
	}
	for _, src := range st.Sources {
		if len(src.Via) > 0 {
			continue
		}
		src.Loop = ""
		s.sources = append(s.sources, src)
		if src.FiretowerID != "" {
			s.path = append(s.path, src.FiretowerID)
		}
	}
	return s
}

// passedID reads the id a pass-on feed gives a lookout it passes on from
// one of its sources: "origin:district/tower/name", every part a name.
func passedID(id string) (origin, district, tower, name string, ok bool) {
	origin, rest, found := strings.Cut(id, ":")
	if !found || checkName("source", origin) != nil {
		return "", "", "", "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		return "", "", "", "", false
	}
	for _, x := range parts {
		if checkName("name", x) != nil {
			return "", "", "", "", false
		}
	}
	return origin, parts[0], parts[1], parts[2], true
}

// checkActID holds an id that goes to another machine's act command to the
// name alphabet: district/tower/name, or one passed on there,
// origin:district/tower/name. It reaches a shell there, so it can only ever
// be one plain argument.
func checkActID(id string) error {
	if strings.Contains(id, ":") {
		if _, _, _, _, ok := passedID(id); !ok {
			return fmt.Errorf("no lookout %q", id)
		}
		return nil
	}
	parts := strings.Split(id, "/")
	if len(parts) != 3 {
		return fmt.Errorf("no lookout %q", id)
	}
	for _, x := range parts {
		if err := checkName("name", x); err != nil {
			return err
		}
	}
	return nil
}

// fromRE is what --from carries along a chain: who asked, and the id of the
// board that sent it on ("someone@<id>"). Plain characters only: it goes
// through a shell on the next machine.
var fromRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}@[0-9a-f]{32}$`)

// whoAmI is this account's name, held to the name alphabet, for the log.
func whoAmI() string {
	n := os.Getenv("USER")
	if u, err := user.Current(); err == nil && u.Username != "" {
		n = u.Username
	}
	var b strings.Builder
	for _, r := range strings.ToLower(n) {
		if strings.ContainsRune(nameChars, r) && b.Len() < maxName {
			b.WriteRune(r)
		}
	}
	if s := b.String(); s != "" && checkName("name", s) == nil {
		return s
	}
	return "someone"
}

// forwardFrom is the --from a hop sends on: the first asker's name, and
// this board's id.
func forwardFrom(from, self string) string {
	who := whoAmI()
	if w, _, ok := strings.Cut(from, "@"); ok && from != "" {
		who = w
	}
	if self == "" {
		return ""
	}
	return who + "@" + self
}

// logAct writes down an action routed through a chain, on every board it
// passes: when, what, on which lookout, who asked and from which board, and
// where it went. acts.log in the state dir; a log that can't be written
// never stops the action.
func logAct(dir, verb, id, from, where string, err error) {
	who, by := whoAmI(), "this machine"
	if w, b, ok := strings.Cut(from, "@"); ok {
		who, by = w, "board "+b
	}
	result := "ok"
	if err != nil {
		result = "failed: " + cleanText(err.Error(), maxText)
	}
	line := fmt.Sprintf("%s %s %s by %s from %s, %s: %s\n", time.Now().UTC().Format(time.RFC3339), verb, id, who, by, where, result)
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	f, ferr := os.OpenFile(filepath.Join(dir, "acts.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if ferr != nil {
		return
	}
	defer f.Close()
	f.WriteString(line)
}
