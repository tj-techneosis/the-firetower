package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The lookout post: what both boards read. It holds this machine's lookouts
// (straight from the files) and every source's (from its feed), and knows
// which sources are out of contact. A source's lookouts keep their own ids
// behind the source's name ("work-mac:website/checkout/deploy"), so two
// machines can't collide.

// State is what a board shows at a moment: /api/state, and `bar --format json`'s input.
type State struct {
	At       time.Time      `json:"at"`
	Lookouts []View         `json:"lookouts"`
	Sources  []SourceStatus `json:"sources"`
}

type SourceStatus struct {
	Name     string     `json:"name"`
	District string     `json:"district,omitempty"`
	Contact  string     `json:"contact"` // connecting | ok | lost
	Since    *time.Time `json:"since,omitempty"`
	Error    string     `json:"error,omitempty"`
	CanAct   bool       `json:"can_act"`
}

// A source goes out of contact when its feed is quiet this long (the feed
// re-sends every 60s), or as soon as its command ends.
const quietLimit = 2 * time.Minute

type post struct {
	cfg     *Config
	dir     string
	mu      sync.Mutex
	local   []View
	sources []*remote
	subs    map[chan struct{}]bool
}

type remote struct {
	src       Source
	views     map[string]View // by the source's own id
	status    SourceStatus
	last      time.Time
	lostSince *time.Time
	okSince   time.Time
}

func newPost(cfg *Config) *post {
	p := &post{cfg: cfg, dir: stateDir(), subs: map[chan struct{}]bool{}}
	for _, s := range cfg.Sources {
		p.sources = append(p.sources, &remote{src: s, views: map[string]View{},
			status: SourceStatus{Name: s.Name, District: s.District, Contact: "connecting", CanAct: len(s.Act) > 0}})
	}
	return p
}

// run keeps the post up to date until ctx ends: the local files every
// second, and each source's feed, reconnecting with a backoff.
func (p *post) run(ctx context.Context) {
	for _, r := range p.sources {
		go p.follow(ctx, r, false)
	}
	p.refreshLocal()
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.refreshLocal()
			p.checkQuiet()
		}
	}
}

// once fills the post a single time and returns: the local files, and each
// source until its first sync (or timeout). For `bar`, which prints and goes.
func (p *post) once(timeout time.Duration) {
	p.refreshLocal()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var wg sync.WaitGroup
	for _, r := range p.sources {
		wg.Add(1)
		go func(r *remote) { defer wg.Done(); p.follow(ctx, r, true) }(r)
	}
	wg.Wait()
	p.mu.Lock()
	for _, r := range p.sources {
		if r.status.Contact == "connecting" {
			p.lose(r, "no answer in "+timeout.String())
		}
	}
	p.mu.Unlock()
}

func (p *post) refreshLocal() {
	if !p.cfg.showLocal() {
		return
	}
	vs := localViews(p.dir, now(), false)
	shown := vs[:0]
	for _, v := range vs {
		if p.cfg.Board.shows(v.District, v.Tower, v.Name, v.Tags) {
			shown = append(shown, v)
		}
	}
	vs = shown
	p.mu.Lock()
	changed := !sameViews(p.local, vs)
	p.local = vs
	p.mu.Unlock()
	if changed {
		p.notify()
	}
}

func sameViews(a, b []View) bool {
	if len(a) != len(b) {
		return false
	}
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func (p *post) checkQuiet() {
	p.mu.Lock()
	changed := false
	for _, r := range p.sources {
		if r.status.Contact == "ok" && time.Since(r.last) > quietLimit {
			p.lose(r, fmt.Sprintf("its feed has been quiet for %s", quietLimit))
			changed = true
		}
	}
	p.mu.Unlock()
	if changed {
		p.notify()
	}
}

// lose marks a source out of contact (the lock is held). Its lookouts stay,
// marked, so the board shows what it last knew rather than nothing.
func (p *post) lose(r *remote, why string) {
	// A machine that answers for a moment and drops again (a flapping
	// link) has been out of contact since it first went, not since the
	// last drop.
	if r.status.Contact != "lost" && (r.lostSince == nil || time.Since(r.okSince) > quietLimit) {
		r.lostSince = tp(now())
	}
	r.status.Since = r.lostSince
	r.status.Contact, r.status.Error = "lost", cleanText(why, maxText)
}

// follow runs a source's command and reads its feed, again and again with a
// backoff, until ctx ends (or, with first, until its first sync).
func (p *post) follow(ctx context.Context, r *remote, first bool) {
	backoff := 2 * time.Second
	for ctx.Err() == nil {
		synced, why := p.readFeed(ctx, r, first)
		if first && synced {
			return
		}
		p.mu.Lock()
		p.lose(r, why)
		p.mu.Unlock()
		p.notify()
		if first {
			return
		}
		if synced {
			backoff = 2 * time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

// readFeed runs the command once. It reports whether a sync arrived and,
// when it ends, why.
func (p *post) readFeed(ctx context.Context, r *remote, first bool) (bool, string) {
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(cctx, r.src.Command[0], r.src.Command[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 3 * time.Second
	out, err := cmd.StdoutPipe()
	if err != nil {
		return false, err.Error()
	}
	errTail := &lastLines{}
	cmd.Stderr = errTail
	if err := cmd.Start(); err != nil {
		return false, "can't start: " + strings.TrimPrefix(err.Error(), "fork/exec ")
	}
	// Quiet past the limit: stop waiting on it, so it reconnects.
	quiet := time.AfterFunc(quietLimit, cancel)
	defer quiet.Stop()
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	synced := false
	for sc.Scan() {
		quiet.Reset(quietLimit)
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue // a login banner, an ssh warning: not the feed's
		}
		if p.apply(r, e) {
			synced = true
			if first {
				cancel()
				break
			}
		}
	}
	io.Copy(io.Discard, out)
	werr := cmd.Wait()
	if first && synced {
		return true, ""
	}
	why := "it stopped answering (the command that reads its feed ended)"
	if msg, _ := errTail.result(); msg != "" {
		why = msg
	} else if werr != nil {
		why = "it stopped answering (the command that reads its feed failed: " + werr.Error() + ")"
	}
	if ctx.Err() == nil && cctx.Err() != nil {
		why = fmt.Sprintf("its feed was quiet for %s", quietLimit)
	}
	return synced, why
}

// apply takes one line of a source's feed. It returns true on a sync.
func (p *post) apply(r *remote, e Event) bool {
	p.mu.Lock()
	r.last = time.Now()
	sync := false
	switch e.Type {
	case "lookout":
		if e.Lookout == nil || e.Lookout.ID == "" {
			break
		}
		v := *e.Lookout
		// The id is made of the names, as every watch feed makes it; one
		// that says otherwise could be filtered by one name and acted on
		// by another, so it's dropped.
		if v.ID != v.District+"/"+v.Tower+"/"+v.Name {
			break
		}
		// A lookout this board doesn't show is dropped as it comes in, so
		// nothing (the boards, the feed, the counts, act) ever sees it.
		// Matched on the names the board will show (a name that isn't one
		// shows as "unnamed") and its cleaned tags, before any folding.
		if d, t := placeNames(v); r.src.shows(d, t, nameOrUnnamed(v.Name), cleanTags(v.Tags)) {
			r.views[v.ID] = v
		} else {
			delete(r.views, v.ID)
		}
	case "gone":
		delete(r.views, e.ID)
	case "sync":
		if e.IDs != nil {
			keep := map[string]bool{}
			for _, id := range *e.IDs {
				keep[id] = true
			}
			for id := range r.views {
				if !keep[id] {
					delete(r.views, id)
				}
			}
		}
		if r.status.Contact != "ok" {
			r.status.Since = tp(now())
			r.okSince = time.Now()
		}
		r.status.Contact, r.status.Error = "ok", ""
		sync = true
	}
	p.mu.Unlock()
	if e.Type != "hello" {
		p.notify()
	}
	return sync
}

// state is the post's picture now. A source's lookouts come back renamed
// and placed: its name in front of the id, its district over theirs when it
// folds them into one (their own kept as a tag), and "lost" while it is out
// of contact. Their text is cleaned again: another machine's feed is
// another program's output.
func (p *post) state() State {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := State{At: now(), Lookouts: append([]View{}, p.local...), Sources: []SourceStatus{}}
	for _, r := range p.sources {
		st.Sources = append(st.Sources, r.status)
		for _, v := range r.views {
			st.Lookouts = append(st.Lookouts, placeRemote(r, v))
		}
	}
	sortViews(st.Lookouts)
	return st
}

// cleanTags keeps a feed's tags that are tags (names), at most maxTags,
// sorted, so a board groups another machine's lookouts as safely as its own.
func cleanTags(in []string) tagList {
	var out tagList
	seen := map[string]bool{}
	for _, t := range in {
		if checkName("tag", t) == nil && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	// Sorted, then capped, so which ones are kept never hangs on a feed's
	// order.
	sort.Strings(out)
	if len(out) > maxTags {
		out = out[:maxTags]
	}
	return out
}

// nameOrUnnamed is a feed lookout's name as hide and keep match it: a name
// that isn't one is "unnamed", as placeNames does for its places.
func nameOrUnnamed(n string) string {
	if checkName("name", n) != nil {
		return "unnamed"
	}
	return n
}

// placeNames is a feed lookout's district and tower as a board shows them:
// a name that isn't one is "unnamed".
func placeNames(v View) (district, tower string) {
	district, tower = v.District, v.Tower
	if checkName("district", district) != nil {
		district = "unnamed"
	}
	if checkName("tower", tower) != nil {
		tower = "unnamed"
	}
	return district, tower
}

func placeRemote(r *remote, v View) View {
	v.Origin = r.src.Name
	v.ID = r.src.Name + ":" + cleanText(v.ID, 200)
	v.Name = cleanText(v.Name, 64)
	v.HomeDistrict = ""
	v.Tags = cleanTags(v.Tags)
	// What a board works out is its own to say: a feed's word for it is
	// taken only when it's one of the words.
	v.Contact = ""
	if v.Movement != "moved" && v.Movement != "still" {
		v.Movement = ""
	}
	if v.DoneBy != "report" && v.DoneBy != "until" {
		v.DoneBy = ""
	}
	if !knownStates[v.State] {
		v.State = "stale" // a word this board doesn't know: shown as needing a look, never as clear
	}
	v.Label = cleanText(v.Label, 80)
	v.Message = cleanText(v.Message, maxText)
	v.Value = cleanText(v.Value, maxText)
	v.Source = cleanText(v.Source, 120)
	v.Link = cleanText(v.Link, 2000)
	v.District, v.Tower = placeNames(v)
	if r.src.District != "" {
		v.HomeDistrict, v.District = v.District, r.src.District
	}
	if r.status.Contact == "lost" {
		v.Contact = "lost"
	}
	return v
}

var knownStates = map[string]bool{"green": true, "yellow": true, "red": true, "done": true, "stale": true, "waiting": true}

func (p *post) subscribe() (chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	p.mu.Lock()
	p.subs[ch] = true
	p.mu.Unlock()
	return ch, func() { p.mu.Lock(); delete(p.subs, ch); p.mu.Unlock() }
}

func (p *post) notify() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for ch := range p.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// act does what a board's buttons do: seen, done or rm, on this machine
// directly, or on a source's machine through its act command.
func (p *post) act(verb, id string) error {
	if verb != "seen" && verb != "done" && verb != "rm" && verb != "expect" {
		return fmt.Errorf("no action %q", verb)
	}
	origin, orig, remoteID := strings.Cut(id, ":")
	if !remoteID {
		parts := strings.Split(id, "/")
		if len(parts) != 3 {
			return fmt.Errorf("no lookout %q", id)
		}
		for _, x := range parts {
			if err := checkName("name", x); err != nil {
				return err
			}
		}
		// A lookout this board doesn't show can't be acted on from it,
		// here as for a source's.
		if !p.cfg.showLocal() {
			return fmt.Errorf("no lookout %q", id)
		}
		var tags []string
		if l, err := loadLookout(lookoutPath(p.dir, parts[0], parts[1], parts[2])); err == nil {
			tags = cleanTags(l.Tags)
		}
		if !p.cfg.Board.shows(parts[0], parts[1], parts[2], tags) {
			return fmt.Errorf("no lookout %q", id)
		}
		var err error
		switch verb {
		case "seen":
			_, err = seen(parts[0], parts[1], parts[2])
		case "done":
			err = done(parts[0], parts[1], parts[2], "")
		case "rm":
			err = rmLookout(parts[0], parts[1], parts[2])
		case "expect":
			// Watch again: book again one that's there, as `firetower
			// expect` does. A button never makes a new lookout.
			if _, serr := os.Stat(lookoutPath(stateDir(), parts[0], parts[1], parts[2])); serr != nil {
				return fmt.Errorf("no lookout %q", id)
			}
			err = cmdExpect([]string{id})
		}
		p.refreshLocal()
		return err
	}
	var r *remote
	for _, s := range p.sources {
		if s.src.Name == origin {
			r = s
		}
	}
	if r == nil {
		return fmt.Errorf("no source %q", origin)
	}
	if len(r.src.Act) == 0 {
		return fmt.Errorf("source %s has no act command, so its lookouts can only be looked at", origin)
	}
	p.mu.Lock()
	_, known := r.views[orig]
	lost := r.status.Contact == "lost"
	p.mu.Unlock()
	if !known {
		return fmt.Errorf("no lookout %q", id)
	}
	if lost {
		return fmt.Errorf("%s is out of contact, so it can't be reached; try again once it's back", origin)
	}
	return runAct(r.src, verb, orig)
}

// runAct runs a source's act command for one lookout (its own id there).
// The id goes to another machine's command line, usually through ssh and a
// shell there, so it is held to the name alphabet first and can only ever be
// one plain argument; and nothing else goes with it (no message).
func runAct(src Source, verb, orig string) error {
	if len(src.Act) == 0 {
		return fmt.Errorf("source %s has no act command, so its lookouts can only be looked at", src.Name)
	}
	parts := strings.Split(orig, "/")
	if len(parts) != 3 {
		return fmt.Errorf("no lookout %q", orig)
	}
	for _, x := range parts {
		if err := checkName("name", x); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	args := append(append([]string{}, src.Act[1:]...), verb, orig)
	out, err := exec.CommandContext(ctx, src.Act[0], args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s: %s", src.Name, cleanText(msg, maxText))
	}
	return nil
}

// Summary is what a bar shows: `bar --format json`, and the data a
// `--template` gets.
type Summary struct {
	At        time.Time      `json:"at"`
	Worst     string         `json:"worst"` // the worst state anywhere, as a state word
	Count     int            `json:"count"` // lookouts on watch (stood-down ones aren't)
	Counts    map[string]int `json:"counts"`
	Attention int            `json:"attention"` // fire, smoke and no word (not seen), and each machine out of contact
	Districts []DistrictSum  `json:"districts"`
	Sources   []SourceStatus `json:"sources"`
	Board     bool           `json:"board"`                // read from a running board (false: none answered, so the bar dialed every source itself)
	BoardNote string         `json:"board_note,omitempty"` // why this person's board, up, didn't answer
}

type DistrictSum struct {
	Name   string     `json:"name"`
	Worst  string     `json:"worst"`
	Towers []TowerSum `json:"towers"`
}

type TowerSum struct {
	Name     string `json:"name"`
	Worst    string `json:"worst"`
	Lookouts []View `json:"lookouts"`
}

func summarize(st State) Summary {
	s := Summary{At: st.At, Counts: map[string]int{}, Sources: st.Sources, Districts: []DistrictSum{}}
	worst := 0
	di := map[string]int{}
	for _, src := range st.Sources {
		if src.Contact == "lost" {
			s.Counts["lost"]++ // once per machine, however many lookouts it had
			s.Attention++
		}
	}
	for _, v := range st.Lookouts {
		b := bucket(v)
		if b != "lost" {
			s.Counts[b]++
		}
		if v.State != "done" {
			s.Count++
		}
		switch b {
		case "red", "yellow", "stale":
			s.Attention++
		}
		r := rank(v)
		worst = max(worst, r)
		i, ok := di[v.District]
		if !ok {
			i = len(s.Districts)
			di[v.District] = i
			s.Districts = append(s.Districts, DistrictSum{Name: v.District})
		}
		d := &s.Districts[i]
		if len(d.Towers) == 0 || d.Towers[len(d.Towers)-1].Name != v.Tower {
			d.Towers = append(d.Towers, TowerSum{Name: v.Tower})
		}
		t := &d.Towers[len(d.Towers)-1]
		t.Lookouts = append(t.Lookouts, v)
	}
	for _, src := range st.Sources {
		if src.Contact == "lost" {
			worst = max(worst, 4)
		}
	}
	for i := range s.Districts {
		dw := 0
		for j := range s.Districts[i].Towers {
			tw := 0
			for _, v := range s.Districts[i].Towers[j].Lookouts {
				tw = max(tw, rank(v))
			}
			s.Districts[i].Towers[j].Worst = rankName[tw]
			dw = max(dw, tw)
		}
		s.Districts[i].Worst = rankName[dw]
	}
	sort.SliceStable(s.Districts, func(i, j int) bool { return s.Districts[i].Name < s.Districts[j].Name })
	s.Worst = rankName[worst]
	if len(st.Lookouts) == 0 && worst == 0 {
		s.Worst = "none"
	}
	return s
}
