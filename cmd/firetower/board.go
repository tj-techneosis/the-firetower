package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed ui
var uiFiles embed.FS

// cmdBoard serves the board on 127.0.0.1: the built-in page (or your own,
// with --ui), and the small API under /api that any page can use.
//
// Only this person should reach it, and only the board's own page should be
// able to press its buttons. So: it listens on loopback only; it answers only
// requests whose Host is 127.0.0.1 or localhost on its port (another site's
// name pointed at 127.0.0.1, DNS rebinding, gets nothing); everything under
// /api needs the key, which only this person can read and which a page on
// another origin can't get at; writes from a browser must come from its own
// origin; and its pages can't be framed, so nobody can lay them under a decoy
// and borrow a click.
func cmdBoard(args []string) error {
	f, err := parseFlags(args, map[string]bool{"port": true, "open": false, "ui": true, "url": false, "once": false, "start": false, "stop": false}, false)
	const u = "board [--port N] [--ui DIR] [--open | --start | --stop] | board --url [--once]"
	if err != nil {
		return bad(u, err)
	}
	if len(f.pos) != 0 {
		return badf(u, "board takes no words")
	}
	n := 0
	for _, k := range []string{"open", "start", "stop", "url"} {
		if f.has(k) {
			n++
		}
	}
	if n > 1 {
		return badf(u, "one of --open, --start, --stop or --url")
	}
	if f.has("once") && !f.has("url") {
		return badf(u, "--once goes with --url")
	}
	cfg, err := loadConfig()
	if err != nil && !f.has("stop") {
		return err
	}
	// --stop needs only the port: a broken config (a pattern mistyped while
	// changing hide) mustn't keep the old board from being stopped.
	port := cfg.Board.Port
	if f.has("port") {
		if port, err = strconv.Atoi(f.get("port")); err != nil || port <= 0 || port > 65535 {
			return fmt.Errorf("--port %q is not a port", f.get("port"))
		}
	}
	if f.has("stop") {
		return stopBoard(port)
	}
	key, err := boardKey(stateDir())
	if err != nil {
		return fmt.Errorf("can't make the board's key: %v", err)
	}
	if f.has("url") {
		if !f.has("once") {
			fmt.Println(boardURL(port, key))
			return nil
		}
		u, err := onceURL(port)
		if err != nil {
			return err
		}
		fmt.Println(u)
		return nil
	}
	// --open and --start never tie a board to whoever asked (a bar click, a
	// terminal): this person's board already up is used, or one is started
	// in the background, and this returns. Plain `board` is the board
	// itself, in the foreground, for a service or a terminal.
	if f.has("open") || f.has("start") {
		if !ourBoard(port, key) {
			if isBoard(port) {
				return fmt.Errorf("port %d has a board that didn't prove it's yours (another person's, or one from another state dir): give this one another with --port or board.port", port)
			}
			if err := startBoard(port, key, f); err != nil {
				return err
			}
		}
		if f.has("open") {
			// The window gets a one-time address, not the key: a command
			// line is readable by everyone on the machine while the window
			// is open, and the key would stay good forever.
			u, err := onceURL(port)
			if err != nil {
				return err
			}
			openWindow(u)
		}
		return nil
	}
	ui := cfg.Board.UI
	if f.has("ui") {
		ui = f.get("ui")
	}
	var pages fs.FS
	if ui != "" {
		ui = expandHome(ui)
		st, err := os.Stat(filepath.Join(ui, "index.html"))
		if err != nil || !st.Mode().IsRegular() {
			return fmt.Errorf("--ui %s: no index.html there", ui)
		}
		pages = os.DirFS(ui)
	} else {
		pages, _ = fs.Sub(uiFiles, "ui")
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		if ourBoard(port, key) {
			fmt.Printf("The board is already up (firetower board --open opens it; --url gives its address with the key)\n")
			return nil
		}
		if isBoard(port) {
			return fmt.Errorf("port %d has a board that didn't prove it's yours (another person's, or one from another state dir): try --port", port)
		}
		return fmt.Errorf("can't listen on %s: %v (another program has the port; try --port)", addr, err)
	}
	p := newPost(cfg)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go p.run(ctx)
	srv := &http.Server{Handler: boardHandler(p, pages, port, key), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		srv.Shutdown(sctx)
	}()
	fmt.Printf("The board is up at http://%s/ (firetower board --url gives the address with its key). It runs while this does: Ctrl-C stops it.\n", addr)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// isBoard: some Firetower board answers on the port, whoever's it is. Only
// ourBoard says it's this person's.
func isBoard(port int) bool {
	c := http.Client{Timeout: 500 * time.Millisecond}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/api/state", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.Header.Get("X-Firetower-Board") == "1"
}

// boardKey is the board's key: a secret in the private state dir, made the
// first time (under the dir's lock, written whole, so two processes making it
// at once agree on one). Loopback answers anyone on the machine, so on a
// machine with more than one person, a port alone would show this person's
// board (and the files its lookouts link to) to all of them. The key is what
// only this person can read.
func boardKey(dir string) (string, error) {
	path := filepath.Join(dir, ".board-key")
	read := func() string {
		b, err := os.ReadFile(path)
		if k := strings.TrimSpace(string(b)); err == nil && len(k) >= 32 {
			return k
		}
		return ""
	}
	if k := read(); k != "" {
		return k, nil
	}
	unlock, err := lockDir(dir)
	if err != nil {
		return "", err
	}
	defer unlock()
	if k := read(); k != "" {
		return k, nil
	}
	b := make([]byte, 24)
	rand.Read(b)
	key := hex.EncodeToString(b)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(key+"\n"), 0o600); err != nil {
		return "", err
	}
	return key, os.Rename(tmp, path)
}

// newBoardKey replaces the board's key with a fresh one.
func newBoardKey(dir string) (string, error) {
	unlock, err := lockDir(dir)
	if err != nil {
		return "", err
	}
	defer unlock()
	b := make([]byte, 24)
	rand.Read(b)
	key := hex.EncodeToString(b)
	path := filepath.Join(dir, ".board-key")
	if err := os.WriteFile(path+".tmp", []byte(key+"\n"), 0o600); err != nil {
		return "", err
	}
	return key, os.Rename(path+".tmp", path)
}

// boardURL is the address that lets a browser in. The page keeps the key in
// its own origin's storage (not a cookie: a browser sends 127.0.0.1's cookies
// to every port on it, so any other local server would get them) and drops it
// from the address bar.
func boardURL(port int, key string) string {
	return fmt.Sprintf("http://127.0.0.1:%d/?key=%s", port, key)
}

func boardHandler(p *post, pages fs.FS, port int, firstKey string) http.Handler {
	// The key can change while the board runs: when a one-time address is
	// used a second time (someone else read it and got in first), the board
	// makes a new key, so what they took stops working.
	var keyMu sync.RWMutex
	key := firstKey
	current := func() string { keyMu.RLock(); defer keyMu.RUnlock(); return key }
	var claimMu sync.Mutex
	hosts := map[string]bool{fmt.Sprintf("127.0.0.1:%d", port): true, fmt.Sprintf("localhost:%d", port): true}
	origins := map[string]bool{fmt.Sprintf("http://127.0.0.1:%d", port): true, fmt.Sprintf("http://localhost:%d", port): true}
	matches := func(v string) bool { return v != "" && subtle.ConstantTimeCompare([]byte(v), []byte(current())) == 1 }

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, p.state())
	})
	mux.HandleFunc("GET /api/feed", func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no streaming", 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		ch, done := p.subscribe()
		defer done()
		given := r.URL.Query().Get("key")
		if g, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			given = g
		}
		send := func() bool {
			if !matches(given) {
				return false // the key changed: this stream ends, and needs the new one
			}
			b, _ := json.Marshal(p.state())
			_, err := fmt.Fprintf(w, "event: state\ndata: %s\n\n", b)
			fl.Flush()
			return err == nil
		}
		if !send() {
			return
		}
		// A heartbeat, so "checked 5m ago" and out-of-contact keep moving
		// even when nothing is written.
		tick := time.NewTicker(15 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ch:
				// Coalesce a burst of changes into one send.
				time.Sleep(100 * time.Millisecond)
				select {
				case <-ch:
				default:
				}
			case <-tick.C:
			}
			if !send() {
				return
			}
		}
	})
	// proof lets a client check that this is the board holding its key
	// before it sends the key: HMAC(key, its nonce). A stranger on the port
	// can't answer it, so the key never goes to one.
	mux.HandleFunc("GET /api/proof", func(w http.ResponseWriter, r *http.Request) {
		nonce := r.URL.Query().Get("nonce")
		if len(nonce) != 32 || strings.Trim(nonce, "0123456789abcdef") != "" {
			http.Error(w, "want ?nonce= (32 hex)", http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]string{"proof": proofOf(current(), port, nonce)})
	})
	mux.HandleFunc("GET /api/whoami", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"pid": os.Getpid(), "port": port, "background": os.Getenv("FIRETOWER_BOARD_BACKGROUND") == "1"})
	})
	// claim trades a one-time address's token (board --open, --url --once)
	// for the key, once, within a minute of its making.
	mux.HandleFunc("POST /api/claim", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Once string `json:"once"`
		}
		json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body)
		// One claim at a time, and a wrong one waits: nobody can hammer it.
		claimMu.Lock()
		defer claimMu.Unlock()
		ok, stolen := claimOnce(stateDir(), body.Once)
		if stolen {
			nk, err := newBoardKey(stateDir())
			if err == nil {
				keyMu.Lock()
				key = nk
				keyMu.Unlock()
				p.notify() // every open page reconnects, and finds it needs the key again
			}
			writeJSONStatus(w, http.StatusUnauthorized, map[string]string{"error": "Someone else used this address first, so the board has changed its key and what they got no longer works. Open it again: firetower board --open", "reason": "stolen"})
			return
		}
		if !ok {
			time.Sleep(100 * time.Millisecond)
			writeJSONStatus(w, http.StatusUnauthorized, map[string]string{"error": "This address was for one use within a minute, and it's spent or too old. Open it again: firetower board --open", "reason": "spent"})
			return
		}
		writeJSON(w, map[string]string{"key": current()})
	})
	for _, verb := range []string{"seen", "done", "rm", "expect"} {
		mux.HandleFunc("POST /api/"+verb, func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				ID string `json:"id"`
			}
			if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil || body.ID == "" {
				http.Error(w, `want {"id": "district/tower/name"}`, http.StatusBadRequest)
				return
			}
			if err := p.act(verb, body.ID); err != nil {
				writeJSONStatus(w, http.StatusConflict, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, map[string]bool{"ok": true})
		})
	}
	mux.HandleFunc("GET /api/link", func(w http.ResponseWriter, r *http.Request) {
		serveLink(w, p, r.URL.Query().Get("id"))
	})
	files := http.FileServerFS(pages)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		files.ServeHTTP(w, r)
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Firetower-Board", "1")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		if !hosts[r.Host] {
			http.Error(w, "the board answers only to 127.0.0.1 and localhost", http.StatusMisdirectedRequest)
			return
		}
		// The pages are only code; the data is all under /api, and every
		// /api request carries the key: as a Bearer header, or (for what a
		// browser opens without headers: the event stream, a log in a new
		// tab) as ?key=. No cookie, so there is no ambient credential for
		// another page to borrow, and nothing for another port to receive.
		if o := r.Header.Get("Origin"); o != "" && r.Method != http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/") && !origins[o] {
			http.Error(w, "not from the board's page", http.StatusForbidden)
			return
		}
		if r.URL.Path == "/api/proof" || r.URL.Path == "/api/claim" {
			mux.ServeHTTP(w, r) // they carry no key: proof gives nothing away, claim takes a one-time token
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			given, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if given == r.Header.Get("Authorization") {
				given = ""
			}
			if given == "" && r.Method == http.MethodGet && (r.URL.Path == "/api/feed" || r.URL.Path == "/api/link") {
				given = r.URL.Query().Get("key")
			}
			if !matches(given) {
				writeJSONStatus(w, http.StatusUnauthorized, map[string]string{"error": "the board needs its key (firetower board --url)"})
				return
			}
			if o := r.Header.Get("Origin"); o != "" && r.Method != http.MethodGet && !origins[o] {
				http.Error(w, "not from the board's page", http.StatusForbidden)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) { writeJSONStatus(w, http.StatusOK, v) }

func writeJSONStatus(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// serveLink shows a local lookout's link when it's a file (a log, a report):
// the last 256 KB, as plain text, never run and never rendered. Only the
// link a lookout on this machine names, so the board can't be asked for any
// other file.
func serveLink(w http.ResponseWriter, p *post, id string) {
	var link string
	found := false
	for _, v := range p.state().Lookouts {
		if v.ID == id && v.Origin == "" {
			link, found = v.Link, true
		}
	}
	if !found {
		http.Error(w, "no lookout "+id+" on this machine", http.StatusNotFound)
		return
	}
	path := localPath(link)
	if path == "" {
		http.Error(w, "its link isn't a file", http.StatusNotFound)
		return
	}
	// Non-blocking, so a link to a FIFO can't hang the board; anything but
	// a plain file is turned away below.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		http.Error(w, "can't open "+path, http.StatusNotFound)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		http.Error(w, path+" isn't a plain file", http.StatusNotFound)
		return
	}
	const limit = 256 * 1024
	if st.Size() > limit {
		f.Seek(st.Size()-limit, io.SeekStart)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Content-Disposition", "inline")
	if st.Size() > limit {
		fmt.Fprintf(w, "[the last 256 KB of %s]\n\n", path)
	}
	io.Copy(w, io.LimitReader(f, limit))
}

// localPath is the file a link names (an absolute path, ~/..., or file://),
// or "" when it names something else.
func localPath(link string) string {
	switch {
	case strings.HasPrefix(link, "file://"):
		u, err := url.Parse(link)
		if err != nil || (u.Host != "" && u.Host != "localhost") {
			return ""
		}
		return filepath.Clean(u.Path)
	case strings.HasPrefix(link, "~/"):
		return filepath.Clean(expandHome(link))
	case strings.HasPrefix(link, "/"):
		return filepath.Clean(link)
	}
	return ""
}

// startBoard starts `firetower board` in the background, in a session of
// its own so it outlives whatever asked, with its output added to the state
// dir's board.log, and waits (5s at most) until it answers as ours.
func startBoard(port int, key string, f flags) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"board", "--port", strconv.Itoa(port)}
	if f.has("ui") {
		args = append(args, "--ui", f.get("ui"))
	}
	if err := os.MkdirAll(stateDir(), 0o700); err != nil {
		return err
	}
	logPath := filepath.Join(stateDir(), "board.log")
	// Appended, never truncated or moved: two starts at once (a double
	// click) mustn't cut or misplace the log the winner is writing. A board
	// writes a line or two when it starts, so it stays small.
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	from, _ := log.Seek(0, io.SeekEnd) // what this start writes begins here
	cmd := exec.Command(exe, args...)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.Env = append(os.Environ(), "FIRETOWER_BOARD_BACKGROUND=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("can't start the board: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if proveBoard(port, key, proofWait) {
			cmd.Process.Release()
			return nil
		}
		select {
		case err := <-exited:
			// It may have lost a race to another start that's up now.
			if proveBoard(port, key, proofWait) {
				return nil
			}
			return fmt.Errorf("the board didn't start: %s", logTail(logPath, from, err))
		case <-time.After(50 * time.Millisecond):
		}
	}
	return fmt.Errorf("the board didn't answer within 5s; it may still be starting (pid %d, its output in %s)", cmd.Process.Pid, logPath)
}

// logTail is the last line this start's board wrote (from where its log
// began), for saying why it didn't start; never an earlier run's line.
func logTail(path string, from int64, err error) string {
	b, _ := os.ReadFile(path)
	if from < int64(len(b)) {
		b = b[from:]
	} else {
		b = nil
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > 0 && lines[len(lines)-1] != "" {
		return cleanText(lines[len(lines)-1], maxText)
	}
	return fmt.Sprint(err)
}

// stopBoard stops this person's board on the port: it asks the board, once
// it has proved it holds the key, for its own pid. So nothing is stopped on
// a stale note's word, and a stranger's board is never touched. It waits 5s
// at most: a board that takes longer to stop is stuck, and it says so.
func stopBoard(port int) error {
	key := savedBoardKey(stateDir())
	if key == "" || !ourBoard(port, key) {
		if isBoard(port) {
			return fmt.Errorf("the board on port %d didn't prove it's yours (another person's, or one from another state dir), so it can't be stopped from here", port)
		}
		fmt.Println("No board is running.")
		return nil
	}
	cfg := &Config{Board: Board{Port: port}}
	resp, err := boardRequest(cfg, "GET", "/api/whoami", nil)
	if errors.Is(err, errNoBoard) {
		fmt.Println("No board is running.") // it went while we asked
		return nil
	}
	if err != nil {
		return fmt.Errorf("the board didn't say who it is: %v", err)
	}
	var who struct {
		Pid        int  `json:"pid"`
		Background bool `json:"background"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&who)
	resp.Body.Close()
	if who.Pid <= 1 {
		return fmt.Errorf("the board gave no pid it can be stopped by")
	}
	if err := syscall.Kill(who.Pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("can't stop the board (pid %d): %v", who.Pid, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		// Gone, or no longer answering as ours (a board run by hand stays
		// in the process table until whatever started it collects it).
		if syscall.Kill(who.Pid, 0) != nil || !ourBoard(port, key) {
			if who.Background {
				fmt.Println("Stopped the board.")
			} else {
				fmt.Println("Stopped the board. It wasn't started by --open or --start: if a service runs it (launchd, systemd), the service may start it again; stop the service instead.")
			}
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("the board (pid %d) didn't stop within 5s", who.Pid)
}

// openWindow opens the board as an app window (a browser window with no
// tabs or toolbar) where a Chromium-family browser is installed, else in the
// default browser.
func openWindow(u string) {
	// $FIRETOWER_OPEN_WITH names a command that opens the address instead
	// (a desktop's own web-app launcher, say).
	if with := strings.Fields(os.Getenv("FIRETOWER_OPEN_WITH")); len(with) > 0 {
		c := exec.Command(with[0], append(with[1:], u)...)
		if c.Start() == nil {
			go c.Wait()
			return
		}
	}
	var tries [][]string
	switch runtime.GOOS {
	case "darwin":
		for _, app := range []string{"Google Chrome", "Microsoft Edge", "Brave Browser", "Chromium", "Arc"} {
			if _, err := os.Stat("/Applications/" + app + ".app"); err == nil {
				tries = append(tries, []string{"open", "-na", app, "--args", "--app=" + u})
			}
		}
		tries = append(tries, []string{"open", u})
	default:
		for _, b := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "brave", "microsoft-edge"} {
			if path, err := exec.LookPath(b); err == nil {
				tries = append(tries, []string{path, "--app=" + u})
			}
		}
		tries = append(tries, []string{"xdg-open", u})
	}
	for _, t := range tries {
		c := exec.Command(t[0], t[1:]...)
		if c.Start() == nil {
			go c.Wait()
			return
		}
	}
	fmt.Fprintf(os.Stderr, "firetower: couldn't open a browser; the board is at %s\n", u)
}
