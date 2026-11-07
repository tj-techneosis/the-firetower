package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// reportFlags are the flags every report takes (set, run, expect).
var reportFlags = map[string]bool{
	"district": true, "tower": true, "label": true, "value": true, "link": true, "source": true,
	"next": true, "until": true, "tag": true,
}

func with(base map[string]bool, more ...string) map[string]bool {
	m := map[string]bool{}
	for k, v := range base {
		m[k] = v
	}
	for _, k := range more {
		takes := !strings.HasPrefix(k, "!")
		m[strings.TrimPrefix(k, "!")] = takes
	}
	return m
}

// where says where a NAME lives. A full path (district/tower/name, or
// tower/name in the default district), --district/--tower, or
// $FIRETOWER_DISTRICT/$FIRETOWER_TOWER place it exactly. A bare name with
// none of those is looked for: if exactly one lookout has it, that's the one;
// if none does, it goes to the defaults (or isn't found, when it must exist).
func where(arg string, f flags, mustExist bool) (district, tower, name string, err error) {
	cfg, cerr := loadConfig()
	dd, dt := cfg.DefaultDistrict, cfg.DefaultTower
	if v := os.Getenv("FIRETOWER_DISTRICT"); v != "" {
		dd = v
	}
	if v := os.Getenv("FIRETOWER_TOWER"); v != "" {
		dt = v
	}
	if cerr != nil {
		// Said every time, not only at a terminal: a scheduler's log is
		// where a broken config is found.
		switch {
		case mustExist:
			// A lookup looks a bare name up; it places nothing.
			fmt.Fprintf(os.Stderr, "firetower: %v (boards won't start until it's fixed)\n", cerr)
		case cfg.defaultsLost():
			fmt.Fprintf(os.Stderr, "firetower: %v (not all its defaults can be used: a bare name goes to %s/%s)\n", cerr, dd, dt)
		default:
			fmt.Fprintf(os.Stderr, "firetower: %v (reports still go to its defaults; boards won't start until it's fixed)\n", cerr)
		}
	}
	parts := strings.Split(arg, "/")
	for _, p := range parts {
		if p == "" {
			return "", "", "", fmt.Errorf("%q: a NAME is name, tower/name or district/tower/name, with no empty parts", arg)
		}
	}
	exact := f.has("district") || f.has("tower") || os.Getenv("FIRETOWER_DISTRICT") != "" || os.Getenv("FIRETOWER_TOWER") != ""
	switch len(parts) {
	case 1:
		name = parts[0]
	case 2:
		tower, name, exact = parts[0], parts[1], true
	case 3:
		district, tower, name, exact = parts[0], parts[1], parts[2], true
	default:
		return "", "", "", fmt.Errorf("%q: a NAME is name, tower/name or district/tower/name", arg)
	}
	if len(parts) > 1 && (f.has("district") || f.has("tower")) {
		return "", "", "", fmt.Errorf("%q already says where it is; leave out --district and --tower", arg)
	}
	if district == "" {
		district = dd
		if f.has("district") {
			district = f.get("district")
		}
	}
	if tower == "" {
		tower = dt
		if f.has("tower") {
			tower = f.get("tower")
		}
	}
	for _, c := range [][2]string{{"district", district}, {"tower", tower}, {"lookout name", name}} {
		if err := checkName(c[0], c[1]); err != nil {
			return "", "", "", err
		}
	}
	dir := stateDir()
	if exact {
		if mustExist {
			if _, err := os.Stat(lookoutPath(dir, district, tower, name)); err != nil {
				return "", "", "", fmt.Errorf("no lookout %s/%s/%s", district, tower, name)
			}
		}
		return district, tower, name, nil
	}
	var found []string
	all, bad := loadAll(dir)
	for _, l := range all {
		if l.Name == name {
			found = append(found, l.ID())
		}
	}
	for _, b := range bad {
		if b.Name == name {
			found = append(found, b.District+"/"+b.Tower+"/"+b.Name)
		}
	}
	switch len(found) {
	case 0:
		if mustExist {
			return "", "", "", fmt.Errorf("no lookout named %q (firetower ls shows them)", name)
		}
		return district, tower, name, nil
	case 1:
		p := strings.Split(found[0], "/")
		return p[0], p[1], p[2], nil
	}
	return "", "", "", fmt.Errorf("%q is in more than one tower (%s): say which, as district/tower/name", name, strings.Join(found, ", "))
}

// update is every write: take the lock, read the lookout (or start one),
// change it, write it back.
func update(district, tower, name string, create bool, change func(l *Lookout, at time.Time) error) (*Lookout, error) {
	dir := stateDir()
	unlock, err := lockDir(dir)
	if err != nil {
		return nil, fmt.Errorf("can't write to %s: %v", dir, err)
	}
	defer unlock()
	path := lookoutPath(dir, district, tower, name)
	created := false
	l, err := loadLookout(path)
	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist) && create:
		l = &Lookout{Name: name, District: district, Tower: tower}
		created = true
	case errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("no lookout %s/%s/%s", district, tower, name)
	default:
		if !create {
			id := district + "/" + tower + "/" + name
			return nil, fmt.Errorf("can't read the report for %s (the file is damaged); firetower rm %s removes it, and the next report replaces it", id, id)
		}
		// An unreadable file is replaced by the next report: that's the
		// fix, and the report is the truth now.
		l = &Lookout{Name: name, District: district, Tower: tower}
	}
	at := now()
	if err := change(l, at); err != nil {
		return nil, err
	}
	// Every write leaves the history tidy, whatever path it took.
	l.History = tidyMoves(l.History, at)
	if err := saveLookout(dir, l); err != nil {
		return nil, fmt.Errorf("can't write %s: %v", path, err)
	}
	maybeGC(dir, at, path)
	if created && atTerminal() {
		fmt.Fprintf(os.Stderr, "firetower: new lookout %s\n", l.ID())
	}
	return l, nil
}

// applyCommon applies the optional report flags. A flag left out keeps the
// field as it was.
//
// --next is the scheduler's promise of the next report: a span counts from
// this report, a time is that time. Readers show "no word" once it has
// passed. --until is when the watch ends. A span for it is anchored once (by
// expect, or the first report that gives one), because a booked "run ...
// --until 6h" repeats the flag on every run and the watch must still end six
// hours after it began. A time always applies. Neither starts a timer: they
// are two times written down, compared with now by whoever reads.
func applyCommon(l *Lookout, f flags, at time.Time, anchor bool) error {
	// A report arriving after its promise is the promise kept, late: it is
	// spent, and this report makes no new one unless it says --next.
	// (Expecting isn't a report: it leaves a lapsed promise showing.)
	if !anchor && !f.has("next") && l.NextBy != nil && at.After(*l.NextBy) {
		l.NextBy = nil
	}
	if v := f.str("next"); v != nil {
		t, _, err := parseWhen("next", *v, at)
		if err != nil {
			return err
		}
		if t != nil && !t.After(at) && !quiet && atTerminal() {
			fmt.Fprintf(os.Stderr, "firetower: --next %s is already past, so the board shows no word at once\n", *v)
		}
		l.NextBy = t
	}
	if v := f.str("until"); v != nil {
		t, relative, err := parseWhen("until", *v, at)
		if err != nil {
			return err
		}
		switch {
		case !relative || anchor || l.Until == nil:
			if t != nil && !t.After(at) && !quiet && atTerminal() {
				fmt.Fprintf(os.Stderr, "firetower: --until %s is already past, so the watch reads as over\n", *v)
			}
			l.Until = t
		case t != nil && !t.Equal(*l.Until) && !quiet && atTerminal():
			// A booked line repeats its span every run, and must stay
			// quiet; a person at a terminal trying to move the end is told
			// how.
			if l.Until.After(at) && t.Sub(*l.Until).Abs() > time.Minute {
				fmt.Fprintf(os.Stderr, "firetower: kept the end set when the watch was booked (%s); firetower expect %s --until %s moves it\n", l.Until.Local().Format("Jan 2 15:04"), l.ID(), *v)
			}
		}
	}
	if v := f.str("label"); v != nil {
		l.Label = cleanText(*v, 80)
	}
	if v := f.str("link"); v != nil {
		l.Link = cleanText(*v, 2000)
	}
	if v := f.str("source"); v != nil {
		l.Source = cleanText(*v, 120)
	}
	if f.has("tag") {
		tags, err := parseTags(f.list("tag"))
		if err != nil {
			return err
		}
		l.Tags = tags
	}
	return nil
}

// report records what a check saw: the heart of set, run and done. A
// lookout that was stood down stays stood down: a job still booked can't
// undo a "done" (the board's Stand down, `firetower done`) by
// reporting again. Its report is noted only as a sign of life, so the
// lookout isn't tidied away and started over, and report says so (false).
// `firetower expect` books it again.
func report(l *Lookout, status, message string, value *string, at time.Time) bool {
	if l.Status == "done" && status != "done" {
		l.CheckedAt = tp(at)
		return false
	}
	// A watch already over by its --until (even one whose end had passed
	// at its first report) is what ls and the board call stood down, so a
	// report to it is no move: its history ends at the until (readers add
	// that end, as they work it out for ls). Otherwise a move is recorded
	// whenever the status differs from the last move's.
	if v := view(l, at); !(v.State == "done" && v.DoneBy == "until" && status != "done") {
		m := Move{At: at, Status: status, Message: message, Value: l.Value}
		if value != nil {
			m.Value = *value
		}
		l.addMove(m, at) // before the status changes, so an old file's history starts from its old status
	}
	if l.Status != status || l.CheckedAt == nil {
		l.StatusSince = tp(at)
	}
	l.Status = status
	l.Message = cleanText(message, maxText)
	if value != nil {
		v := cleanText(*value, maxText)
		if v != l.Value || l.MovedAt == nil {
			l.MovedAt = tp(at)
		}
		l.Value = v
		if v == "" {
			l.MovedAt = nil
		}
	}
	if status == "green" {
		l.LastClearAt = tp(at)
	}
	l.CheckedAt = tp(at)
	// A seen covers one status and value. Once either moves on, it is
	// spent, so the same trouble coming back later breaks through again.
	if l.Seen != nil && !seenApplies(l) {
		l.Seen = nil
	}
	return true
}

// stoodDown tells whoever reported to a stood-down lookout why nothing
// changed. It isn't an error: the scheduler's job did its part.
func stoodDown(id string) {
	// For a person at a terminal. A scheduler's job that's still booked
	// would write it to its log on every run.
	if !atTerminal() {
		return
	}
	fmt.Fprintf(os.Stderr, "firetower: %s is stood down (by hand, or its --until passed), so the board still shows it over. To watch it again, book it again: firetower expect %s (with the --next and --until it should have)\n", id, id)
}

func checkStatus(s string) error {
	if !statuses[s] {
		hint := ""
		low := strings.ToLower(s)
		switch {
		case statuses[low]:
			hint = fmt.Sprintf(" (lowercase: %s)", low)
		case low == "stale" || low == "waiting":
			hint = " (" + low + " is worked out by the board, not reported)"
		}
		switch low {
		case "clear", "ok", "fine", "pass", "up", "good":
			hint = " (clear is green)"
		case "smoke", "warn", "warning", "slow":
			hint = " (smoke is yellow)"
		case "fire", "error", "fail", "failed", "down", "bad", "critical":
			hint = " (fire is red)"
		case "stood-down", "stand-down", "over", "finished", "ended":
			hint = " (stood down is done)"
		}
		return fmt.Errorf("status %q: use green, yellow, red or done%s", s, hint)
	}
	return nil
}

// quiet holds back the notes while run checks its flags before the check runs.
var quiet = false

// atTerminal: a person is reading stderr, not a scheduler's log.
// FIRETOWER_TERMINAL=1 says so, for tests.
func atTerminal() bool {
	if os.Getenv("FIRETOWER_TERMINAL") == "1" {
		return true
	}
	st, err := os.Stderr.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func cmdSet(args []string) error {
	const u = `set NAME STATUS ["MESSAGE"] [--district D] [--tower T] [--label L] [--value V] [--next 30m] [--until 6h] [--link X] [--source S] [--tag T]...`
	f, err := parseFlags(args, reportFlags, true)
	if err != nil {
		return bad(u, err)
	}
	f.pos = append(f.pos, f.rest...) // set x red -- "--a message that starts with dashes"
	switch {
	case len(f.pos) < 2:
		return badf(u, "set needs a NAME and a STATUS (green, yellow, red or done)")
	case len(f.pos) > 3:
		return badf(u, "too many words: put the message in quotes, like \"all quiet\"")
	}
	if err := checkName("lookout name", f.pos[0][strings.LastIndex(f.pos[0], "/")+1:]); err != nil {
		return err
	}
	status, msg := f.pos[1], ""
	if len(f.pos) == 3 {
		msg = f.pos[2]
	}
	if err := checkStatus(status); err != nil {
		return err
	}
	d, t, n, err := where(f.pos[0], f, false)
	if err != nil {
		return err
	}
	_, err = update(d, t, n, true, func(l *Lookout, at time.Time) error {
		prev := *l
		before := view(l, at)
		if err := applyCommon(l, f, at, false); err != nil {
			return err
		}
		ok := status == "done" || !endedByUntil(before)
		if ok {
			ok = report(l, status, msg, f.str("value"), at)
		}
		if status == "done" {
			backdateStandDown(l, before) // the same as `firetower done`
		}
		if !ok {
			// A stood-down watch is only noted as alive: nothing the
			// report carried (tags, label, times) lands on it.
			*l = prev
			l.CheckedAt = tp(at)
			stoodDown(l.ID())
		} else if status != "done" && view(l, at).State == "done" {
			stoodDown(l.ID())
		}
		return nil
	})
	return err
}

// cmdExpect puts a lookout on the board before its first report, so a check
// that never starts shows "waiting" instead of nothing, and "no word yet"
// once the --next it was given has passed. Expecting is booking: it anchors
// a span for --until afresh.
func cmdExpect(args []string) error {
	const u = `expect NAME [--next 30m] [--until 6h] [--district D] [--tower T] [--label L] [--link X] [--source S] [--tag T]...`
	f, err := parseFlags(args, reportFlags, false)
	if err != nil {
		return bad(u, err)
	}
	switch {
	case f.has("value"):
		return badf(u, "expect takes no --value: a report sets that")
	case len(f.pos) != 1:
		return badf(u, "expect takes only a NAME (and flags)")
	}
	d, t, n, err := where(f.pos[0], f, false)
	if err != nil {
		return err
	}
	_, err = update(d, t, n, true, func(l *Lookout, at time.Time) error {
		defer func() {
			// Only a watch left with no promise at all waits for good.
			if l.NextBy == nil && l.CheckedAt == nil && !f.has("next") && atTerminal() {
				fmt.Fprintf(os.Stderr, "firetower: no --next, so it waits for its first report for good; give --next for it to show no word if that never comes\n")
			}
		}()
		// Expecting is booking. A watch that was over (stood down, or past
		// its until) starts again, waiting for its first report.
		v := view(l, at) // how it stands before this booking changes its times
		over := l.Name != "" && v.State == "done"
		if err := applyCommon(l, f, at, true); err != nil {
			return err
		}
		if over {
			// Its history says how it ended, then that it was booked again.
			if l.Status != "done" && v.DoneAt != nil {
				l.addMove(Move{At: *v.DoneAt, Status: "done", Message: "its --until passed"}, at)
			}
			l.addMove(Move{At: at, Status: "waiting", Message: "booked again"}, at)
			l.Status, l.Message, l.Value, l.Seen = "", "", "", nil
			l.CheckedAt, l.StatusSince, l.MovedAt = nil, nil, nil
			// The old end is over; one given now stands, even if past.
			if !f.has("until") && l.Until != nil && !at.Before(*l.Until) {
				l.Until = nil
			}
		}
		return nil
	})
	return err
}

func cmdSeen(args []string) error {
	const u = "seen NAME [--district D] [--tower T]"
	f, err := parseFlags(args, map[string]bool{"district": true, "tower": true}, false)
	if err != nil {
		return bad(u, err)
	}
	if len(f.pos) != 1 {
		return badf(u, "seen takes one NAME")
	}
	if elsewhere(f.pos[0]) {
		if err := noPlaceFlags(f); err != nil {
			return bad(u, err)
		}
		return actElsewhere("seen", f.pos[0])
	}
	d, t, n, err := where(f.pos[0], f, true)
	if err != nil {
		return err
	}
	return seen(d, t, n)
}

// elsewhere: a NAME that is another machine's lookout, as the boards print
// it ("work-mac:website/checkout/deploy"). A name of this machine's can't
// hold a ":".
func elsewhere(name string) bool { return strings.Contains(name, ":") }

// noPlaceFlags: another machine's NAME already says where it is.
func noPlaceFlags(f flags) error {
	if f.has("district") || f.has("tower") {
		return fmt.Errorf("another machine's lookout (source:district/tower/name) already says where it is; leave out --district and --tower")
	}
	return nil
}

func seen(d, t, n string) error {
	_, err := update(d, t, n, false, func(l *Lookout, at time.Time) error {
		v := view(l, at)
		if v.State != "yellow" && v.State != "red" {
			what := map[string]string{"green": "it's clear", "done": "it's stood down",
				"stale": "it's gone quiet (seen covers a report, not a silence)", "waiting": "it hasn't reported yet"}[v.State]
			return fmt.Errorf("nothing to acknowledge on %s: %s", l.ID(), what)
		}
		l.Seen = &Seen{At: at, Status: l.Status, Value: l.Value}
		return nil
	})
	return err
}

func cmdDone(args []string) error {
	const u = `done NAME ["MESSAGE"] [--district D] [--tower T]`
	f, err := parseFlags(args, map[string]bool{"district": true, "tower": true}, true)
	if err != nil {
		return bad(u, err)
	}
	f.pos = append(f.pos, f.rest...)
	switch {
	case len(f.pos) < 1:
		return badf(u, "done needs a NAME")
	case len(f.pos) > 2:
		return badf(u, "too many words: put the message in quotes")
	}
	msg := ""
	if len(f.pos) == 2 {
		msg = f.pos[1]
	}
	if elsewhere(f.pos[0]) {
		if err := noPlaceFlags(f); err != nil {
			return bad(u, err)
		}
		if msg != "" || f.dash {
			// It would cross a shell on the other machine: no free text goes.
			return fmt.Errorf("a message can't go to another machine's lookout; without one, done keeps what the watch last said")
		}
		return actElsewhere("done", f.pos[0])
	}
	d, t, n, err := where(f.pos[0], f, true)
	if err != nil {
		return err
	}
	return done(d, t, n, msg)
}

// endedByUntil: a watch over because its --until passed. A report to one is
// only a sign of life, like one to a watch stood down by hand: its own
// times can't revive it (only expect books it again).
func endedByUntil(v View) bool { return v.State == "done" && v.DoneBy == "until" }

// backdateStandDown: a watch whose end had already passed (v, from before
// the report) was stood down then, not now. Its status_since and its
// history say so, as ls and the board already do.
func backdateStandDown(l *Lookout, v View) {
	if v.State == "done" && v.DoneAt != nil {
		l.StatusSince = v.DoneAt
		l.backdateMove("done", *v.DoneAt)
	}
}

func done(d, t, n, msg string) error {
	_, err := update(d, t, n, false, func(l *Lookout, at time.Time) error {
		// With no message of its own, a stand-down keeps what the watch
		// last said: the board shows it over, and what it saw.
		if msg == "" {
			msg = l.Message
		}
		// A watch whose end already passed was stood down then, not now:
		// standing it down again mustn't put it back on the board for a day.
		v := view(l, at)
		report(l, "done", msg, nil, at)
		backdateStandDown(l, v)
		return nil
	})
	return err
}

func cmdRm(args []string) error {
	const u = "rm NAME | rm --district D [--tower T] | rm --tag T... [--district D] [--tower T]"
	f, err := parseFlags(args, map[string]bool{"district": true, "tower": true, "tag": true}, false)
	if err != nil {
		return bad(u, err)
	}
	if len(f.pos) > 1 || (len(f.pos) == 0 && !f.has("district") && !f.has("tower") && !f.has("tag")) {
		return badf(u, "rm takes one NAME, --district (and --tower) to remove a whole district or tower, or --tag to remove what carries it")
	}
	if f.has("tag") {
		if len(f.pos) != 0 {
			return badf(u, "rm --tag removes every lookout carrying the tags, so it takes no NAME")
		}
		tags, err := tagFilter(f)
		if err != nil {
			return bad(u, err)
		}
		return rmTagged(f, tags)
	}
	if len(f.pos) == 1 {
		if elsewhere(f.pos[0]) {
			if err := noPlaceFlags(f); err != nil {
				return bad(u, err)
			}
			return actElsewhere("rm", f.pos[0])
		}
		d, t, n, err := where(f.pos[0], f, true)
		if err != nil {
			return err
		}
		return rmLookout(d, t, n)
	}
	// The district, as every other command finds it: --district, then
	// $FIRETOWER_DISTRICT, then the config's default.
	cfg, cerr := loadConfig()
	district := cfg.DefaultDistrict
	switch {
	case f.has("district"):
		district = f.get("district")
	case os.Getenv("FIRETOWER_DISTRICT") != "":
		district = os.Getenv("FIRETOWER_DISTRICT")
	case cfg.districtLost:
		// Removing a whole tower from a district guessed at is how the
		// wrong lookouts go: say so and stop.
		return fmt.Errorf("%v\nso the default district isn't known; fix the config, or name the district with --district", cerr)
	case cerr != nil:
		fmt.Fprintf(os.Stderr, "firetower: %v (the default district, %s, still holds)\n", cerr, district)
	}
	if err := checkName("district", district); err != nil {
		return err
	}
	var towers []string
	if f.has("tower") {
		if err := checkName("tower", f.get("tower")); err != nil {
			return err
		}
		towers = []string{f.get("tower")}
	}
	dir := stateDir()
	unlock, err := lockDir(dir)
	if err != nil {
		return err
	}
	defer unlock()
	if towers == nil {
		entries, err := os.ReadDir(filepath.Join(dir, district))
		if err != nil {
			return fmt.Errorf("no district %s", district)
		}
		for _, e := range entries {
			if e.IsDir() && checkName("tower", e.Name()) == nil {
				towers = append(towers, e.Name())
			}
		}
	}
	n := 0
	for _, t := range towers {
		files, err := os.ReadDir(filepath.Join(dir, district, t))
		if err != nil {
			if f.has("tower") {
				hint := ""
				if !f.has("district") {
					hint = " (in the default district; give --district for another)"
				}
				return fmt.Errorf("no tower %s/%s%s", district, t, hint)
			}
			continue
		}
		// Only what is the Firetower's: lookouts and their temp files.
		for _, e := range files {
			name := e.Name()
			if strings.HasSuffix(name, ".json") && checkName("lookout", strings.TrimSuffix(name, ".json")) == nil {
				if os.Remove(filepath.Join(dir, district, t, name)) == nil {
					n++
				}
			} else if strings.HasPrefix(name, ".") && strings.Contains(name, ".json.tmp") {
				os.Remove(filepath.Join(dir, district, t, name))
			}
		}
		removeEmpty(dir, district, t)
	}
	removeEmpty(dir, district, "")
	fmt.Printf("removed %d lookout%s\n", n, plural(n))
	return nil
}

// rmTagged removes every lookout carrying all of tags, in any state, within
// --district and --tower when they're given (exactly: no default district,
// since a tag already says which).
func rmTagged(f flags, tags []string) error {
	for _, k := range []string{"district", "tower"} {
		if f.has(k) {
			if err := checkName(k, f.get(k)); err != nil {
				return err
			}
		}
	}
	dir := stateDir()
	unlock, err := lockDir(dir)
	if err != nil {
		return err
	}
	defer unlock()
	n := 0
	ls, bad := loadAll(dir)
	for _, l := range ls {
		if f.has("district") && l.District != f.get("district") || f.has("tower") && l.Tower != f.get("tower") || !hasTags(l.Tags, tags) {
			continue
		}
		if os.Remove(lookoutPath(dir, l.District, l.Tower, l.Name)) == nil {
			n++
			removeEmpty(dir, l.District, l.Tower)
		}
	}
	unreadable(bad)
	if n == 0 {
		// Not an error (a tidy-up run twice finds nothing the second time),
		// but said, so a typo in a script's tag shows in its log.
		fmt.Fprintf(os.Stderr, "firetower: no lookout carries %s, so nothing was removed\n", strings.Join(tags, " and "))
	}
	fmt.Printf("removed %d lookout%s tagged %s\n", n, plural(n), strings.Join(tags, ", "))
	return nil
}

// unreadable says when lookout files couldn't be read, so a command that
// went by their tags couldn't see them: a broken file stays, and shows as
// fire on the board.
func unreadable(bad []broken) {
	if len(bad) > 0 {
		fmt.Fprintf(os.Stderr, "firetower: %d lookout file%s can't be read, so %s tags aren't known (firetower ls shows %s)\n",
			len(bad), plural(len(bad)), map[bool]string{true: "its", false: "their"}[len(bad) == 1], map[bool]string{true: "it", false: "them"}[len(bad) == 1])
	}
}

func rmLookout(d, t, n string) error {
	dir := stateDir()
	unlock, err := lockDir(dir)
	if err != nil {
		return err
	}
	defer unlock()
	if err := os.Remove(lookoutPath(dir, d, t, n)); err != nil {
		return fmt.Errorf("no lookout %s/%s/%s", d, t, n)
	}
	removeEmpty(dir, d, t)
	return nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func cmdLs(args []string) error {
	const u = "ls [--district D] [--tower T] [--tag T]... [--all] [--json]"
	f, err := parseFlags(args, map[string]bool{"district": true, "tower": true, "tag": true, "json": false, "all": false}, false)
	if err != nil {
		return bad(u, err)
	}
	if len(f.pos) != 0 {
		return badf(u, "ls takes no NAME: filter with --district, --tower or --tag")
	}
	tags, err := tagFilter(f)
	if err != nil {
		return bad(u, err)
	}
	at := now()
	var vs []View
	anyTags := false
	for _, v := range localViews(stateDir(), at, f.has("all")) {
		if f.has("district") && v.District != f.get("district") || f.has("tower") && v.Tower != f.get("tower") || !hasTags(v.Tags, tags) {
			continue
		}
		vs = append(vs, v)
		anyTags = anyTags || len(v.Tags) > 0
	}
	if f.has("json") {
		if vs == nil {
			vs = []View{}
		}
		b, _ := json.MarshalIndent(vs, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	if len(vs) == 0 {
		switch {
		case f.has("district") || f.has("tower") || f.has("tag"):
			fmt.Println("No lookouts there. firetower ls shows them all.")
		case !f.has("all") && len(localViews(stateDir(), at, true)) > 0:
			fmt.Println("Nothing on watch: only lookouts stood down over a day ago (firetower ls --all shows them).")
		default:
			fmt.Println("No lookouts. Report one: firetower set NAME green \"all quiet\" --next 30m")
		}
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	head := "STATE\tLOOKOUT\tCHECKED\tMESSAGE"
	if anyTags {
		head = "STATE\tLOOKOUT\tTAGS\tCHECKED\tMESSAGE"
	}
	fmt.Fprintln(w, head)
	for _, v := range vs {
		st := v.State
		if v.Seen {
			st += " (seen)"
		}
		id := v.ID
		if anyTags {
			t := strings.Join(v.Tags, ",")
			if t == "" {
				t = "-"
			}
			id += "\t" + t
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", st, id, ago(v.CheckedAt, at), lsMessage(v, at))
	}
	return w.Flush()
}

// tagFilter is the --tag a reader filters by: every one must be on a lookout.
func tagFilter(f flags) ([]string, error) {
	if !f.has("tag") {
		return nil, nil
	}
	if len(f.list("tag")) == 1 && strings.TrimSpace(f.get("tag")) == "none" {
		return nil, fmt.Errorf("--tag none clears a lookout's tags; to filter, name a tag")
	}
	return parseTags(f.list("tag"))
}

// lsMessage is the message with what the Firetower knows that the check
// doesn't: since when, the last clear, movement.
func lsMessage(v View, at time.Time) string {
	msg := v.Message
	var notes []string
	switch v.State {
	case "stale":
		if v.CheckedAt == nil {
			notes = append(notes, "no word yet, due "+ago(v.NextBy, at))
		} else {
			notes = append(notes, "no word since "+ago(v.CheckedAt, at)+", due "+ago(v.NextBy, at))
		}
	case "waiting":
		if v.NextBy != nil {
			notes = append(notes, "waiting for a first report, due "+until(v.NextBy, at))
		} else {
			notes = append(notes, "waiting for a first report")
		}
	case "done":
		if v.DoneBy == "until" {
			notes = append(notes, "its watch ended "+ago(v.DoneAt, at))
		} else {
			notes = append(notes, "stood down "+ago(v.DoneAt, at))
		}
	case "red", "yellow":
		notes = append(notes, "since "+ago(v.StatusSince, at))
		if v.LastClearAt != nil {
			notes = append(notes, "last clear "+ago(v.LastClearAt, at))
		} else {
			notes = append(notes, "never clear")
		}
	}
	switch map[bool]string{true: v.Movement}[v.State != "stale" && v.State != "done"] {
	case "moved":
		notes = append(notes, "moved")
	case "still":
		notes = append(notes, "still since "+ago(v.MovedAt, at))
	}
	if len(notes) > 0 {
		if msg != "" {
			msg += " "
		}
		msg += "(" + strings.Join(notes, ", ") + ")"
	}
	return msg
}

// until says how far off t is: "in 20m", or "20m ago" once it has passed.
func until(t *time.Time, at time.Time) string {
	if t == nil || !t.After(at) {
		return ago(t, at)
	}
	d := t.Sub(at)
	switch {
	case d < time.Minute:
		return "in under a minute"
	case d < time.Hour:
		return fmt.Sprintf("in %dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("in %dh", int(d.Hours()))
	}
	return fmt.Sprintf("in %dd", int(d.Hours()/24))
}

func ago(t *time.Time, at time.Time) string {
	if t == nil {
		return "never"
	}
	d := at.Sub(*t)
	switch {
	case d < 10*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func cmdGc(args []string) error {
	const u = "gc [--tag T]..."
	f, err := parseFlags(args, map[string]bool{"tag": true}, false)
	if err != nil {
		return bad(u, err)
	}
	if len(f.pos) != 0 {
		return badf(u, "gc takes nothing but --tag")
	}
	tags, err := tagFilter(f)
	if err != nil {
		return bad(u, err)
	}
	if tags != nil {
		n, err := gcTagged(stateDir(), now(), tags)
		if err != nil {
			return err
		}
		fmt.Printf("cleared %d stood-down lookout%s tagged %s\n", n, plural(n), strings.Join(tags, ", "))
		return nil
	}
	n, err := gc(stateDir(), now())
	if err != nil {
		return err
	}
	fmt.Printf("cleared %d stood-down lookout%s\n", n, plural(n))
	return nil
}

// gcTagged forgets every stood-down lookout carrying all of tags, at once
// rather than a day on: "a temporary watch goes when it's over", for a
// person's own script or scheduler to say. The Firetower does nothing by
// itself; this is the command it's said with.
func gcTagged(dir string, at time.Time, tags []string) (int, error) {
	unlock, err := lockDir(dir)
	if err != nil {
		return 0, err
	}
	defer unlock()
	n, carried := 0, false
	ls, bad := loadAll(dir)
	defer unreadable(bad)
	for _, l := range ls {
		carried = carried || hasTags(l.Tags, tags)
		if view(l, at).State == "done" && hasTags(l.Tags, tags) {
			if os.Remove(lookoutPath(dir, l.District, l.Tower, l.Name)) == nil {
				n++
				removeEmpty(dir, l.District, l.Tower)
			}
		}
	}
	if !carried {
		// Nothing over is a normal day; nothing carrying the tag at all
		// may be a typo, so it's said.
		fmt.Fprintf(os.Stderr, "firetower: no lookout carries %s\n", strings.Join(tags, " and "))
	}
	return n, nil
}

// gc deletes the files of lookouts stood down over a day ago (readers have
// stopped showing them already) and temp files a crashed write left.
func gc(dir string, at time.Time) (int, error) {
	unlock, err := lockDir(dir)
	if err != nil {
		return 0, err
	}
	defer unlock()
	return gcLocked(dir, at, ""), nil
}

// gcLocked tidies up, leaving skip (the file a write just saved) alone.
func gcLocked(dir string, at time.Time, skip string) int {
	n := 0
	ls, _ := loadAll(dir)
	for _, l := range ls {
		// Gone from the boards, and quiet for a day too. A stood-down
		// lookout something still reports to is kept, stood down: deleting
		// it would let the next report start it over as a new watch.
		path := lookoutPath(dir, l.District, l.Tower, l.Name)
		if path != skip && view(l, at).hidden(at) && (l.CheckedAt == nil || at.Sub(*l.CheckedAt) > doneShown) {
			if os.Remove(path) == nil {
				n++
				removeEmpty(dir, l.District, l.Tower)
			}
		}
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*", "*", ".*.json.tmp*"))
	for _, m := range matches {
		if st, err := os.Stat(m); err == nil && at.Sub(st.ModTime()) > time.Hour {
			os.Remove(m)
		}
	}
	return n
}

// maybeGC runs gc at most once an hour, from a write that already holds the
// lock, so nobody has to book gc for the files to stay tidy.
func maybeGC(dir string, at time.Time, skip string) {
	mark := filepath.Join(dir, ".gc")
	if st, err := os.Stat(mark); err == nil && at.Sub(st.ModTime()) < time.Hour && !st.ModTime().After(at) {
		return
	}
	gcLocked(dir, at, skip)
	os.WriteFile(mark, nil, 0o600)
	os.Chtimes(mark, at, at)
}

// sortViews puts views in board order.
func sortViews(vs []View) {
	sort.SliceStable(vs, func(i, j int) bool {
		a, b := vs[i], vs[j]
		if a.District != b.District {
			return a.District < b.District
		}
		if a.Tower != b.Tower {
			return a.Tower < b.Tower
		}
		return a.ID < b.ID
	})
}

// cmdHistory prints a lookout's moves: each change of status as it landed,
// oldest first, so a person or a board can see how it got where it is.
func cmdHistory(args []string) error {
	const u = "history NAME [--since 7d] [--json] [--district D] [--tower T]"
	f, err := parseFlags(args, map[string]bool{"since": true, "json": false, "district": true, "tower": true}, false)
	if err != nil {
		return bad(u, err)
	}
	if len(f.pos) != 1 {
		return badf(u, "history takes one NAME")
	}
	if elsewhere(f.pos[0]) {
		return fmt.Errorf("history is kept with the lookout's own files: run firetower history on the machine it's on")
	}
	var since time.Time
	if f.has("since") {
		span, err := parseSpan(f.get("since"))
		if err != nil || span <= 0 {
			return badf(u, "--since %q: want a span like 7d", f.get("since"))
		}
		since = now().Add(-span)
	}
	d, t, n, err := where(f.pos[0], f, true)
	if err != nil {
		return err
	}
	l, err := loadLookout(lookoutPath(stateDir(), d, t, n))
	if err != nil {
		return fmt.Errorf("can't read the report for %s/%s/%s: %v (firetower rm removes it, and the next report replaces it)", d, t, n, err)
	}
	moves := cleanMoves(historyAsRead(l, now()), since)
	if f.has("json") {
		b, _ := json.MarshalIndent(moves, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	if len(moves) == 0 {
		fmt.Printf("No changes recorded for %s%s.\n", l.ID(), map[bool]string{true: " in that time"}[f.has("since")])
		return nil
	}
	at := now()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "WHEN\tSTATUS\tMESSAGE")
	for _, m := range moves {
		msg := m.Message
		if m.Value != "" {
			msg = strings.TrimSpace(msg + " (" + m.Value + ")")
		}
		fmt.Fprintf(w, "%s (%s)\t%s\t%s\n", m.At.Local().Format("Jan 2 15:04"), ago(tp(m.At), at), m.Status, msg)
	}
	return w.Flush()
}

// historyAsRead is a lookout's history as a reader sees it: caught up to
// where the lookout is (one from before 0.4 starts at its current status),
// and, for a watch over by its --until, ending there with "done", as ls and
// the board show it.
func historyAsRead(l *Lookout, at time.Time) []Move {
	l.startHistory(at)
	v := view(l, at)
	if v.State != "done" || v.DoneBy != "until" || v.DoneAt == nil {
		return l.History
	}
	out := []Move{}
	for _, m := range l.History {
		if !m.At.After(*v.DoneAt) {
			out = append(out, m)
		}
	}
	if n := len(out); n == 0 || out[n-1].Status != "done" {
		out = append(out, Move{At: v.DoneAt.UTC(), Status: "done", Message: "its --until passed"})
	}
	return out
}

// cleanMoves is a history to show: tidied as on writing, from since on.
func cleanMoves(in []Move, since time.Time) []Move {
	out := []Move{}
	for _, m := range tidyMoves(in, now()) {
		if !m.At.Before(since) {
			out = append(out, m)
		}
	}
	return out
}
