package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// cmdRun runs a check that knows nothing about the Firetower and reports
// what it saw: its exit code is the status (0 clear, 1 smoke, 2 fire,
// anything else fire too), its last line of output the message, a
// "VALUE: x" line the value. A check that can't run at all is fire, never
// silence; a run that is stopped (a scheduler's timeout, a kill) reports
// fire before it goes. The command's output passes through untouched and its
// exit code is run's own, so the scheduler's history stays true too.
func cmdRun(args []string) error {
	const u = `run NAME [--district D] [--tower T] [--label L] [--next 30m] [--until 6h] [--link X] [--source S] [--tag T]... [--message M] -- CMD ...`
	f, err := parseFlags(args, with(reportFlags, "message"), true)
	if err != nil {
		hasDash := false
		for _, a := range args {
			if a == "--" {
				hasDash = true
				break
			}
		}
		switch {
		case strings.HasPrefix(err.Error(), "unknown flag -m"):
			return badf(u, "unknown flag -m: run's message flag is --message")
		case strings.HasPrefix(err.Error(), "unknown flag") && !hasDash:
			return badf(u, "%v: is that the check's own flag? Put the check after --, like: firetower run NAME -- curl -fsS https://example.com/", err)
		}
		return bad(u, err)
	}
	switch {
	case len(f.pos) == 0:
		return badf(u, "run needs a NAME, then -- and the check to run")
	case !f.dash || len(f.rest) == 0:
		return badf(u, "put the check after --, like: firetower run %s -- curl -fsS https://example.com/", f.pos[0])
	case len(f.pos) > 1:
		return badf(u, "put the check after --: firetower run %s -- %s", f.pos[0], strings.Join(f.pos[1:], " "))
	}
	if f.has("value") {
		return fmt.Errorf("run takes its value from a VALUE: line in the command's output, not --value")
	}
	// The scheduler knows when the next run is due and when the watch
	// ends. When it says so in the environment, those are the defaults, so
	// the timing is said once, to the scheduler; a flag still wins.
	//
	// CRONTINENTAL_NEXT is when the next run starts, and its report lands
	// only after it finishes (and the tick may start it a minute late), so
	// the report is due nextSlack after it. A check that takes longer passes
	// --next itself.
	for flag, env := range map[string]string{"next": "CRONTINENTAL_NEXT", "until": "CRONTINENTAL_UNTIL"} {
		v := os.Getenv(env)
		if f.has(flag) {
			continue
		}
		if v == "" {
			// Run by Crontinental with no NEXT: it won't come round again
			// (a one-off, a repeat's last run), so nothing more is promised.
			if flag == "next" && os.Getenv("CRONTINENTAL_JOB") != "" {
				f.vals["next"] = "none"
			}
			continue
		}
		// What a scheduler hands over is a time, RFC 3339, or "none".
		if v == "none" {
			f.vals[flag] = "none"
			continue
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			fmt.Fprintf(os.Stderr, "firetower: ignoring %s=%q: want a time like 2026-11-01T06:00:00Z\n", env, v)
			continue
		}
		if flag == "next" {
			// A next start already past (catching up after a sleep) is
			// due from now.
			if at := now(); t.Before(at) {
				t = at
			}
			t = t.Add(nextSlack)
		}
		v = t.UTC().Format(time.RFC3339)
		f.vals[flag] = v
	}
	d, t, n, err := where(f.pos[0], f, false)
	if err != nil {
		return err
	}
	// Check the flags before running anything, so a typo isn't found only
	// after a long check. (Quietly: the report says anything worth saying.)
	quiet = true
	err = applyCommon(&Lookout{}, f, now(), false)
	quiet = false
	if err != nil {
		return err
	}
	// rep reports. --message stands in for the command's own words only on
	// a normal exit: "Can't run" and "Stopped" always say so.
	rep := func(status, msg string, value *string) error {
		_, err := update(d, t, n, true, func(l *Lookout, at time.Time) error {
			tags := l.Tags
			if err := applyCommon(l, f, at, false); err != nil {
				return err
			}
			if !report(l, status, msg, value, at) {
				l.Tags = tags // a stood-down watch is only noted as alive: its tags stay what they were
				stoodDown(l.ID())
			} else if view(l, at).State == "done" {
				stoodDown(l.ID())
			}
			return nil
		})
		return err
	}

	cmd := exec.Command(f.rest[0], f.rest[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Its own process group, so a stop reaches its children too. From a
	// terminal that group is in the background, and reading the terminal
	// would stop it for good (SIGTTIN), so a check run by hand gets no
	// terminal input: one that prompts fails, and says so, instead of
	// hanging. Piped input passes through.
	if st, err := os.Stdin.Stat(); err == nil && st.Mode()&os.ModeCharDevice == 0 {
		cmd.Stdin = os.Stdin
	}
	// Output is copied through pipes, and Wait waits for every holder of
	// them: a check that leaves something running in the background would
	// hold run open. Two seconds after the check itself ends, stop waiting.
	cmd.WaitDelay = 2 * time.Second
	tail := &lastLines{}
	cmd.Stdout = io.MultiWriter(os.Stdout, tail)
	cmd.Stderr = io.MultiWriter(os.Stderr, tail)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigs)

	if err := cmd.Start(); err != nil {
		why := strings.TrimPrefix(err.Error(), "fork/exec ")
		var ee *exec.Error
		if errors.As(err, &ee) {
			why = fmt.Sprintf("%s: %v", f.rest[0], ee.Err)
		}
		if strings.ContainsAny(f.rest[0], " |&;<>") {
			fmt.Fprintf(os.Stderr, "firetower: %q is one word to run, not a shell line; for pipes or &&, run it with: -- sh -c '...'\n", f.rest[0])
		}
		if rerr := rep("red", "Can't run the check: "+why, nil); rerr != nil {
			return exitError{127, fmt.Sprintf("%s, and can't report it: %v", why, rerr)}
		}
		return exitError{127, "can't run the check: " + why}
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	stop := func(why string, code int) error {
		// The whole process group: a check is often a shell with children.
		syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-waited:
		case <-time.After(5 * time.Second):
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			// Something that left the group (setsid) can't be reached;
			// report anyway rather than wait on it.
			select {
			case <-waited:
			case <-time.After(5 * time.Second):
			}
		}
		if err := rep("red", why, nil); err != nil {
			return exitError{code, fmt.Sprintf("%s, and can't report it: %v", why, err)}
		}
		return exitError{code, fmt.Sprintf("reported %s/%s/%s as fire: %s", d, t, n, strings.ToLower(why[:1])+why[1:])}
	}
	select {
	case s := <-sigs:
		code := 128 + int(s.(syscall.Signal))
		if atTerminal() {
			fmt.Fprintln(os.Stderr, "\nfiretower: stopping the check (press Ctrl-C again to stop it at once)...")
		}
		// A second signal while it winds down: stop waiting.
		go func() { <-sigs; syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }()
		return stop(fmt.Sprintf("Stopped (%s) before it finished.", sigName(s)), code)
	case err := <-waited:
		code := 0
		if errors.Is(err, exec.ErrWaitDelay) {
			// The check ended; something it left behind held its output.
			err = nil
			code = cmd.ProcessState.ExitCode()
		}
		if err != nil {
			var xe *exec.ExitError
			if !errors.As(err, &xe) {
				return err
			}
			code = xe.ExitCode()
			if code < 0 {
				// Killed by a signal from outside, not one sent to run.
				ws, _ := xe.Sys().(syscall.WaitStatus)
				code = 128 + int(ws.Signal())
				msg := fmt.Sprintf("The check was killed (%s) before it finished.", ws.Signal())
				if rerr := rep("red", msg, nil); rerr != nil {
					return exitError{code, rerr.Error()}
				}
				return exitError{code, ""}
			}
		}
		// 0 clear, 1 smoke, 2 fire; anything else is fire too. No code
		// stands a watch down: a stood-down watch stays over, and tools
		// exit with all sorts of codes (curl exits 3 on a bad URL). A
		// check that's finished says so with `firetower done`.
		status := map[int]string{0: "green", 1: "yellow", 2: "red"}[code]
		if status == "" {
			status = "red"
		}
		msg, value := tail.result()
		if f.has("message") {
			msg = f.get("message")
		}
		if msg == "" {
			msg = fmt.Sprintf("Exited %d.", code)
			if code == 0 {
				msg = "Ran clean."
			}
		} else if status == "red" && code != 2 {
			// The exit code goes on the end, kept whole however long the
			// line it follows.
			suffix := fmt.Sprintf(" (exit %d)", code)
			msg = cleanText(msg, maxText-len([]rune(suffix))) + suffix
		}
		if err := rep(status, msg, value); err != nil {
			return exitError{max(code, 1), "the check ran, but can't report it: " + err.Error()}
		}
		if code != 0 {
			return exitError{code, ""}
		}
		return nil
	}
}

// nextSlack is how long after the scheduler's next start time its report
// is due: the run's own time, and a late tick.
const nextSlack = 5 * time.Minute

func sigName(s os.Signal) string {
	switch s {
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGHUP:
		return "SIGHUP"
	}
	return s.String()
}

// lastLines keeps the end of a command's output: its last non-empty line
// that isn't a VALUE: line, and its last VALUE: line. Only a bounded tail is
// kept, however much the command prints.
type lastLines struct {
	mu      sync.Mutex
	partial []byte
	last    string
	value   *string
}

func (w *lastLines) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, c := range p {
		// A progress meter redraws its line with \r: each redraw is a line.
		if c == '\n' || c == '\r' {
			w.line(string(w.partial))
			w.partial = w.partial[:0]
			continue
		}
		if len(w.partial) < 4096 {
			w.partial = append(w.partial, c)
		}
	}
	return len(p), nil
}

func (w *lastLines) line(s string) {
	s = strings.TrimRight(s, "\r")
	if v, ok := strings.CutPrefix(s, "VALUE:"); ok {
		v = strings.TrimSpace(v)
		w.value = &v
		return
	}
	if strings.TrimSpace(cleanText(s, maxText)) != "" {
		w.last = s
	}
}

func (w *lastLines) result() (string, *string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.partial) > 0 {
		w.line(string(w.partial))
		w.partial = nil
	}
	return w.last, w.value
}
