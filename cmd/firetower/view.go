package main

import (
	"sort"
	"time"
)

// A View is a lookout as every reader sees it: the file plus what the
// Firetower works out from it (no word, stood down, waiting, seen, moved).
// It keeps no clock: it compares the two times a report carries, next_by and
// until, with now, and nothing else.
// `ls --json`, the feed and the board's API all print Views, so a custom
// board never has to repeat the rules.
type View struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	District    string     `json:"district"`
	Tower       string     `json:"tower"`
	Label       string     `json:"label"`
	State       string     `json:"state"` // green yellow red done stale waiting
	Status      string     `json:"status,omitempty"`
	Message     string     `json:"message,omitempty"`
	Value       string     `json:"value,omitempty"`
	Movement    string     `json:"movement,omitempty"` // moved | still
	Seen        bool       `json:"seen"`
	SeenAt      *time.Time `json:"seen_at,omitempty"`
	CheckedAt   *time.Time `json:"checked_at,omitempty"`
	MovedAt     *time.Time `json:"moved_at,omitempty"`
	LastClearAt *time.Time `json:"last_clear_at,omitempty"`
	StatusSince *time.Time `json:"status_since,omitempty"`
	NextBy      *time.Time `json:"next_by,omitempty"`
	StaleSince  *time.Time `json:"stale_since,omitempty"`
	Until       *time.Time `json:"until,omitempty"`
	DoneAt      *time.Time `json:"done_at,omitempty"`
	DoneBy      string     `json:"done_by,omitempty"` // "report" (done, Stand down) or "until" (its end passed)
	Link        string     `json:"link,omitempty"`
	Source      string     `json:"source,omitempty"`
	Tags        tagList    `json:"tags,omitempty"`
	Broken      bool       `json:"broken,omitempty"`

	// Set by a board for lookouts that came from another machine.
	Origin       string `json:"origin,omitempty"`        // the source's name
	HomeDistrict string `json:"home_district,omitempty"` // its own district, when the source folds them into one
	Contact      string `json:"contact,omitempty"`       // "lost" while the source is out of contact
}

// Stood-down lookouts stay on the boards, grey, this long, then go.
const doneShown = 24 * time.Hour

func view(l *Lookout, at time.Time) View {
	v := View{
		ID: l.ID(), Name: l.Name, District: l.District, Tower: l.Tower, Label: l.Label,
		Status: l.Status, Message: l.Message, Value: l.Value,
		CheckedAt: l.CheckedAt, MovedAt: l.MovedAt, LastClearAt: l.LastClearAt,
		StatusSince: l.StatusSince, NextBy: l.NextBy, Until: l.Until, Link: l.Link, Source: l.Source,
		Tags: cleanTags(l.Tags), // a file anyone wrote: held to what a tag is, as a feed's are
	}
	if v.Label == "" {
		v.Label = l.Name
	}
	overdue := l.NextBy != nil && at.After(*l.NextBy)
	switch {
	case l.Status == "done":
		v.State, v.DoneAt, v.DoneBy = "done", l.StatusSince, "report" // when it was stood down, not its latest sign of life
	case l.Until != nil && !at.Before(*l.Until):
		v.State, v.DoneAt, v.DoneBy = "done", l.Until, "until" // the watch is over
	case overdue:
		v.State, v.StaleSince = "stale", l.NextBy // no word: the promised report didn't come
	case l.CheckedAt == nil || l.Status == "":
		v.State = "waiting" // expected, not reported yet
	default:
		v.State = l.Status
	}
	if (v.State == "yellow" || v.State == "red") && seenApplies(l) {
		v.Seen, v.SeenAt = true, tp(l.Seen.At)
	}
	if l.Value != "" && l.MovedAt != nil && l.CheckedAt != nil {
		v.Movement = "still"
		if l.MovedAt.Equal(*l.CheckedAt) {
			v.Movement = "moved"
		}
	}
	return v
}

func seenApplies(l *Lookout) bool {
	return l.Seen != nil && l.Seen.Status == l.Status && l.Seen.Value == l.Value
}

func brokenView(b broken) View {
	// The file's path and the parser's words mean nothing on a board, and
	// say where the files live: just what's wrong and what to do.
	return View{
		ID: b.District + "/" + b.Tower + "/" + b.Name, Name: b.Name, District: b.District, Tower: b.Tower,
		Label: b.Name, State: "red", Broken: true, StatusSince: b.Since,
		Message: "Can't read this report: its file is damaged. Remove it, or the next report replaces it.",
	}
}

// hidden: stood down more than a day ago, so readers stop showing it.
func (v View) hidden(at time.Time) bool {
	return v.State == "done" && v.DoneAt != nil && at.Sub(*v.DoneAt) > doneShown
}

// localViews is every lookout under dir as of at, broken files included,
// long-stood-down ones left out unless all is set.
func localViews(dir string, at time.Time, all bool) []View {
	ls, bad := loadAll(dir)
	out := make([]View, 0, len(ls)+len(bad))
	for _, l := range ls {
		v := view(l, at)
		if all || !v.hidden(at) {
			out = append(out, v)
		}
	}
	for _, b := range bad {
		out = append(out, brokenView(b))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// rank orders states by how much they want attention. A seen smoke or fire
// ranks with clear: it is known, so it stops shouting. A source out of
// contact ranks with no word: both are silence.
func rank(v View) int {
	if v.Contact == "lost" {
		return 4
	}
	if v.Seen {
		return 1
	}
	switch v.State {
	case "red":
		return 5
	case "stale":
		return 4
	case "yellow":
		return 3
	case "waiting":
		return 2
	case "green":
		return 1
	}
	return 0
}

// bucket is the one word a summary counts a lookout under.
func bucket(v View) string {
	switch {
	case v.Contact == "lost":
		return "lost"
	case v.Seen:
		return "seen"
	}
	return v.State
}

var rankName = []string{"done", "green", "waiting", "yellow", "stale", "red"}
