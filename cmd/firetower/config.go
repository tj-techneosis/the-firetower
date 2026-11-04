package main

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is ~/.config/firetower/config.toml (or $FIRETOWER_CONFIG). Most
// people need none: the defaults are "main", and a board shows this
// machine's lookouts.
type Config struct {
	DefaultDistrict string   `toml:"default_district"`
	DefaultTower    string   `toml:"default_tower"`
	Board           Board    `toml:"board"`
	Sources         []Source `toml:"source"`

	path string
	// A broken file whose default couldn't be used: it is "main" instead.
	districtLost, towerLost bool
}

func (c *Config) defaultsLost() bool { return c.districtLost || c.towerLost }

type Board struct {
	Port  int    `toml:"port"`
	UI    string `toml:"ui"`
	Local *bool  `toml:"local"` // false: show only the sources, not this machine's own lookouts
	Shown
}

// Shown is which towers a board shows from one place (a source, or this
// machine under [board]): every one, less those matching hide, plus those
// matching keep. A pattern is a shell glob against "district/tower", or
// against the tower alone when it has no "/".
type Shown struct {
	Hide []string `toml:"hide"`
	Keep []string `toml:"keep"`
}

// A Source is another feed a board shows: any command that prints
// `firetower watch --json`, usually over ssh.
type Source struct {
	Name     string   `toml:"name"`
	Command  []string `toml:"command"`
	District string   `toml:"district"`
	Act      []string `toml:"act"`
	Shown
}

const defaultPort = 7480

func configPath() string {
	if p := os.Getenv("FIRETOWER_CONFIG"); p != "" {
		return p
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "firetower", "config.toml")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "firetower", "config.toml")
}

// loadConfig reads the config. A missing file is no config. A broken one is
// an error the caller decides about: a report still goes somewhere (a check
// that can't report is worse than one in the wrong tower), a board refuses to
// start. A file that reads but has a bad setting elsewhere (a board's port, a
// pattern, an unknown key) keeps its default_district and default_tower, so a
// slip in a board's settings never moves where reports land, a value of the
// wrong type included; only a file that can't be read as TOML, or a bad
// default itself, leaves "main". The Config says which (districtLost,
// towerLost).
func loadConfig() (*Config, error) {
	c := &Config{path: configPath()}
	b, err := os.ReadFile(c.path)
	if err != nil {
		if os.IsNotExist(err) {
			return c.fill(), nil
		}
		return c.lost(), fmt.Errorf("%s: %v", c.path, err)
	}
	md, err := toml.Decode(string(b), c)
	if err != nil {
		err = fmt.Errorf("%s: %v", c.path, err)
		// A value of the wrong type elsewhere (port = "7480", hide =
		// "x") stops the decode; the defaults, read on their own, may
		// still be good.
		var d struct {
			DefaultDistrict string `toml:"default_district"`
			DefaultTower    string `toml:"default_tower"`
		}
		if _, derr := toml.Decode(string(b), &d); derr == nil {
			return (&Config{path: c.path, DefaultDistrict: d.DefaultDistrict, DefaultTower: d.DefaultTower}).defaultsOnly(), err
		}
		return c.lost(), err
	}
	if und := md.Undecoded(); len(und) > 0 {
		keys := make([]string, len(und))
		for i, k := range und {
			keys[i] = k.String()
		}
		return c.defaultsOnly(), fmt.Errorf("%s: unknown setting %s", c.path, strings.Join(keys, ", "))
	}
	if err := c.check(); err != nil {
		return c.defaultsOnly(), fmt.Errorf("%s: %v", c.path, err)
	}
	return c.fill(), nil
}

// lost is the config of a file that couldn't be read: nothing from it.
func (c *Config) lost() *Config {
	return (&Config{path: c.path, districtLost: true, towerLost: true}).fill()
}

// defaultsOnly is what a report can still use from a file with a bad setting:
// its defaults, each one that is a name.
func (c *Config) defaultsOnly() *Config {
	k := &Config{path: c.path}
	if c.Board.Port > 0 && c.Board.Port <= 65535 {
		k.Board.Port = c.Board.Port // for --stop, which needs only the port
	}
	if c.DefaultDistrict != "" {
		if checkName("district", c.DefaultDistrict) == nil {
			k.DefaultDistrict = c.DefaultDistrict
		} else {
			k.districtLost = true
		}
	}
	if c.DefaultTower != "" {
		if checkName("tower", c.DefaultTower) == nil {
			k.DefaultTower = c.DefaultTower
		} else {
			k.towerLost = true
		}
	}
	return k.fill()
}

func (c *Config) fill() *Config {
	if c.DefaultDistrict == "" {
		c.DefaultDistrict = "main"
	}
	if c.DefaultTower == "" {
		c.DefaultTower = "main"
	}
	if c.Board.Port == 0 {
		c.Board.Port = defaultPort
	}
	return c
}

func (c *Config) check() error {
	if c.DefaultDistrict != "" {
		if err := checkName("default_district", c.DefaultDistrict); err != nil {
			return err
		}
	}
	if c.DefaultTower != "" {
		if err := checkName("default_tower", c.DefaultTower); err != nil {
			return err
		}
	}
	if c.Board.Port < 0 || c.Board.Port > 65535 {
		return fmt.Errorf("board.port %d is not a port", c.Board.Port)
	}
	if err := c.Board.Shown.check("[board]"); err != nil {
		return err
	}
	seen := map[string]bool{}
	for i, s := range c.Sources {
		if err := checkName(fmt.Sprintf("source %d's name", i+1), s.Name); err != nil {
			return err
		}
		if seen[s.Name] {
			return fmt.Errorf("two sources named %q", s.Name)
		}
		seen[s.Name] = true
		if len(s.Command) == 0 {
			return fmt.Errorf("source %q has no command", s.Name)
		}
		if s.District != "" {
			if err := checkName(fmt.Sprintf("source %q's district", s.Name), s.District); err != nil {
				return err
			}
		}
		if err := s.Shown.check(fmt.Sprintf("source %q", s.Name)); err != nil {
			return err
		}
	}
	return nil
}

// patternRE is what a hide or keep pattern may hold: the name alphabet and
// the glob characters. Anything else (a capital, a space) could never match
// a name, so it is refused rather than left to hide nothing.
var patternRE = regexp.MustCompile(`^[a-z0-9._*?\[\]!^-]+$`)

func (s Shown) check(where string) error {
	for _, field := range []struct {
		name string
		pats []string
	}{{"hide", s.Hide}, {"keep", s.Keep}} {
		for _, p := range field.pats {
			bad := func(why string) error {
				return fmt.Errorf("%s's %s pattern %q %s", where, field.name, p, why)
			}
			if p == "" {
				return bad("is empty")
			}
			parts := strings.Split(p, "/")
			if len(parts) > 2 {
				return bad("has more than one '/': it is district/tower, or a tower")
			}
			for _, part := range parts {
				if part == "" {
					return bad("has an empty side of its '/': it is district/tower, or a tower")
				}
				if !patternRE.MatchString(part) || !canMatchName(part) {
					return bad("can never match: names are lowercase letters, digits, '.', '_' and '-', starting with a letter or digit, and a pattern adds only * ? [ ] (and [! ] or [^ ])")
				}
				if _, err := path.Match(glob(part), ""); err != nil {
					return bad("is not a pattern: " + err.Error())
				}
			}
		}
	}
	return nil
}

// canMatchName rules out what patternRE lets through but no name can be: a
// first character that can't start a name, "!" or "^" outside the start of
// a class, and plain text that isn't a name (too long, say).
func canMatchName(part string) bool {
	if strings.ContainsRune("._-!^]", rune(part[0])) {
		return false
	}
	for i, r := range part {
		if (r == '!' || r == '^') && (i == 0 || part[i-1] != '[') {
			return false
		}
	}
	if !strings.ContainsAny(part, "*?[") {
		return nameRE.MatchString(part)
	}
	return true
}

// glob turns a shell glob into Go's: a class negates with "!" in the shell
// and "^" in Go (both are taken). Names can't hold "!", so nothing is lost.
func glob(p string) string { return strings.ReplaceAll(p, "[!", "[^") }

// shows says whether a lookout in district/tower is shown. keep only brings
// back what hide took away, so keep alone changes nothing.
func (s Shown) shows(district, tower string) bool {
	return !matchTower(s.Hide, district, tower) || matchTower(s.Keep, district, tower)
}

func matchTower(pats []string, district, tower string) bool {
	for _, p := range pats {
		name := tower
		if strings.Contains(p, "/") {
			name = district + "/" + tower
		}
		if ok, _ := path.Match(glob(p), name); ok {
			return true
		}
	}
	return false
}

func (c *Config) showLocal() bool { return c.Board.Local == nil || *c.Board.Local }

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p
}
