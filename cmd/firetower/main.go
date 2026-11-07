// The Firetower: a lookout for background watches. Checks report one line;
// boards show every watch at a glance. See README.md and docs/design.md.
package main

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

const usage = `firetower: a lookout for background watches

Report (what checks run):
  firetower set NAME STATUS ["MESSAGE"] [--district D] [--tower T] [--label L]
                [--value V] [--next 30m|TIME|none] [--until 6h|TIME|none] [--link X] [--source S]
                [--tag T]...
  firetower run NAME [the same flags but --value] [--message M] -- CMD ...
  firetower expect NAME [--next 30m] [--until 6h] [--district D] [--tower T] [--label L] [--link X]
                [--source S] [--tag T]...

Look after them:
  firetower ls [--district D] [--tower T] [--tag T]... [--all] [--json]
  firetower history NAME [--since 7d] [--json]
                                   how it got here: its last 50 changes of status
  firetower seen NAME              acknowledge the current smoke or fire
  firetower done NAME ["MESSAGE"]  stand a watch down (it stays down until expect;
                                   with no MESSAGE it keeps what the watch last said)
  firetower rm NAME | --district D [--tower T] | --tag T... [--district D] [--tower T]
  firetower gc [--tag T]...        forget lookouts stood down over a day ago
                                   (with --tag: every stood-down one carrying it, now)

Boards:
  firetower board [--port N] [--ui DIR]   the board, here, until Ctrl-C (for a service)
  firetower board --open | --start | --stop | --url [--once]
                                   open it in a window (starting it in the background if
                                   it isn't up), only start it, stop it, or print its address
                                   (--once: an address good for one browser, for a minute)
  firetower bar [--format plain|json|waybar|swiftbar|xbar] [--template T] [--tag T]...
                [--timeout 8s]
  firetower watch --json [--once]  the feed boards read

  firetower upgrade [--check] | firetower version | firetower help

STATUS is green, yellow, red or done. A NAME is name, tower/name or
district/tower/name; the district and the tower default to main (every
command that takes a NAME also takes --district and --tower). Another
machine's lookout is source:district/tower/name, as the boards show it:
seen, done and rm reach it through the running board, or its source's act
command. A message of more than one word goes in quotes; one that starts
with a dash goes after --. run's check goes after -- (for pipes or &&: -- sh
-c '...'); it reports 0 as clear, 1 as smoke, anything else as fire, and
exits with the check's own code (64 for a mistake in the run line itself).

--tag is a word a lookout carries (repeat it, or join words with commas;
"none" clears them): a board groups by it, and your own scripts act on it
with ls, rm and gc --tag. To filter, every --tag given must be on the
lookout. The Firetower does nothing with a tag by itself.

--next is when the next report is due (past it, the board says "no word");
--until is when the watch ends. Both take a span (30m, 6h, 2w, "5 hours"), a
clock time (23:59, "tomorrow 07:00"), a date (through that day), a full time
(local unless it says its zone), or none. An --until span or clock time
counts from when the watch was booked, not from each run. They are the
scheduler's times, written down: the Firetower keeps no clock, and never
times, schedules or kills anything. run takes them from $CRONTINENTAL_NEXT
and $CRONTINENTAL_UNTIL when Crontinental sets them, and --source from its
job (crontinental:<job>, for a run Crontinental started: it sets both
$CRONTINENTAL_JOB and $CRONTINENTAL_RUN).

bar and seen, done and rm on another machine's lookout ask the running board
first (the one on board.port, once it proves it's yours), so they don't dial
every machine themselves.

State lives in ~/.local/state/firetower ($FIRETOWER_DIR); config in
~/.config/firetower/config.toml ($FIRETOWER_CONFIG).
`

// usageError is a command used wrong: what was wrong first, then the usage.
type usageError struct{ msg, reason string }

func (e usageError) Error() string {
	if e.reason != "" {
		return "firetower: " + e.reason + "\nusage: firetower " + e.msg
	}
	return "usage: firetower " + e.msg
}

// bad is a usage error with its reason.
func bad(usage string, err error) error { return usageError{usage, err.Error()} }

func badf(usage, format string, a ...any) error {
	return usageError{usage, fmt.Sprintf(format, a...)}
}

// exitError ends the process with a code of its own (run passes its
// command's exit code through), after saying msg, if any.
type exitError struct {
	code int
	msg  string
}

func (e exitError) Error() string { return e.msg }

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	// --help anywhere before a run's "--" is a request for help, not a flag.
	for _, a := range args {
		if a == "--" {
			break
		}
		if a == "--help" || a == "-h" {
			fmt.Print(usage)
			return
		}
	}
	var err error
	switch cmd {
	case "set":
		err = cmdSet(args)
	case "run":
		err = cmdRun(args)
	case "expect":
		err = cmdExpect(args)
	case "seen":
		err = cmdSeen(args)
	case "done":
		err = cmdDone(args)
	case "rm":
		err = cmdRm(args)
	case "ls", "list":
		err = cmdLs(args)
	case "history":
		err = cmdHistory(args)
	case "gc":
		err = cmdGc(args)
	case "watch":
		err = cmdWatch(args)
	case "bar":
		err = cmdBar(args)
	case "board":
		err = cmdBoard(args)
	case "upgrade":
		err = cmdUpgrade(args)
	case "version", "--version", "-v":
		fmt.Println(versionString())
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		err = badf("<command> ...", "no command %q (firetower help lists them)", cmd)
	}
	if err == nil {
		return
	}
	var ee exitError
	if errors.As(err, &ee) {
		if ee.msg != "" {
			fmt.Fprintln(os.Stderr, "firetower: "+ee.msg)
		}
		os.Exit(ee.code)
	}
	// run passes its check's exit code through, so 1 and 2 mean smoke and
	// fire there. Its own mistakes (a bad flag, a bad name) exit 64, the
	// usage code, so a booking with a typo doesn't look like a failing check.
	usageCode, errCode := 2, 1
	if cmd == "run" {
		usageCode, errCode = 64, 64
	}
	var ue usageError
	if errors.As(err, &ue) {
		fmt.Fprintln(os.Stderr, ue.Error())
		os.Exit(usageCode)
	}
	fmt.Fprintln(os.Stderr, "firetower: "+err.Error())
	os.Exit(errCode)
}

// flags is a parsed command line: positionals, flag values, and whatever
// came after "--".
type flags struct {
	pos  []string
	vals map[string]string
	all  map[string][]string // every value a flag was given, for the repeatable ones (--tag)
	rest []string
	dash bool
}

func (f flags) has(k string) bool      { _, ok := f.vals[k]; return ok }
func (f flags) get(k string) string    { return f.vals[k] }
func (f flags) list(k string) []string { return f.all[k] }
func (f flags) str(k string) *string {
	if v, ok := f.vals[k]; ok {
		return &v
	}
	return nil
}

// parseFlags reads flags anywhere among the positionals (--x v or --x=v).
// spec maps each flag to whether it takes a value. "--" ends the flags when
// stopAtDash is set, and everything after it is rest.
func parseFlags(args []string, spec map[string]bool, stopAtDash bool) (flags, error) {
	f := flags{vals: map[string]string{}, all: map[string][]string{}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" && stopAtDash {
			f.rest, f.dash = args[i+1:], true
			return f, nil
		}
		if len(a) > 1 && a[0] == '-' && a[1] != '-' && !isNumber(a) {
			if a == "-m" {
				return f, fmt.Errorf("unknown flag -m: a message is a quoted word, like \"all quiet\" (run's own is --message)")
			}
			return f, fmt.Errorf("unknown flag %s: flags start with -- (a message that starts with a dash goes after --)", a)
		}
		if !strings.HasPrefix(a, "--") || a == "--" {
			f.pos = append(f.pos, a)
			continue
		}
		k, v, hasV := strings.Cut(a[2:], "=")
		takes, ok := spec[k]
		if !ok {
			if k == "message" {
				return f, fmt.Errorf("unknown flag --message: only run takes it; a message here is a quoted word")
			}
			return f, fmt.Errorf("unknown flag --%s", k)
		}
		if takes && !hasV {
			if i+1 >= len(args) {
				return f, fmt.Errorf("--%s needs a value", k)
			}
			i++
			v = args[i]
			// "--next $UNSET -- curl": the empty variable is gone and the
			// flag would swallow the next flag (or --) as its value.
			if v == "--" || flagLike.MatchString(v) {
				return f, fmt.Errorf("--%s needs a value, and got %s (an empty variable? for a value that starts with --, write --%s=%s)", k, v, k, v)
			}
		}
		if !takes && hasV {
			return f, fmt.Errorf("--%s takes no value", k)
		}
		if takes && v == "" && (k == "next" || k == "until") {
			// An unset variable ("--next $INTERVAL") must not quietly drop a promise.
			return f, fmt.Errorf("--%s is empty: give a time, or none to clear it", k)
		}
		f.vals[k] = v
		f.all[k] = append(f.all[k], v)
	}
	return f, nil
}

func isNumber(s string) bool {
	_, err := strconv.ParseFloat(s, 64)
	return err == nil
}

// flagLike is a word that is a flag, not a value that happens to start with dashes.
var flagLike = regexp.MustCompile(`^--[a-z][a-z-]*(=.*)?$`)
