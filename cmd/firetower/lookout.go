package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// A Lookout is one watch: the file <dir>/<district>/<tower>/<name>.json.
// Checks write status, message and the optional fields through the CLI; the
// Firetower alone writes the owned fields (moved_at, last_clear_at,
// status_since, seen). It keeps no clock: when a check runs, how long it may
// run and when a watch ends are the scheduler's business. A report carries
// two times the scheduler knows, next_by (when the next report is due) and
// until (when the watch ends), and readers only compare them with now. Fields it doesn't know are kept in extra
// and written back, so the format can grow.
type Lookout struct {
	Name        string     `json:"name"`
	District    string     `json:"district"`
	Tower       string     `json:"tower"`
	Label       string     `json:"label,omitempty"`
	Status      string     `json:"status,omitempty"`
	Message     string     `json:"message,omitempty"`
	Value       string     `json:"value,omitempty"`
	MovedAt     *time.Time `json:"moved_at,omitempty"`
	CheckedAt   *time.Time `json:"checked_at,omitempty"`
	LastClearAt *time.Time `json:"last_clear_at,omitempty"`
	StatusSince *time.Time `json:"status_since,omitempty"`
	NextBy      *time.Time `json:"next_by,omitempty"`
	Until       *time.Time `json:"until,omitempty"`
	Seen        *Seen      `json:"seen"`
	Link        string     `json:"link,omitempty"`
	Source      string     `json:"source,omitempty"`
	Tags        tagList    `json:"tags,omitempty"`
	History     moveList   `json:"history,omitempty"`

	extra map[string]json.RawMessage
}

// A Move is one change of status, as it landed: the Firetower writes one
// whenever a report's status differs from the last, and keeps the latest
// maxMoves, so a board can draw how a watch got where it is without
// remembering the past itself. No word isn't a move: it's worked out by
// readers from next_by, and shows in a history as the gap between reports.
type Move struct {
	At      time.Time `json:"at"`
	Status  string    `json:"status"`
	Message string    `json:"message,omitempty"`
	Value   string    `json:"value,omitempty"`
}

const maxMoves = 50

// moveStatuses are what a move can be: the four a report gives, and
// "waiting" for a watch booked again with expect.
var moveStatuses = map[string]bool{"green": true, "yellow": true, "red": true, "done": true, "waiting": true}

// moveList is a history as JSON reads it. A file anyone wrote may hold
// anything there; a wrong shape must never make the lookout itself
// unreadable (and the next report replace it), so whatever isn't a move is
// left out, and tidyMoves holds the rest to what a move is.
type moveList []Move

func (ml *moveList) UnmarshalJSON(b []byte) error {
	var raw []json.RawMessage
	if json.Unmarshal(b, &raw) != nil {
		*ml = nil
		return nil
	}
	out := moveList{}
	for _, r := range raw {
		var m Move
		if json.Unmarshal(r, &m) == nil {
			out = append(out, m)
		}
	}
	*ml = out
	return nil
}

// tidyMoves holds a history to what a move is: a known status, a time
// that's come (none from the future), clean one-line text, in time order,
// at most maxMoves (the newest). It's the one trim, on writing and on
// reading alike.
func tidyMoves(in []Move, now time.Time) moveList {
	out := moveList{}
	for _, m := range in {
		if !moveStatuses[m.Status] || m.At.IsZero() {
			continue
		}
		// A time ahead of now (a clock that stepped back, or a planted
		// entry) is kept, as now: never lost, and never ahead of what's
		// recorded next.
		if m.At.After(now) {
			m.At = now
		}
		m.At = m.At.UTC()
		m.Message, m.Value = cleanText(m.Message, maxText), cleanText(m.Value, maxText)
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	if len(out) > maxMoves {
		out = append(moveList{}, out[len(out)-maxMoves:]...)
	}
	return out
}

// startHistory brings a lookout's history up to where the lookout is: one
// from before history (0.3 and earlier) starts at its current status, and
// one an older version wrote to since (its status changed with no move)
// gets that status as a move, from when it began.
func (l *Lookout) startHistory(now time.Time) {
	l.History = tidyMoves(l.History, now)
	if !moveStatuses[l.Status] || l.StatusSince == nil {
		return
	}
	// A watch over by its --until isn't caught up past its end: what came
	// after is no move (a report to it is only a sign of life).
	if l.Until != nil && !now.Before(*l.Until) && l.Status != "done" && l.StatusSince.After(*l.Until) {
		return
	}
	n := len(l.History)
	if n == 0 || (l.History[n-1].Status != l.Status && l.StatusSince.After(l.History[n-1].At)) {
		at := l.StatusSince.UTC()
		if at.After(now) {
			at = now.UTC()
		}
		l.History = append(l.History, Move{At: at, Status: l.Status, Message: cleanText(l.Message, maxText), Value: cleanText(l.Value, maxText)})
	}
}

// addMove records m, unless it says what the last move already says.
func (l *Lookout) addMove(m Move, now time.Time) {
	l.startHistory(now)
	if n := len(l.History); n > 0 && l.History[n-1].Status == m.Status {
		return
	}
	m.At = m.At.UTC()
	m.Message, m.Value = cleanText(m.Message, maxText), cleanText(m.Value, maxText)
	l.History = tidyMoves(append(l.History, m), now)
}

// backdateMove moves the last move, if it's status, to at: a stand-down
// that took effect earlier than it was written (an --until that passed).
func (l *Lookout) backdateMove(status string, at time.Time) {
	if n := len(l.History); n > 0 && l.History[n-1].Status == status {
		l.History[n-1].At = at.UTC()
	}
}

// Seen is an acknowledgement of one status and value. It applies only while
// both are unchanged: the message is left out on purpose, because messages
// often carry a time and would end a seen on every check.
type Seen struct {
	At     time.Time `json:"at"`
	Status string    `json:"status"`
	Value  string    `json:"value,omitempty"`
}

var knownFields = []string{"name", "district", "tower", "label", "status", "message", "value",
	"moved_at", "checked_at", "last_clear_at", "status_since", "next_by", "until", "seen", "link", "source", "tags", "history"}

func (l *Lookout) UnmarshalJSON(b []byte) error {
	type plain Lookout
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return err
	}
	for _, k := range knownFields {
		delete(all, k)
	}
	*l = Lookout(p)
	l.extra = all
	return nil
}

func (l Lookout) MarshalJSON() ([]byte, error) {
	type plain Lookout
	b, err := json.Marshal(plain(l))
	if err != nil || len(l.extra) == 0 {
		return b, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	for k, v := range l.extra {
		if _, ok := m[k]; !ok {
			m[k] = v
		}
	}
	return json.Marshal(m)
}

func (l *Lookout) ID() string { return l.District + "/" + l.Tower + "/" + l.Name }

// Statuses a check can report. The boards say clear, smoke, fire and stood
// down; the files and the CLI keep the plain words.
var statuses = map[string]bool{"green": true, "yellow": true, "red": true, "done": true}

// Names of lookouts, towers and districts are file and folder names, so they
// are held to a safe alphabet: lowercase (case-insensitive file systems would
// make Deploy and deploy one file), no slashes, no leading dot.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// tagList is tags as JSON reads them: a list of strings. Anything else (a
// lone string, numbers, junk from a file or a feed someone else wrote) reads
// as the strings it holds, or none, never as an error that would lose the
// whole lookout. Which of them are tags is decided by cleanTags.
type tagList []string

func (t *tagList) UnmarshalJSON(b []byte) error {
	var raw []json.RawMessage
	if json.Unmarshal(b, &raw) != nil {
		var one string
		if json.Unmarshal(b, &one) == nil {
			*t = tagList{one}
		} else {
			*t = nil
		}
		return nil
	}
	out := tagList{}
	for _, r := range raw {
		var s string
		if json.Unmarshal(r, &s) == nil {
			out = append(out, s)
		}
	}
	*t = out
	return nil
}

// Tags are words a lookout carries for whoever reads it: a board groups by
// them, a person's own script acts on them (firetower ls --tag, rm --tag, gc
// --tag). They're names, so they read the same everywhere, and few.
const maxTags = 16

// parseTags reads --tag values: each a word, or words split by commas.
// "none" alone clears them. The result is sorted and has no repeats, so the
// same tags given in any order are the same tags.
func parseTags(vals []string) ([]string, error) {
	if len(vals) == 1 && strings.TrimSpace(vals[0]) == "none" {
		return []string{}, nil
	}
	seen := map[string]bool{}
	var out []string
	for _, v := range vals {
		for _, t := range strings.Split(v, ",") {
			t = strings.TrimSpace(t)
			if t == "none" {
				return nil, fmt.Errorf("--tag none clears the tags, so it goes alone")
			}
			if t == "" {
				return nil, fmt.Errorf("--tag %q: an empty tag (a stray comma?)", v)
			}
			if err := checkName("--tag", t); err != nil {
				return nil, err
			}
			if !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	if len(out) > maxTags {
		return nil, fmt.Errorf("--tag: %d tags, and a lookout carries at most %d", len(out), maxTags)
	}
	sort.Strings(out)
	return out, nil
}

// hasTags: the lookout carries every one of want.
func hasTags(have, want []string) bool {
	for _, w := range want {
		found := false
		for _, h := range have {
			if h == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func checkName(kind, s string) error {
	if !nameRE.MatchString(s) {
		return fmt.Errorf("%s %q: use lowercase letters, digits, '.', '_' and '-', starting with a letter or digit (at most 64)", kind, s)
	}
	return nil
}

const maxText = 200

var (
	csiRE = regexp.MustCompile(`\x1b\[[0-9:;<=>?]*[ -/]*[@-~]`)
	oscRE = regexp.MustCompile(`\x1b\][^\x07\x1b]*(\x07|\x1b\\)?`)
)

// cleanText makes s one line of plain text: terminal escape sequences and
// control characters go, so does anything that reorders text on screen
// (bidi overrides), runs of whitespace become one space, and it is cut at
// max characters. Whatever a check prints, it can't garble a terminal or a
// board.
func cleanText(s string, max int) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "?")
	}
	s = oscRE.ReplaceAllString(s, "")
	s = csiRE.ReplaceAllString(s, "")
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case r == '\t' || r == '\n' || r == '\r' || unicode.IsSpace(r):
			space = true
			continue
		case unicode.IsControl(r), isBidi(r):
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	out := b.String()
	if utf8.RuneCountInString(out) > max {
		r := []rune(out)
		out = string(r[:max-1]) + "…"
	}
	return out
}

func isBidi(r rune) bool {
	return (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) || r == 0x200E || r == 0x200F || r == 0x061C
}

// parseWhen reads a moment for --next and --until: a span from now (30m, 6h,
// 2d), a clock time (23:59 is the next 23:59, today or tomorrow), "today
// 18:00", "tomorrow 07:00", a local date and time, or RFC 3339. "none" clears
// it. relative says it was a span.
func parseWhen(flag, s string, now time.Time) (t *time.Time, relative bool, err error) {
	if s == "none" || s == "" {
		return nil, false, nil
	}
	if d, err := parseSpan(s); err == nil {
		t := now.Add(d).UTC().Truncate(time.Second)
		return &t, true, nil
	}
	// A clock time ("18:00", "tomorrow 07:00") is worked out from now, so
	// it counts as relative: a booked line repeating it every run would
	// otherwise move the end along with each run.
	if t, ok := parseClock(s, now); ok {
		return &t, true, nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02T15:04", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			t = t.UTC()
			return &t, false, nil
		}
	}
	// A date alone means through that day: until the next midnight.
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		t = t.AddDate(0, 0, 1).UTC()
		return &t, false, nil
	}
	hint := ""
	if _, err := strconv.Atoi(s); err == nil {
		hint = " (a number needs a unit: " + s + "m, " + s + "h)"
	} else if m := upperUnitRE.FindStringSubmatch(s); m != nil {
		hint = " (units are lowercase: " + m[1] + strings.ToLower(m[2]) + ")"
	}
	return nil, false, fmt.Errorf("--%s %q%s: want a span like 30m, 6h, 2w or \"5 hours\", a time like 23:59 or \"tomorrow 07:00\", a date like 2026-11-01 (through that day), a time like 2026-11-01 06:00, or none to clear it", flag, s, hint)
}

var upperUnitRE = regexp.MustCompile(`^(\d+)([SMHDW])$`)

var clockRE = regexp.MustCompile(`^(?:(today|tomorrow) +)?(\d{1,2}):(\d{2})$`)

// parseClock reads "23:59", "today 18:00" or "tomorrow 07:00" in local time.
// A bare clock time already past today means tomorrow's.
func parseClock(s string, now time.Time) (time.Time, bool) {
	m := clockRE.FindStringSubmatch(strings.ToLower(strings.TrimSpace(s)))
	if m == nil {
		return time.Time{}, false
	}
	h, _ := strconv.Atoi(m[2])
	mi, _ := strconv.Atoi(m[3])
	if h > 23 || mi > 59 {
		return time.Time{}, false
	}
	local := now.In(time.Local)
	t := time.Date(local.Year(), local.Month(), local.Day(), h, mi, 0, 0, time.Local)
	if m[1] == "tomorrow" || (m[1] == "" && !t.After(local)) {
		t = t.AddDate(0, 0, 1)
	}
	return t.UTC(), true
}

var dayRE = regexp.MustCompile(`^(\d+)d(.*)$`)

// parseSpan reads a span: Go durations plus d (days), so 30s, 8s, 2d.
// maxSpan keeps a span from overflowing a time (and landing in the past).
const maxSpan = 100 * 365 * 24 * time.Hour

var wordSpanRE = regexp.MustCompile(`(?i)^(\d{1,9}) *(secs?|seconds?|mins?|minutes?|hrs?|hours?|days?|weeks?|d|w)$`)

func parseSpan(s string) (time.Duration, error) {
	// "5 hours", "90 minutes", "2 days": the way Crontinental writes spans.
	// Go's own units (30m, 1h30m) stay case-sensitive: 30M isn't minutes.
	s = strings.TrimSpace(s)
	if m := wordSpanRE.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		unit := map[byte]time.Duration{'s': time.Second, 'm': time.Minute, 'h': time.Hour, 'd': 24 * time.Hour, 'w': 7 * 24 * time.Hour}[strings.ToLower(m[2])[0]]
		d := time.Duration(n) * unit
		if d > maxSpan || d/unit != time.Duration(n) {
			return 0, fmt.Errorf("bad span")
		}
		return d, nil
	}
	var extra time.Duration
	if m := dayRE.FindStringSubmatch(s); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil || n > 36500 {
			return 0, fmt.Errorf("bad span")
		}
		extra = time.Duration(n) * 24 * time.Hour
		s = m[2]
		if s == "" {
			return extra, nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 || d > maxSpan || d+extra > maxSpan {
		return 0, fmt.Errorf("bad span")
	}
	return d + extra, nil
}
