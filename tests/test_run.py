"""run (a check that knows nothing about the Firetower) and the feed."""

import json
import os
import signal
import subprocess
import time

from _binary import BIN, Firetower, at


class Run(Firetower):
    def run_check(self, script, *flags, ok=None, **env):
        return self.ft("run", "chk", *flags, "--", "sh", "-c", script, ok=ok, **env)

    def test_exit_codes(self):
        # 0 clear, 1 smoke, 2 fire, anything else fire. No code stands a
        # watch down: curl exits 3 on a bad URL, and stood down sticks.
        for code, state in ((0, "green"), (1, "yellow"), (2, "red"), (3, "red"), (7, "red")):
            p = self.run_check(f"echo line {code}; exit {code}")
            self.assertEqual(p.returncode, code)  # passed through, for the scheduler
            v = self.one("main/main/chk")
            self.assertEqual(v["state"], state, code)
            self.assertTrue(v["message"].startswith(f"line {code}"))
            if code in (3, 7):
                self.assertIn(f"(exit {code})", v["message"])

    def test_runs_own_mistakes_exit_64(self):
        # 1 and 2 are smoke and fire under run; a typo'd booking mustn't look like either.
        for args in (("run", "chk", "--nope", "--", "true"), ("run", "chk", "true"), ("run", "BAD", "--", "true")):
            self.assertEqual(self.ft(*args, ok=False).returncode, 64, args)

    def test_the_exit_code_survives_a_long_line(self):
        self.run_check("printf '%0300d\\n' 0; exit 22")
        self.assertTrue(self.one("main/main/chk")["message"].endswith("(exit 22)"))

    def test_progress_meters_dont_garble_the_message(self):
        self.run_check("printf '10%%\\r50%%\\r100%%\\rdone downloading'")
        self.assertEqual(self.one("main/main/chk")["message"], "done downloading")

    def test_m_in_run_says_message(self):
        p = self.ft("run", "x", "-m", "hi", "--", "true", ok=False)
        self.assertIn("--message", p.stderr)
        self.assertNotIn("check's own flag", p.stderr)
        p = self.ft("run", "x", "--nope", "--", "true", ok=False)
        self.assertNotIn("check's own flag", p.stderr)  # the check is already after --

    def test_a_shell_line_gets_a_hint(self):
        p = self.ft("run", "chk", "--", "curl -fsS https://example.com/ | head", ok=False)
        self.assertIn("sh -c", p.stderr)

    def test_stopping_by_hand_says_what_it_reported(self):
        env = self.env()
        env.pop("FIRETOWER_NOW")
        p = subprocess.Popen([BIN, "run", "chk", "--", "sleep", "30"], env=env,
                             stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True)
        time.sleep(0.5)
        p.send_signal(signal.SIGINT)
        _, err = p.communicate(timeout=15)
        self.assertIn("reported main/main/chk as fire", err)

    def test_last_line_and_value(self):
        p = self.run_check("echo first; echo VALUE: abc123; echo; echo 'the last word'; echo")
        self.assertIn("first", p.stdout)  # output passes through
        v = self.one("main/main/chk")
        self.assertEqual((v["message"], v["value"]), ("the last word", "abc123"))
        # Stderr counts too (one stream at a time: the order of lines across
        # two pipes isn't knowable).
        p = self.run_check("echo 'it broke' >&2; exit 2")
        self.assertIn("it broke", p.stderr)
        self.assertEqual(self.one("main/main/chk")["message"], "it broke")

    def test_no_output(self):
        self.run_check("true")
        self.assertEqual(self.one("main/main/chk")["message"], "Ran clean.")
        self.run_check("exit 1")
        self.assertEqual(self.one("main/main/chk")["message"], "Exited 1.")

    def test_message_flag_overrides_output(self):
        # For checks whose output could carry something private.
        self.run_check("echo token=s3cret; exit 1", "--message", "Health check failed")
        self.assertEqual(self.one("main/main/chk")["message"], "Health check failed")

    def test_missing_command_is_fire(self):
        p = self.ft("run", "chk", "--", "/no/such/check", ok=False)
        self.assertEqual(p.returncode, 127)
        v = self.one("main/main/chk")
        self.assertEqual(v["state"], "red")
        self.assertIn("Can't run the check", v["message"])

    def test_not_executable_is_fire(self):
        f = self.tmp / "check.sh"
        f.write_text("#!/bin/sh\necho hi\n")
        self.ft("run", "chk", "--", str(f), ok=False)
        self.assertEqual(self.one("main/main/chk")["state"], "red")

    def test_flags_checked_before_running(self):
        marker = self.tmp / "ran"
        self.ft("run", "chk", "--nope", "--", "touch", str(marker), ok=False)
        self.assertFalse(marker.exists())
        self.ft("run", "chk", "--value", "x", "--", "true", ok=False)
        p = self.ft("run", "chk", "true", ok=False)  # no --
        self.assertIn("put the check after --", p.stderr)
        p = self.ft("run", "chk", "curl", "--max-time", "20", ok=False)
        self.assertIn("put the check after --", p.stderr.lower())

    def test_stopped_reports_fire_and_kills_the_group(self):
        pidfile = self.tmp / "child"
        env = self.env()
        env.pop("FIRETOWER_NOW")
        p = subprocess.Popen([BIN, "run", "chk", "--", "sh", "-c",
                              f"sleep 60 & echo $! > {pidfile}; wait"], env=env,
                             stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        for _ in range(50):
            if pidfile.exists() and pidfile.read_text().strip():
                break
            time.sleep(0.1)
        p.send_signal(signal.SIGTERM)
        self.assertEqual(p.wait(timeout=15), 128 + signal.SIGTERM)
        v = self.one("main/main/chk")
        self.assertEqual(v["state"], "red")
        self.assertIn("Stopped", v["message"])
        child = int(pidfile.read_text())
        time.sleep(0.2)
        with self.assertRaises(ProcessLookupError):
            os.kill(child, 0)

    def test_killed_from_outside(self):
        self.run_check("kill -9 $$")
        v = self.one("main/main/chk")
        self.assertEqual(v["state"], "red")
        self.assertIn("killed", v["message"])

    def test_background_leftovers_dont_hold_it_open(self):
        t = time.time()
        p = self.run_check("(sleep 20 &); echo hi", timeout=15)
        self.assertEqual(p.returncode, 0)
        self.assertLess(time.time() - t, 6)
        self.assertEqual(self.one("main/main/chk")["state"], "green")

    def test_stop_reports_even_when_something_escaped(self):
        # A child that left the process group (setsid) can't be killed with
        # it; a stopped run must report anyway, not wait on it.
        env = self.env()
        env.pop("FIRETOWER_NOW")
        p = subprocess.Popen([BIN, "run", "chk", "--", "sh", "-c",
                              "python3 -c 'import os,time; os.setsid(); time.sleep(40)' & sleep 100"],
                             env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        time.sleep(1)
        t = time.time()
        p.send_signal(signal.SIGTERM)
        self.assertEqual(p.wait(timeout=30), 128 + signal.SIGTERM)
        self.assertLess(time.time() - t, 20)
        self.assertIn("Stopped", self.one("main/main/chk")["message"])

    def test_keeps_no_time(self):
        # How long a check may run is the scheduler's business.
        self.ft("run", "chk", "--timeout", "1s", "--", "true", ok=False)

    def test_a_terminal_isnt_read(self):
        # From a terminal the check's group is in the background: reading
        # the terminal would stop it for good. It gets no terminal input.
        import pty
        master, slave = pty.openpty()
        self.addCleanup(os.close, master)
        p = subprocess.Popen([BIN, "run", "chk", "--", "sh", "-c", "read a; echo got:$a"],
                             stdin=slave, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=self.env(),
                             start_new_session=True)
        os.close(slave)
        try:
            p.communicate(timeout=10)
        except subprocess.TimeoutExpired:
            p.kill()
            self.fail("run hung reading the terminal")
        self.one("main/main/chk")

    def test_piped_input_passes_through(self):
        p = self.ft("run", "chk", "--", "sh", "-c", "read a; echo got:$a", input="hello\n")
        self.assertIn("got:hello", p.stdout)

    def test_message_flag_doesnt_hide_failures(self):
        self.ft("run", "chk", "--message", "Deploy landed.", "--", "/no/such", ok=False)
        self.assertIn("Can't run", self.one("main/main/chk")["message"])

    def test_next_and_until_from_the_scheduler(self):
        # The scheduler says when the next run is due and when the watch
        # ends; run takes them from its environment, and a flag still wins.
        # Crontinental's own format. NEXT is when the next run starts; its
        # report is due five minutes after that.
        self.run_check("true", CRONTINENTAL_NEXT="2026-10-31T12:10:00+00:00",
                       CRONTINENTAL_UNTIL="2026-10-31T17:00:00+00:00")
        f = self.file("main/main/chk")
        self.assertEqual((f["next_by"], f["until"]), ("2026-10-31T12:15:00Z", "2026-10-31T17:00:00Z"))
        self.assertEqual(self.one("main/main/chk", FIRETOWER_NOW=at(14))["state"], "green")
        self.assertEqual(self.one("main/main/chk", FIRETOWER_NOW=at(16))["state"], "stale")
        # A slow check says its own --next, which wins.
        self.run_check("true", "--next", "1h", CRONTINENTAL_NEXT="2026-10-31T12:10:00+00:00")
        self.assertEqual(self.file("main/main/chk")["next_by"], "2026-10-31T13:00:00Z")

    def test_no_next_from_the_scheduler_means_no_promise(self):
        # A one-off, or a repeat on its last run: Crontinental leaves NEXT unset.
        self.run_check("true", CRONTINENTAL_JOB="w", CRONTINENTAL_RUN="1", CRONTINENTAL_NEXT="2026-10-31T12:10:00+00:00")
        self.assertIn("next_by", self.file("main/main/chk"))
        self.run_check("true", CRONTINENTAL_JOB="w")  # a shell with JOB left exported: not a run, so the promise stands
        self.assertIn("next_by", self.file("main/main/chk"))
        self.run_check("true", CRONTINENTAL_JOB="w", CRONTINENTAL_RUN="2")  # its last run
        self.assertNotIn("next_by", self.file("main/main/chk"))
        self.assertEqual(self.one("main/main/chk", FIRETOWER_NOW=at(60 * 24))["state"], "green")

    def test_scheduler_values_are_times_only(self):
        # A span or a clock time isn't what a scheduler hands over.
        for bad in ("10m", "10:30"):
            p = self.run_check("true", CRONTINENTAL_NEXT=bad)
            self.assertIn("ignoring CRONTINENTAL_NEXT", p.stderr)
        self.run_check("true", "--next", "1h")
        self.run_check("true", CRONTINENTAL_NEXT="none")  # none clears, like unset
        self.assertNotIn("next_by", self.file("main/main/chk"))

    def test_a_next_already_past_counts_from_now(self):
        # Catching up after a sleep: the next start is in the past.
        self.run_check("true", CRONTINENTAL_NEXT="2026-10-31T11:00:00+00:00")
        self.assertEqual(self.file("main/main/chk")["next_by"], "2026-10-31T12:05:00Z")
        self.assertEqual(self.one("main/main/chk")["state"], "green")

    def test_a_bad_scheduler_value_doesnt_stop_the_report(self):
        p = self.run_check("true", CRONTINENTAL_NEXT="whenever")
        self.assertIn("ignoring CRONTINENTAL_NEXT", p.stderr)
        self.assertEqual(self.one("main/main/chk")["state"], "green")

    def test_set_ignores_the_scheduler_env(self):
        self.ft("set", "x", "green", CRONTINENTAL_NEXT="10m")
        self.assertNotIn("next_by", self.file("main/main/x"))

    def test_huge_output(self):
        p = self.run_check("yes 'spam spam spam' | head -c 20000000; echo; echo done here")
        self.assertEqual(p.returncode, 0)
        self.assertEqual(self.one("main/main/chk")["message"], "done here")

    def test_hostile_output(self):
        self.run_check("printf 'boom \\033[31mred\\033[0m\\033]0;pwned\\007\\n'; exit 2")
        self.assertEqual(self.one("main/main/chk")["message"], "boom red")


class Watch(Firetower):
    def feed(self, *args, **env):
        p = self.ft("watch", "--json", "--once", *args, **env)
        return [json.loads(l) for l in p.stdout.splitlines()]

    def test_once(self):
        self.ft("set", "a", "green")
        self.ft("set", "b", "red")
        ev = self.feed()
        self.assertEqual(ev[0]["type"], "hello")
        self.assertEqual(ev[0]["v"], 1)
        self.assertEqual([e["lookout"]["id"] for e in ev if e["type"] == "lookout"], ["main/main/a", "main/main/b"])
        self.assertEqual(ev[-1]["type"], "sync")
        self.assertEqual(ev[-1]["ids"], ["main/main/a", "main/main/b"])

    def test_empty(self):
        ev = self.feed()
        self.assertEqual([e["type"] for e in ev], ["hello", "sync"])
        self.assertEqual(ev[-1]["ids"], [])

    def test_needs_json(self):
        self.ft("watch", ok=False)

    def test_changes_nobody_writes(self):
        # A lookout going past its next_by is a change, sent without anyone writing.
        env = self.env()
        env.pop("FIRETOWER_NOW")
        subprocess.run([BIN, "set", "q", "green", "--next", "1s"], env=env, check=True)
        subprocess.run([BIN, "expect", "e", "--next", "1s"], env=env, check=True)
        subprocess.run([BIN, "set", "gone", "green"], env=env, check=True)
        w = subprocess.Popen([BIN, "watch", "--json"], env=env, stdout=subprocess.PIPE, text=True)
        self.addCleanup(lambda: (w.kill(), w.wait(), w.stdout.close()))
        states, seen_gone, removed = {}, False, False
        deadline = time.time() + 10
        while time.time() < deadline:
            line = w.stdout.readline()
            if not line:
                break
            e = json.loads(line)
            if e["type"] == "sync" and not removed:
                subprocess.run([BIN, "rm", "gone"], env=env, check=True)
                removed = True
            if e["type"] == "lookout":
                states[e["lookout"]["id"]] = e["lookout"]["state"]
            if e["type"] == "gone" and e["id"] == "main/main/gone":
                seen_gone = True
            if states.get("main/main/q") == "stale" and states.get("main/main/e") == "stale" and seen_gone:
                break
        self.assertEqual(states.get("main/main/q"), "stale")
        self.assertEqual(states.get("main/main/e"), "stale")
        self.assertTrue(seen_gone)

    def test_reader_going_away_ends_the_feed(self):
        self.ft("set", "a", "green")
        env = self.env()
        env.pop("FIRETOWER_NOW")
        w = subprocess.Popen([BIN, "watch", "--json"], env=env, stdout=subprocess.PIPE)
        w.stdout.readline()
        w.stdout.close()
        subprocess.run([BIN, "set", "a", "red"], env=env, check=True)
        self.assertIsNotNone(w.wait(timeout=10))
