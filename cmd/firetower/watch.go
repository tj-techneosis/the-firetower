package main

import (
	"bufio"
	"encoding/json"
	"os"
	"time"
)

// The feed: what every board reads, locally or over ssh. One JSON object per
// line:
//
//	{"type":"hello","v":1,"version":"firetower v0.1.0 (darwin/arm64)"}
//	{"type":"lookout","lookout":{...a View...}}   every lookout, then each change
//	{"type":"gone","id":"district/tower/name"}    a lookout left
//	{"type":"sync","at":"...","ids":[...]}         the full set, after every full send
//
// It sends everything, then a sync; then a line for every change, including
// the ones nobody writes (a lookout past its next_by going to no word, one
// past its until standing down); and every 60s everything again and a sync, so a reader knows the feed is alive and
// can drop anything it missed the gone of.
//
// `watch --json --pass-on` is the running board's picture instead (see
// passon.go): the hello says pass_on, firetower_id and path, a sources line
// comes before every full send and on each change, and a lookout passed on
// from a source carries origin and origin_id. A plain feed has none of it.
type Event struct {
	Type    string     `json:"type"`
	V       int        `json:"v,omitempty"`
	Version string     `json:"version,omitempty"`
	At      *time.Time `json:"at,omitempty"`
	Lookout *View      `json:"lookout,omitempty"`
	ID      string     `json:"id,omitempty"`
	IDs     *[]string  `json:"ids,omitempty"` // always there on a sync, even empty

	// A pass-on feed's (watch --json --pass-on) only: a plain feed's lines
	// never carry them.
	PassOn      bool            `json:"pass_on,omitempty"`      // hello: this feed passes on a board's picture
	FiretowerID string          `json:"firetower_id,omitempty"` // hello: whose board it is
	Path        []string        `json:"path,omitempty"`         // hello, sources: the id of every machine it carries
	Sources     *[]SourceStatus `json:"sources,omitempty"`      // sources: the board's sources and their contact
}

const (
	feedVersion = 1
	resendEvery = 60 * time.Second
	pollEvery   = time.Second
)

func cmdWatch(args []string) error {
	f, err := parseFlags(args, map[string]bool{"json": false, "once": false, "pass-on": false}, false)
	const u = "watch --json [--pass-on] [--once]"
	if err != nil {
		return bad(u, err)
	}
	if len(f.pos) != 0 || !f.has("json") {
		return badf(u, "the feed is JSON lines, for boards: say --json (firetower ls is for people)")
	}
	if f.has("pass-on") {
		return passOnFeed(f.has("once"))
	}
	dir := stateDir()
	return runFeed(Event{Type: "hello", V: feedVersion, Version: versionString()}, func(at time.Time) (feedSnap, error) {
		return feedSnap{views: localViews(dir, at, false)}, nil
	}, false, f.has("once"))
}

// feedSnap is what a feed sends at a moment: the lookouts, and on a pass-on
// feed the board's sources and the machines it carries.
type feedSnap struct {
	views   []View
	sources []SourceStatus
	path    []string
}

func runFeed(hello Event, take func(time.Time) (feedSnap, error), passOn, once bool) error {
	out := bufio.NewWriter(os.Stdout)
	enc := json.NewEncoder(out)
	return feed(hello, take, passOn, once, func(e Event) error { return enc.Encode(e) }, out.Flush, nil)
}

// feed runs the feed until a write fails (the reader went away), stop
// closes, or take fails (a pass-on feed's board went away: the error is
// why).
func feed(hello Event, take func(time.Time) (feedSnap, error), passOn, once bool, emit func(Event) error, flush func() error, stop <-chan struct{}) error {
	if err := emit(hello); err != nil {
		return nil
	}
	last := map[string]string{}
	lastSources := ""
	sources := func(s feedSnap, always bool) error {
		if !passOn {
			return nil
		}
		b, _ := json.Marshal([]any{s.sources, s.path})
		if !always && string(b) == lastSources {
			return nil
		}
		lastSources = string(b)
		return emit(Event{Type: "sources", Sources: &s.sources, Path: s.path})
	}
	var takeErr error
	full := func() error {
		at := now()
		s, err := take(at)
		if err != nil {
			takeErr = err
			return err
		}
		if err := sources(s, true); err != nil {
			return err
		}
		vs := s.views
		ids := make([]string, 0, len(vs))
		next := map[string]string{}
		for i := range vs {
			v := vs[i]
			b, _ := json.Marshal(v)
			next[v.ID] = string(b)
			ids = append(ids, v.ID)
			if err := emit(Event{Type: "lookout", Lookout: &v}); err != nil {
				return err
			}
		}
		last = next
		if err := emit(Event{Type: "sync", At: &at, IDs: &ids}); err != nil {
			return err
		}
		return flush()
	}
	if err := full(); err != nil || once {
		return takeErr
	}
	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	lastFull := time.Now()
	for {
		select {
		case <-stop:
			return nil
		case <-tick.C:
		}
		if time.Since(lastFull) >= resendEvery {
			if full() != nil {
				return takeErr
			}
			lastFull = time.Now()
			continue
		}
		s, err := take(now())
		if err != nil {
			return err
		}
		if sources(s, false) != nil {
			return nil
		}
		vs := s.views
		next := map[string]string{}
		for i := range vs {
			v := vs[i]
			b, _ := json.Marshal(v)
			str := string(b)
			next[v.ID] = str
			if last[v.ID] != str {
				if emit(Event{Type: "lookout", Lookout: &v}) != nil {
					return nil
				}
			}
		}
		for id := range last {
			if _, ok := next[id]; !ok {
				if emit(Event{Type: "gone", ID: id}) != nil {
					return nil
				}
			}
		}
		last = next
		if flush() != nil {
			return nil
		}
	}
}
