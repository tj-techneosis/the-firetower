package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/url"
	"os"
	"strings"
	"text/template"
	"time"
)

// The words and colours the boards use. The files and the CLI keep the plain
// words (green, stale); a board says what a lookout would.
var boardWord = map[string]string{
	"green": "clear", "yellow": "smoke", "red": "fire", "done": "stood down",
	"stale": "no word", "waiting": "waiting", "seen": "seen", "lost": "out of contact", "none": "no lookouts",
}

var boardColor = map[string]string{
	"green": "#3fb950", "yellow": "#d29922", "red": "#f85149", "stale": "#a371f7",
	"waiting": "#8b949e", "done": "#6e7681", "seen": "#8b949e", "lost": "#a371f7", "none": "#6e7681",
}

// bar order: what's shown first in a count.
var barOrder = []string{"red", "lost", "stale", "yellow", "waiting", "seen", "green"}

func cmdBar(args []string) error {
	f, err := parseFlags(args, map[string]bool{"format": true, "template": true, "timeout": true, "tag": true}, false)
	const u = "bar [--format plain|json|waybar|swiftbar] [--template T] [--tag T]... [--timeout 8s]"
	if err != nil {
		return bad(u, err)
	}
	if len(f.pos) != 0 {
		return badf(u, "bar takes no words")
	}
	format := f.get("format")
	if format == "" {
		format = "plain"
	}
	if f.has("template") && f.has("format") {
		return fmt.Errorf("--format or --template, not both")
	}
	var tmpl *template.Template
	if f.has("template") {
		tmpl, err = template.New("bar").Funcs(barFuncs).Parse(f.get("template"))
		if err != nil {
			return fmt.Errorf("--template: %v", err)
		}
	} else if !map[string]bool{"plain": true, "json": true, "waybar": true, "swiftbar": true, "xbar": true}[format] {
		return fmt.Errorf("--format %q: plain, json, waybar or swiftbar (xbar is swiftbar)", format)
	}
	timeout := 8 * time.Second
	if f.has("timeout") {
		if timeout, err = parseSpan(f.get("timeout")); err != nil || timeout <= 0 {
			return fmt.Errorf("--timeout %q: want a span like 8s", f.get("timeout"))
		}
	}
	cfg, cerr := loadConfig()
	if cerr != nil {
		// A bar that prints nothing looks like a quiet one. Say it.
		return printBarError(format, cerr)
	}
	tags, err := tagFilter(f)
	if err != nil {
		return bad(u, err)
	}
	// A running board has the whole picture already: ask it. Only with
	// none does the bar dial every source itself, once, and go.
	st, berr := askBoard(cfg)
	if berr != nil {
		p := newPost(cfg)
		p.once(timeout)
		got := p.state()
		st = &got
	}
	if tags != nil {
		kept := []View{}
		has := map[string]bool{}
		for _, v := range st.Lookouts {
			if hasTags(v.Tags, tags) {
				kept = append(kept, v)
				has[v.Origin] = true
			}
		}
		st.Lookouts = kept
		// A machine counts only for what it has under the tag: one out of
		// contact with none of it is no part of this reading.
		srcs := []SourceStatus{}
		for _, src := range st.Sources {
			if has[src.Name] {
				srcs = append(srcs, src)
			}
		}
		st.Sources = srcs
	}
	s := summarize(*st)
	s.Board = berr == nil
	if berr != nil && !errors.Is(berr, errNoBoard) {
		s.BoardNote = berr.Error()
	} else if berr != nil && isBoard(cfg.Board.Port) {
		s.BoardNote = fmt.Sprintf("a board on port %d didn't prove it's yours, or was too busy to", cfg.Board.Port)
	}
	if tmpl != nil {
		return tmpl.Execute(os.Stdout, s)
	}
	switch format {
	case "json":
		b, _ := json.MarshalIndent(s, "", "  ")
		fmt.Println(string(b))
	case "waybar":
		b, _ := json.Marshal(waybar(s))
		fmt.Println(string(b))
	case "swiftbar", "xbar":
		fmt.Print(swiftbar(s, cfg))
	default:
		fmt.Println(plainBar(s))
	}
	return nil
}

var barFuncs = template.FuncMap{
	"word":  func(s string) string { return boardWord[s] },
	"color": func(s string) string { return boardColor[s] },
	"ago":   func(t *time.Time) string { return ago(t, now()) },
	"join":  strings.Join,
}

func printBarError(format string, err error) error {
	msg := cleanText(err.Error(), maxText)
	switch format {
	case "json":
		b, _ := json.Marshal(map[string]string{"worst": "red", "error": msg})
		fmt.Println(string(b))
	case "waybar":
		b, _ := json.Marshal(map[string]string{"text": "● !", "class": "red", "alt": "red", "tooltip": html.EscapeString("Firetower: " + msg)})
		fmt.Println(string(b))
	case "swiftbar", "xbar":
		fmt.Printf("! | color=%s sfimage=binoculars.fill\n---\n%s\n", boardColor["red"], swiftEscape("Firetower: "+msg))
	default:
		fmt.Println("firetower: " + msg)
	}
	return exitError{1, ""}
}

// counts is "1 fire · 2 smoke · 9 clear", in order of attention.
func counts(s Summary) string {
	var parts []string
	for _, k := range barOrder {
		n := s.Counts[k]
		switch {
		case n == 0:
		case k == "lost":
			var names []string
			for _, src := range s.Sources {
				if src.Contact == "lost" {
					names = append(names, src.Name)
				}
			}
			parts = append(parts, strings.Join(names, ", ")+" out of contact")
		default:
			parts = append(parts, fmt.Sprintf("%d %s", n, boardWord[k]))
		}
	}
	return strings.Join(parts, " · ")
}

func plainBar(s Summary) string {
	if s.Count == 0 && s.Attention == 0 && counts(s) == "" {
		return "no lookouts"
	}
	if s.Worst == "green" {
		return fmt.Sprintf("all clear (%d)", s.Count)
	}
	return counts(s)
}

type waybarOut struct {
	Text    string `json:"text"`
	Alt     string `json:"alt"`
	Class   string `json:"class"`
	Tooltip string `json:"tooltip"`
}

// waybar is one custom module's line (Waybar reads tooltips as Pango
// markup, so everything a check wrote is escaped).
func waybar(s Summary) waybarOut {
	text := fmt.Sprintf("● %d", s.Count)
	if s.Attention > 0 {
		text = fmt.Sprintf("● %d/%d", s.Attention, s.Count)
	}
	var tip []string
	if c := counts(s); c != "" {
		tip = append(tip, html.EscapeString(c))
	} else {
		tip = append(tip, "No lookouts")
	}
	many := len(s.Districts) > 1
	for _, d := range s.Districts {
		if many {
			tip = append(tip, "", "<b>"+html.EscapeString(d.Name)+"</b>")
		}
		for _, t := range d.Towers {
			tip = append(tip, fmt.Sprintf("<span color='%s'>●</span> %s", boardColor[t.Worst], html.EscapeString(t.Name)))
			for _, v := range t.Lookouts {
				line := fmt.Sprintf("    <span color='%s'>●</span> %s", boardColor[bucket(v)], html.EscapeString(v.Label))
				if m := barMessage(v); m != "" {
					line += ": " + html.EscapeString(m)
				}
				tip = append(tip, line)
			}
		}
	}
	return waybarOut{Text: text, Alt: s.Worst, Class: s.Worst, Tooltip: strings.Join(tip, "\n")}
}

// barMessage is the message, or what the Firetower knows in its place.
func barMessage(v View) string {
	m := v.Message
	switch bucket(v) {
	case "stale":
		if v.CheckedAt == nil {
			return "no word yet"
		}
		return "no word since " + ago(v.CheckedAt, now())
	case "lost":
		return "out of contact"
	case "waiting":
		return "waiting for a first report"
	}
	if len([]rune(m)) > 80 {
		m = string([]rune(m)[:79]) + "…"
	}
	return m
}

// swiftbar is a SwiftBar (or xbar) plugin's output: the menu-bar title, then
// a menu with a section per district and a line per lookout.
func swiftbar(s Summary, cfg *Config) string {
	var b strings.Builder
	title := fmt.Sprint(s.Count)
	if s.Attention > 0 {
		title = fmt.Sprintf("%d/%d", s.Attention, s.Count)
	}
	fmt.Fprintf(&b, "%s | color=%s sfimage=binoculars.fill sfcolor=%s\n---\n", title, boardColor[s.Worst], boardColor[s.Worst])
	if c := counts(s); c != "" {
		fmt.Fprintf(&b, "%s\n", swiftEscape(c))
	} else {
		b.WriteString("No lookouts\n")
	}
	many := len(s.Districts) > 1
	for _, d := range s.Districts {
		b.WriteString("---\n")
		if many {
			fmt.Fprintf(&b, "%s | size=11 color=%s\n", swiftEscape(strings.ToUpper(d.Name)), boardColor["waiting"])
		}
		for _, t := range d.Towers {
			fmt.Fprintf(&b, "● %s | color=%s\n", swiftEscape(t.Name), boardColor[t.Worst])
			for _, v := range t.Lookouts {
				line := "● " + v.Label
				if m := barMessage(v); m != "" {
					line += ": " + m
				}
				params := "color=" + boardColor[bucket(v)]
				if u := webLink(v.Link); u != "" {
					params += " href=" + u
				}
				fmt.Fprintf(&b, "--%s | %s\n", swiftEscape(line), params)
			}
		}
	}
	if !s.Board {
		if s.BoardNote != "" {
			b.WriteString("---\n" + swiftEscape("The board didn't answer ("+s.BoardNote+"): this reads every machine itself") + " | color=" + boardColor["waiting"] + "\n")
		} else {
			b.WriteString("---\nNo board is running: this reads every machine itself | color=" + boardColor["waiting"] + "\n")
		}
		b.WriteString("Start the board: firetower board --open | color=" + boardColor["waiting"] + "\n")
	} else if u, err := onceURL(cfg.Board.Port); err == nil {
		// A one-time address, as --open gives: the menu bar app hands its
		// links to a browser's command line, which everyone can read.
		fmt.Fprintf(&b, "---\nOpen the board | href=%s\n", u)
	}
	return b.String()
}

// swiftEscape keeps a check's text from being read as SwiftBar syntax: a |
// would start parameters, and a leading - a submenu or a separator.
func swiftEscape(s string) string {
	s = strings.ReplaceAll(s, "|", "¦")
	return strings.TrimLeft(s, "-")
}

// webLink is a link only when it's a plain http(s) URL with no spaces or
// quotes: the only kind a menu or a board opens directly.
func webLink(s string) string {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	out := u.String()
	if strings.ContainsAny(out, " \"'<>`\\") {
		return ""
	}
	return out
}
