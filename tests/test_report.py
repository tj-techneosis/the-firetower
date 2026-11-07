"""The report format and the CLI that writes it: set, expect, seen, done, rm,
ls, gc, and the fields the Firetower owns."""

import json
import re
import os
import stat
import subprocess
import threading

from _binary import BIN, Firetower, at


class Owned(Firetower):
    def test_value_still_then_moved(self):
        self.ft("set", "deploy", "green", "a", "--value", "1")
        self.clock = at(5)
        self.ft("set", "deploy", "green", "b", "--value", "1")
        v = self.one("main/main/deploy")
        self.assertEqual(v["movement"], "still")
        self.assertEqual(v["moved_at"], "2026-10-31T12:00:00Z")
        self.clock = at(10)
        self.ft("set", "deploy", "green", "c", "--value", "2")
        v = self.one("main/main/deploy")
        self.assertEqual(v["movement"], "moved")
        self.assertEqual(v["moved_at"], "2026-10-31T12:10:00Z")

    def test_set_without_value_keeps_it(self):
        self.ft("set", "deploy", "green", "--value", "1")
        self.clock = at(5)
        self.ft("set", "deploy", "green")
        v = self.one("main/main/deploy")
        self.assertEqual((v["value"], v["movement"]), ("1", "still"))

    def test_last_clear_and_status_since(self):
        self.ft("set", "site", "green", "up")
        self.clock = at(5)
        self.ft("set", "site", "red", "down")
        self.clock = at(10)
        self.ft("set", "site", "red", "still down")
        v = self.one("main/main/site")
        self.assertEqual(v["last_clear_at"], "2026-10-31T12:00:00Z")
        self.assertEqual(v["status_since"], "2026-10-31T12:05:00Z")
        self.clock = at(15)
        self.ft("set", "site", "green", "back")
        v = self.one("main/main/site")
        self.assertEqual(v["last_clear_at"], "2026-10-31T12:15:00Z")
        self.assertEqual(v["status_since"], "2026-10-31T12:15:00Z")

    def test_checks_cant_write_owned_fields(self):
        # The only way in is the CLI, and it has no flag for them; a field a
        # check sneaks into the file by hand is overwritten on the next report.
        self.ft("set", "x", "red", "--value", "1")
        p = self.dir / "main/main/x.json"
        d = json.loads(p.read_text())
        d["last_clear_at"] = "2026-10-31T11:00:00Z"
        p.write_text(json.dumps(d))
        self.ft("set", "x", "red", "--value", "1")
        self.assertEqual(self.one("main/main/x")["last_clear_at"], "2026-10-31T11:00:00Z")  # kept, not invented
        self.ft("set", "x", "green")
        self.assertEqual(self.one("main/main/x")["last_clear_at"], "2026-10-31T12:00:00Z")

    def test_flags_left_out_keep_their_fields(self):
        self.ft("set", "x", "green", "--label", "The X", "--link", "https://example.com", "--source", "cron")
        self.ft("set", "x", "yellow", "hmm")
        f = self.file("main/main/x")
        self.assertEqual((f["label"], f["link"], f["source"]), ("The X", "https://example.com", "cron"))
        self.ft("set", "x", "yellow", "--link", "")
        f = self.file("main/main/x")
        self.assertNotIn("link", f)

    def test_unknown_fields_are_kept(self):
        self.ft("set", "x", "green")
        p = self.dir / "main/main/x.json"
        d = json.loads(p.read_text())
        d["future_thing"] = {"a": 1}
        p.write_text(json.dumps(d))
        self.ft("set", "x", "red")
        self.assertEqual(self.file("main/main/x")["future_thing"], {"a": 1})


class Seen(Firetower):
    def test_seen_holds_through_message_changes(self):
        self.ft("set", "x", "yellow", "slow at 4:57 pm", "--value", "slow")
        self.ft("seen", "x")
        self.assertTrue(self.one("main/main/x")["seen"])
        self.clock = at(5)
        self.ft("set", "x", "yellow", "slow at 5:02 pm", "--value", "slow")
        self.assertTrue(self.one("main/main/x")["seen"])

    def test_seen_ends_on_status_or_value(self):
        self.ft("set", "x", "yellow", "--value", "slow")
        self.ft("seen", "x")
        self.ft("set", "x", "yellow", "--value", "slower")
        self.assertFalse(self.one("main/main/x")["seen"])
        self.ft("seen", "x")
        self.ft("set", "x", "red", "--value", "slower")
        self.assertFalse(self.one("main/main/x")["seen"])

    def test_same_trouble_coming_back_breaks_through(self):
        self.ft("set", "x", "yellow", "--value", "slow")
        self.ft("seen", "x")
        self.ft("set", "x", "green", "--value", "ok")
        self.ft("set", "x", "yellow", "--value", "slow")
        self.assertFalse(self.one("main/main/x")["seen"])

    def test_nothing_to_see(self):
        self.ft("set", "x", "green")
        self.assertIn("clear", self.ft("seen", "x", ok=False).stderr)
        self.ft("seen", "nope", ok=False)


class Silence(Firetower):
    """No word: the report said when the next one was due, and it didn't
    come. The Firetower keeps no clock; readers compare next_by with now."""

    def test_no_word_past_next_by(self):
        self.ft("set", "x", "green", "--next", "10m")
        self.assertEqual(self.file("main/main/x")["next_by"], "2026-10-31T12:10:00Z")
        self.assertEqual(self.one("main/main/x", FIRETOWER_NOW=at(10))["state"], "green")
        v = self.one("main/main/x", FIRETOWER_NOW=at(10.5))
        self.assertEqual((v["state"], v["stale_since"], v["status"]), ("stale", "2026-10-31T12:10:00Z", "green"))

    def test_each_report_renews_it(self):
        self.ft("set", "x", "green", "--next", "10m")
        self.clock = at(9)
        self.ft("set", "x", "green", "--next", "10m")
        self.assertEqual(self.one("main/main/x", FIRETOWER_NOW=at(15))["state"], "green")

    def test_a_time_works_too(self):
        self.ft("set", "x", "green", "--next", "2026-10-31T13:00:00Z")
        self.assertEqual(self.one("main/main/x", FIRETOWER_NOW=at(61))["state"], "stale")

    def test_a_report_without_next_keeps_the_promise(self):
        # The deadline is the schedule's: a report by hand in between doesn't move it.
        self.ft("set", "x", "green", "--next", "10m")
        self.clock = at(5)
        self.ft("set", "x", "yellow", "checked by hand")
        self.assertEqual(self.one("main/main/x", FIRETOWER_NOW=at(11))["state"], "stale")
        self.ft("set", "x", "green", "--next", "none")
        self.assertEqual(self.one("main/main/x", FIRETOWER_NOW=at(60 * 24 * 30))["state"], "green")

    def test_no_next_is_never_no_word(self):
        self.ft("set", "x", "green")
        self.assertEqual(self.one("main/main/x", FIRETOWER_NOW=at(60 * 24 * 30))["state"], "green")

    def test_a_failing_check_that_reports_keeps_its_age(self):
        # Fresh is not healthy: red stays red, with since when and the last clear.
        self.ft("set", "x", "green", "--next", "10m")
        for m in (10, 20, 30):
            self.clock = at(m)
            self.ft("set", "x", "red", "still failing", "--next", "10m")
        v = self.one("main/main/x")
        self.assertEqual((v["state"], v["status_since"], v["last_clear_at"]),
                         ("red", "2026-10-31T12:10:00Z", "2026-10-31T12:00:00Z"))

    def test_expect_waits_then_no_word_yet(self):
        self.ft("expect", "nightly", "--next", "1h", "--label", "Nightly")
        self.assertEqual(self.one("main/main/nightly", FIRETOWER_NOW=at(59))["state"], "waiting")
        self.assertEqual(self.one("main/main/nightly", FIRETOWER_NOW=at(61))["state"], "stale")
        self.assertIn("no word yet", self.ft("ls", FIRETOWER_NOW=at(61)).stdout)
        self.clock = at(62)
        self.ft("set", "nightly", "green", "--next", "1h")
        self.assertEqual(self.one("main/main/nightly")["state"], "green")

    def test_expect_without_next_just_waits(self):
        self.ft("expect", "n")
        self.assertEqual(self.one("main/main/n", FIRETOWER_NOW=at(60 * 24))["state"], "waiting")

    def test_no_clock_flags(self):
        # The Firetower times nothing: no intervals, no timeouts.
        for flag in ("--every", "--timeout"):
            self.ft("set", "x", "green", flag, "1h", ok=False)
            self.ft("expect", "x", flag, "1h", ok=False)
        self.ft("set", "x", "green", "--next", "soon", ok=False)
        self.ft("set", "x", "green", "--until", "whenever", ok=False)


class StandDown(Firetower):
    def test_until_stands_down_without_anyone_writing(self):
        self.ft("set", "x", "green", "--next", "2h", "--until", "1h")
        self.assertEqual(self.one("main/main/x", FIRETOWER_NOW=at(59))["state"], "green")
        v = self.one("main/main/x", FIRETOWER_NOW=at(61))
        self.assertEqual((v["state"], v["done_at"]), ("done", "2026-10-31T13:00:00Z"))

    def test_a_report_after_until_stays_stood_down(self):
        self.ft("set", "x", "green", "--until", "1h")
        self.clock = at(90)
        self.ft("set", "x", "red", "late", "--until", "1h")  # the same booking, still over
        self.assertEqual(self.one("main/main/x")["state"], "done")
        self.ft("expect", "x", "--until", "1h")  # booked again
        self.ft("set", "x", "red", "late", "--until", "1h")
        self.assertEqual(self.one("main/main/x")["state"], "red")

    def test_a_relative_until_doesnt_slide(self):
        for m in (0, 170, 350):
            self.clock = at(m)
            self.ft("set", "x", "green", "--next", "3h", "--until", "6h")
        self.assertEqual(self.one("main/main/x", FIRETOWER_NOW=at(365))["state"], "done")
        self.clock = at(370)
        # Once it's over, a report's own time doesn't bring it back (stood
        # down sticks, however it happened); booking it again does.
        self.ft("set", "x", "green", "--until", "2026-10-31T20:00:00Z")
        self.assertEqual(self.one("main/main/x")["state"], "done")
        self.ft("expect", "x", "--until", "2026-10-31T20:00:00Z")
        self.ft("set", "x", "green")
        self.assertEqual(self.one("main/main/x")["state"], "green")

    def test_a_later_time_moves_the_end_of_a_watch_still_on(self):
        self.ft("set", "x", "green", "--until", "6h")
        self.clock = at(60)
        self.ft("set", "x", "green", "--until", "2026-10-31T20:00:00Z")  # a time always applies, while it's on
        self.assertEqual(self.one("main/main/x", FIRETOWER_NOW=at(420))["state"], "green")

    def test_a_watch_past_until_doesnt_come_back(self):
        # A job nobody unbooked keeps reporting: tidying up must not delete
        # the lookout and let the next report start it over.
        self.ft("set", "w", "green", "--until", "1h")
        for m in (60 * 26, 60 * 27.5, 60 * 52):
            self.clock = at(m)
            self.ft("set", "w", "green", "--until", "1h")
            self.assertTrue((self.dir / "main/main/w.json").exists(), m)
        all_ = {v["id"]: v for v in json.loads(self.ft("ls", "--json", "--all").stdout)}
        self.assertEqual(all_["main/main/w"]["state"], "done")
        self.ft("gc", FIRETOWER_NOW=at(60 * 77))  # quiet for a day: it goes
        self.assertFalse((self.dir / "main/main/w.json").exists())

    def test_stood_down_sticks_until_booked_again(self):
        # A job still booked can't undo the board's Stand down.
        self.ft("set", "d", "green", "--next", "10m")
        self.ft("done", "d", "Landed.")
        self.clock = at(10)
        p = self.ft("set", "d", "green", "fine", FIRETOWER_TERMINAL="1")
        self.assertIn("stood down", p.stderr)
        self.assertNotIn("stood down", self.ft("set", "d", "green", "fine").stderr)  # not into a scheduler's log
        v = self.one("main/main/d")
        self.assertEqual((v["state"], v["message"], v["done_at"]), ("done", "Landed.", "2026-10-31T12:00:00Z"))
        self.ft("expect", "d", "--next", "10m")  # booked again
        self.assertEqual(self.one("main/main/d")["state"], "waiting")
        self.ft("set", "d", "green", "fine")
        self.assertEqual(self.one("main/main/d")["state"], "green")

    def test_expect_rebooks_an_old_watch_without_losing_it(self):
        # It once deleted it in the same write that re-booked it.
        self.ft("set", "g", "green")
        self.ft("done", "g")
        self.clock = at(60 * 72)
        self.ft("expect", "g", "--next", "30m", "--until", "6h")
        v = self.one("main/main/g")
        self.assertEqual(v["state"], "waiting")
        self.ft("set", "u", "green", "--until", "1h")
        self.clock = at(60 * 100)
        self.ft("expect", "u", "--next", "30m")  # its until long gone, no new one
        self.assertEqual(self.one("main/main/u")["state"], "waiting")

    def test_a_clock_until_doesnt_slide(self):
        self.ft("set", "a", "green", "--until", "13:00", TZ="UTC")
        self.clock = at(61)
        self.ft("set", "a", "green", "--until", "13:00", TZ="UTC")  # the same booked line, after it
        self.assertEqual(self.one("main/main/a", TZ="UTC")["state"], "done")

    def test_huge_spans_are_refused(self):
        for span in ("106752d", "200000d", "99999999999999999999d", "1000000h"):
            self.ft("set", "x", "green", "--next", span, ok=False)

    def test_done_then_hidden_after_a_day_then_gc(self):
        self.ft("set", "x", "green")
        self.ft("done", "x", "Landed.")
        v = self.one("main/main/x")
        self.assertEqual((v["state"], v["message"]), ("done", "Landed."))
        self.assertNotIn("main/main/x", self.ls(FIRETOWER_NOW=at(60 * 25)))
        self.assertTrue((self.dir / "main/main/x.json").exists())
        self.ft("gc", FIRETOWER_NOW=at(60 * 25))
        self.assertFalse((self.dir / "main").exists())  # towers and districts go with their last lookout

    def test_writes_tidy_up_by_themselves(self):
        self.ft("set", "old", "green")
        self.ft("done", "old")
        self.clock = at(60 * 25)
        self.ft("set", "new", "green")
        self.assertFalse((self.dir / "main/main/old.json").exists())


class Places(Firetower):
    def test_defaults_env_and_config(self):
        self.ft("set", "a", "green")
        self.ft("set", "b", "green", FIRETOWER_DISTRICT="app", FIRETOWER_TOWER="signup")
        self.config.write_text('default_district = "site"\ndefault_tower = "admin"\n')
        self.ft("set", "c", "green")
        self.assertEqual(set(self.ls()), {"main/main/a", "app/signup/b", "site/admin/c"})

    def test_bare_name_finds_the_one_lookout(self):
        self.ft("set", "deploy", "green", "--district", "website", "--tower", "checkout")
        self.ft("set", "deploy", "red", "now")
        self.ft("done", "deploy")
        self.assertEqual(set(self.ls()), {"website/checkout/deploy"})

    def test_ambiguous_names_must_say_where(self):
        self.ft("set", "checkout", "green", "--district", "app", "--tower", "pay")
        self.ft("set", "checkout", "green", "--district", "website", "--tower", "pay")
        p = self.ft("seen", "checkout", ok=False)
        self.assertIn("more than one tower", p.stderr)
        self.ft("set", "app/pay/checkout", "yellow")
        self.ft("seen", "app/pay/checkout")
        self.ft("set", "pay/x", "green")
        self.assertIn("main/pay/x", self.ls())

    def test_rm_lookout_tower_district(self):
        for id in ("app/a/1", "app/a/2", "app/b/1", "web/c/1"):
            self.ft("set", id, "green")
        self.ft("rm", "app/a/1")
        self.ft("rm", "--district", "app", "--tower", "a")
        self.assertEqual(set(self.ls()), {"app/b/1", "web/c/1"})
        self.ft("rm", "--district", "app")
        self.assertEqual(set(self.ls()), {"web/c/1"})
        self.assertFalse((self.dir / "app").exists())
        self.ft("rm", "nope", ok=False)


class Hostile(Firetower):
    """Red team: what a careless or hostile check can throw at it."""

    def test_names_cant_leave_the_state_dir(self):
        for bad in ("../x", "..", ".hidden", "a\\b", "x/../../y", "/etc/passwd", "UPPER", "a b", "", "x" * 65):
            self.ft("set", bad, "green", ok=False)
        for flag in ("--district", "--tower"):
            for bad in ("..", "../../tmp", ".x", "a/b"):
                self.ft("set", "ok", "green", flag, bad, ok=False)
        self.assertFalse(self.dir.exists() and any(self.dir.rglob("*.json")))

    def test_messages_are_one_clean_line(self):
        nasty = "line1\nline2\r\x1b[31mred\x1b[0m\x1b]0;title\x07 bell\x07 \u202egnp.exe\u202c tab\there"
        self.ft("set", "x", "red", nasty, "--label", "L\x1b[2J\nabel")
        v = self.one("main/main/x")
        self.assertEqual(v["message"], "line1 line2 red bell gnp.exe tab here")
        self.assertEqual(v["label"], "L abel")
        self.assertNotIn("\x1b", self.ft("ls").stdout)

    def test_messages_are_cut(self):
        self.ft("set", "x", "red", "é" * 500)
        self.assertEqual(len(self.one("main/main/x")["message"]), 200)

    def test_bad_utf8(self):
        p = subprocess.run([BIN, "set", "x", "red", b"bad \xff\xfe bytes".decode("latin-1")], env=self.env())
        self.assertEqual(p.returncode, 0)
        self.one("main/main/x")

    def test_an_unreadable_file_shows_as_fire(self):
        self.ft("set", "x", "green")
        (self.dir / "main/main/x.json").write_text("{not json")
        v = self.one("main/main/x")
        self.assertEqual((v["state"], v["broken"]), ("red", True))
        self.ft("set", "x", "green", "fixed")  # the next report replaces it
        self.assertEqual(self.one("main/main/x")["state"], "green")

    def test_strays_in_the_state_dir_are_ignored(self):
        self.ft("set", "x", "green")
        (self.dir / "main/main/notes.txt").write_text("hi")
        (self.dir / "main/main/.x.json.tmp123").write_text("{")
        (self.dir / "Weird Dir").mkdir()
        os.symlink("/etc/hosts", self.dir / "main/main/link.json")
        self.assertEqual(set(self.ls()), {"main/main/x"})

    def test_state_is_private(self):
        self.ft("set", "x", "green")
        self.assertEqual(stat.S_IMODE((self.dir / "main/main/x.json").stat().st_mode), 0o600)
        self.assertEqual(stat.S_IMODE((self.dir / "main").stat().st_mode), 0o700)

    def test_concurrent_reports_lose_nothing(self):
        self.ft("set", "x", "green", "--value", "0")
        errs = []

        def worker(i):
            p = subprocess.run([BIN, "set", "x", "red" if i % 2 else "green", f"m{i}", "--value", str(i % 3)],
                               capture_output=True, text=True, env=self.env())
            if p.returncode:
                errs.append(p.stderr)

        ts = [threading.Thread(target=worker, args=(i,)) for i in range(40)]
        [t.start() for t in ts]
        [t.join() for t in ts]
        self.assertEqual(errs, [])
        f = self.file("main/main/x")
        self.assertIn("last_clear_at", f)
        json.dumps(f)  # one whole file, never half of two

    def test_messages_that_look_like_flags(self):
        self.ft("set", "x", "red", "--label", "X", "--", "--force failed")
        self.assertEqual(self.one("main/main/x")["message"], "--force failed")
        self.ft("done", "x", "--", "-- all done")
        self.assertEqual(self.one("main/main/x")["message"], "-- all done")

    def test_status_words(self):
        self.assertIn("fire is red", self.ft("set", "x", "fire", ok=False).stderr)
        self.ft("set", "x", "purple", ok=False)

    def test_bad_flags(self):
        self.ft("set", "x", "green", "--nope", ok=False)
        self.ft("set", "x", "green", "--label", ok=False)

    def test_broken_config_doesnt_stop_a_report(self):
        self.config.write_text("this is = = not toml")
        p = self.ft("set", "x", "green")
        self.assertIn("goes to main/main", p.stderr)
        self.assertIn("main/main/x", self.ls())

    def test_a_boards_bad_setting_doesnt_move_reports(self):
        # A slip in a board-only setting keeps the file's defaults: the
        # reports land where they always did, and it's said every time.
        for slip in ('[board]\nport = 99999\n', '[board]\nhide = ["Inbox"]\n',
                     '[[source]]\nname = "x"\ncommand = ["true"]\nkeep = ["a/"]\n', 'colour = "red"\n',
                     '[board]\nport = "7480"\n', '[board]\nhide = "inbox/*"\n', '[board]\nlocal = "no"\n'):
            with self.subTest(slip=slip):
                self.config.write_text('default_district = "site"\ndefault_tower = "admin"\n' + slip)
                p = self.ft("set", "x", "green")
                self.assertIn("reports still go to its defaults", p.stderr)
                self.assertIn("site/admin/x", self.ls())
                p = self.ft("seen", "x", ok=False)  # a lookup warns too, without claiming to place anything
                self.assertIn("boards won't start", p.stderr)
                self.assertNotIn("goes to", p.stderr)
                self.ft("rm", "site/admin/x")

    def test_a_bad_default_is_lost_alone(self):
        self.config.write_text('default_district = "Site"\ndefault_tower = "admin"\n')
        p = self.ft("set", "x", "green")
        self.assertIn("goes to main/admin", p.stderr)
        self.assertIn("main/admin/x", self.ls())
        # A bad tower alone leaves the district known: rm --tower still works there.
        self.config.write_text('default_district = "site"\ndefault_tower = "bad Tower"\n')
        p = self.ft("set", "y", "green")
        self.assertIn("goes to site/main", p.stderr)
        self.ft("set", "site/b/z", "green")
        self.ft("rm", "--tower", "b")
        self.assertEqual(set(self.ls()), {"main/admin/x", "site/main/y"})

    def test_rm_a_tower_wont_guess_the_district(self):
        self.ft("set", "a/b/x", "green")
        self.ft("set", "main/b/y", "green")
        self.config.write_text("this is = = not toml")
        p = self.ft("rm", "--tower", "b", ok=False)
        self.assertIn("--district", p.stderr)
        self.assertEqual(set(self.ls()), {"a/b/x", "main/b/y"})
        self.ft("rm", "--district", "a", "--tower", "b")
        self.assertEqual(set(self.ls()), {"main/b/y"})
        # $FIRETOWER_DISTRICT names it, as for every other command.
        self.ft("set", "a/b/x", "green")
        self.ft("rm", "--tower", "b", FIRETOWER_DISTRICT="a")
        self.assertEqual(set(self.ls()), {"main/b/y"})
        # A bad board setting keeps the default district, and says so.
        self.config.write_text('default_district = "a"\n[board]\nport = 99999\n')
        self.ft("set", "a/b/z", "green")
        p = self.ft("rm", "--tower", "b")
        self.assertIn("default district, a, still holds", p.stderr)
        self.assertEqual(set(self.ls()), {"main/b/y"})

    def test_unwritable_state_dir_is_an_error_not_silence(self):
        self.dir.mkdir()
        self.dir.chmod(0o500)
        self.addCleanup(self.dir.chmod, 0o700)
        p = self.ft("set", "x", "green", ok=False)
        self.assertIn("can't write", p.stderr)


class FirstUse(Firetower):
    """What a newcomer types, and what they're told."""

    def test_errors_say_why_first(self):
        cases = [
            (("set", "x", "green", "all", "quiet"), "quote"),
            (("set", "x", "green", "-m", "hi"), "a message is a quoted word"),
            (("set", "x", "green", "-q"), "flags start with --"),
            (("run", "x", "--next", "--", "true"), "an empty variable"),
            (("set", "x", "OK"), "clear is green"),
            (("done", "x", "--message", "hi"), "only run takes it"),
            (("set", "x", "green", "--message", "hi"), "only run takes it"),
            (("set", "x"), "needs a NAME and a STATUS"),
            (("seen", "x", "--foo"), "unknown flag --foo"),
            (("done", "x", "a", "b"), "quote"),
            (("expect", "x", "--value", "1"), "no --value"),
            (("ls", "--district"), "--district needs a value"),
            (("ls", "-a"), "flags start with --"),
            (("rm",), "rm takes one NAME"),
            (("set", "x", "Green"), "lowercase: green"),
            (("set", "x", "stale"), "worked out by the board"),
        ]
        for args, want in cases:
            p = self.ft(*args, ok=False)
            self.assertIn(want, p.stderr, args)
            self.assertTrue(p.stderr.startswith("firetower:"), (args, p.stderr))

    def test_help_everywhere(self):
        for cmd in ("set", "run", "expect", "ls", "seen"):
            p = self.ft(cmd, "--help")
            self.assertIn("firetower set NAME STATUS", p.stdout)

    def test_an_empty_time_is_refused(self):
        # --next "$INTERVAL" with the variable unset must not drop the promise.
        self.ft("set", "x", "green", "--next", "", ok=False)
        self.ft("set", "x", "green", "--until=", ok=False)

    def test_a_late_report_ends_a_lapsed_promise(self):
        self.ft("set", "x", "green", "--next", "1m")
        self.clock = at(2)
        self.assertEqual(self.one("main/main/x")["state"], "stale")
        self.ft("set", "x", "green", "fixed")
        v = self.one("main/main/x")
        self.assertEqual(v["state"], "green")
        self.assertNotIn("next_by", v)

    def test_a_date_means_through_that_day(self):
        self.ft("set", "x", "green", "--until", "2026-10-31", TZ="UTC")
        self.assertEqual(self.file("main/main/x")["until"], "2026-11-01T00:00:00Z")
        self.assertEqual(self.one("main/main/x", TZ="UTC")["state"], "green")

    def test_spans_in_words(self):
        # Crontinental writes "5 hours"; a booking line shouldn't need two grammars.
        for span, want in (("5 hours", "2026-10-31T17:00:00Z"), ("90 minutes", "2026-10-31T13:30:00Z"), ("2 days", "2026-11-02T12:00:00Z")):
            self.ft("set", "x", "green", "--next", span)
            self.assertEqual(self.file("main/main/x")["next_by"], want, span)

    def test_a_report_to_a_finished_watch_says_so(self):
        self.ft("set", "x", "green", "--until", "1h")
        self.clock = at(90)
        p = self.ft("set", "x", "red", "late", FIRETOWER_TERMINAL="1")
        self.assertIn("stood down", p.stderr)

    def test_done_keeps_what_the_watch_last_said(self):
        self.ft("set", "x", "yellow", "3 orders waiting.")
        p = self.ft("done", "x")
        v = self.one("main/main/x")
        self.assertEqual((v["state"], v["message"]), ("done", "3 orders waiting."))
        p = self.ft("set", "y", "done", "Landed.")
        self.assertNotIn("stood down", p.stderr)  # a deliberate done isn't told off

    def test_more_time_forms(self):
        for when, want in (("2026-11-01T06:00:00", "2026-11-01T06:00:00Z"), ("2026-11-01 06:00:00", "2026-11-01T06:00:00Z")):
            self.ft("set", "x", "green", "--next", when, TZ="UTC")
            self.assertEqual(self.file("main/main/x")["next_by"], want)
        for span, want in (("2w", "2026-11-14T12:00:00Z"), ("6hrs", "2026-10-31T18:00:00Z"), ("6hr", "2026-10-31T18:00:00Z"), ("2D", "2026-11-02T12:00:00Z")):
            self.ft("set", "x", "green", "--next", span)
            self.assertEqual(self.file("main/main/x")["next_by"], want, span)
        self.assertIn("needs a unit", self.ft("set", "x", "green", "--next", "30", ok=False).stderr)
        self.ft("set", "x", "green", "--next", "30M", ok=False)  # M could be months: refused, not guessed

    def test_a_past_time_is_accepted(self):
        # Pointed out at a terminal only; under a scheduler it would fill a log.
        p = self.ft("set", "x", "green", "--until", "2026-10-30")
        self.assertEqual(self.one("main/main/x")["state"], "done")
        self.assertNotIn("already past", p.stderr)
        p = self.ft("set", "y", "green", "--until", "2026-10-30", FIRETOWER_TERMINAL="1")
        self.assertIn("already past", p.stderr)

    def test_standing_down_an_ended_watch_keeps_its_end(self):
        self.ft("set", "x", "green", "--until", "1h")
        self.clock = at(60 * 20)
        self.ft("done", "x")
        v = self.one("main/main/x")
        self.assertEqual(v["done_at"], "2026-10-31T13:00:00Z")
        self.assertNotIn("main/main/x", self.ls(FIRETOWER_NOW=at(60 * 26)))  # not back on the board for a day

    def test_expect_with_a_past_until_keeps_it(self):
        self.ft("set", "x", "green")
        self.ft("done", "x")
        self.ft("expect", "x", "--until", "2026-10-30", TZ="UTC")
        self.assertEqual(self.file("main/main/x")["until"], "2026-10-31T00:00:00Z")

    def test_expect_leaves_a_lapsed_promise_showing(self):
        self.ft("expect", "x", "--next", "30m")
        self.clock = at(40)
        self.ft("expect", "x", "--label", "X")
        self.assertEqual(self.one("main/main/x")["state"], "stale")

    def test_ls_notes(self):
        self.ft("expect", "w", "--next", "20m")
        self.ft("set", "u", "green", "--until", "1h")
        self.ft("set", "d", "green")
        self.ft("done", "d")
        out = self.ft("ls", FIRETOWER_NOW=at(61)).stdout
        self.assertIn("its watch ended", out)
        self.assertIn("stood down", out)
        self.assertIn("waiting for a first report", self.ft("ls").stdout)
        self.assertIn("due in 20m", self.ft("ls").stdout)
        self.assertIn("No lookouts there", self.ft("ls", "--district", "nope").stdout)
        self.ft("rm", "w")
        self.assertIn("--all", self.ft("ls", FIRETOWER_NOW=at(60 * 30)).stdout)

    def test_a_damaged_report_says_what_to_do(self):
        self.ft("set", "x", "green")
        (self.dir / "main/main/x.json").write_text("{")
        p = self.ft("seen", "x", ok=False)
        self.assertIn("firetower rm main/main/x", p.stderr)
        v = self.one("main/main/x")
        self.assertNotIn(str(self.dir), v["message"])  # no paths on a board


class Upgrade(Firetower):
    """Against a file:// copy of a releases page, as install.sh reads it.
    The suite runs on a build from the checkout and, in the release check,
    on the release binary itself: each behaves as what it is."""

    def releases(self, version):
        d = self.tmp / "rel" / "latest" / "download"
        d.mkdir(parents=True, exist_ok=True)
        (d / "VERSION").write_text(version + "\n")
        return f"file://{self.tmp / 'rel'}"

    def this(self):
        v = self.ft("version").stdout.split()[1]
        return v if re.fullmatch(r"v\d+\.\d+\.\d+(-\S+)?", v) else None

    def test_check_names_the_latest(self):
        p = self.ft("upgrade", "--check", FIRETOWER_RELEASES=self.releases("v9.9.9"), ok=None)
        self.assertIn("v9.9.9", p.stdout)
        # A release older than the latest says so, and exits 3 for a scheduled check.
        self.assertEqual(p.returncode, 3 if self.this() else 0)

    def test_check_up_to_date(self):
        if not self.this():
            self.skipTest("a build from a checkout has no release to compare")
        p = self.ft("upgrade", "--check", FIRETOWER_RELEASES=self.releases(self.this()))
        self.assertIn("is the latest release", p.stdout)

    def test_check_cant_tell(self):
        p = self.ft("upgrade", "--check", FIRETOWER_RELEASES=f"file://{self.tmp}/nowhere", ok=False)
        self.assertEqual(p.returncode, 1)
        p = self.ft("upgrade", "--check", FIRETOWER_RELEASES=self.releases("latest"), ok=False)
        self.assertIn("not a version", p.stderr)

    def test_upgrade_needs_an_installed_release(self):
        # A checkout build upgrades in its checkout; a release binary not
        # installed by install.sh is pointed at the one-liner.
        p = self.ft("upgrade", FIRETOWER_RELEASES=self.releases("v9.9.9"), ok=False)
        self.assertIn("install.sh", p.stderr)
        if not self.this():
            self.assertIn("git pull", p.stderr)


class Docs(Firetower):
    def test_readme_lists_the_help(self):
        from _binary import ROOT
        readme = (ROOT / "README.md").read_text()
        block = readme.split("### All of it", 1)[1].split("```", 2)[1]
        help_ = self.ft("help").stdout
        for line in block.splitlines():
            if line.strip():
                self.assertIn(line.strip(), help_, "README's command list differs from firetower help")


class Ls(Firetower):
    def test_human_ls(self):
        self.ft("set", "x", "red", "boom")
        out = self.ft("ls").stdout
        self.assertIn("main/main/x", out)
        self.assertIn("never clear", out)
        self.assertIn("No lookouts", self.ft("ls", "--district", "nope").stdout)

    def test_filters(self):
        self.ft("set", "app/a/1", "green")
        self.ft("set", "web/b/1", "green")
        self.assertEqual(set(self.ls()), {"app/a/1", "web/b/1"})
        vs = json.loads(self.ft("ls", "--json", "--district", "app").stdout)
        self.assertEqual([v["id"] for v in vs], ["app/a/1"])
