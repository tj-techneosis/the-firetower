"""A watch's history (its changes of status, kept on each write) and who
feeds a watch (--source, which run fills in under Crontinental)."""

import json

from _binary import Firetower, at


class History(Firetower):
    def history(self, name, *args):
        return json.loads(self.ft("history", name, "--json", *args).stdout)

    def test_each_change_of_status_is_kept_in_order(self):
        self.ft("set", "a/b/x", "green", "fine")
        self.clock = at(5)
        self.ft("set", "a/b/x", "green", "still fine")  # same status: not a move
        self.clock = at(10)
        self.ft("set", "a/b/x", "red", "down", "--value", "500")
        self.clock = at(20)
        self.ft("set", "a/b/x", "green", "back")
        self.clock = at(30)
        self.ft("done", "a/b/x")
        moves = self.history("a/b/x")
        self.assertEqual([m["status"] for m in moves], ["green", "red", "green", "done"])
        self.assertEqual((moves[1]["message"], moves[1]["value"]), ("down", "500"))
        self.assertEqual(moves[0]["message"], "fine")

    def test_since_and_the_human_view(self):
        self.ft("set", "x", "green", "fine")
        self.clock = at(60 * 24 * 3)
        self.ft("set", "x", "yellow", "slow")
        self.assertEqual([m["status"] for m in self.history("x", "--since", "1d")], ["yellow"])
        out = self.ft("history", "x").stdout
        self.assertIn("STATUS", out.splitlines()[0])
        self.assertIn("slow", out)
        self.ft("history", "x", "--since", "soon", ok=False)
        self.ft("history", "nope", ok=False)

    def test_it_is_bounded(self):
        for i in range(60):
            self.clock = at(i)
            self.ft("set", "x", "green" if i % 2 else "red", f"n{i}")
        moves = self.history("x")
        self.assertEqual(len(moves), 50)
        self.assertEqual(moves[-1]["message"], "n59")

    def test_a_checks_own_file_cant_plant_a_history(self):
        self.ft("set", "x", "green", "fine")
        f = self.dir / "main/main/x.json"
        data = json.loads(f.read_text())
        data["history"] = [{"at": "2026-10-31T11:00:00Z", "status": "lies", "message": "x"},
                           {"at": "2026-10-31T11:30:00Z", "status": "red", "message": "a\nb\u202e"}] + data["history"]
        f.write_text(json.dumps(data))
        moves = self.history("x")
        self.assertEqual([(m["status"], m["message"]) for m in moves], [("red", "a b"), ("green", "fine")])

    def test_ls_and_the_feed_stay_light(self):
        self.ft("set", "x", "green")
        self.ft("set", "x", "red")
        self.assertNotIn("history", self.one("main/main/x"))

    def test_history_of_another_machines_lookout_says_where(self):
        p = self.ft("history", "work-mac:a/b/c", ok=False)
        self.assertIn("machine it's on", p.stderr)


class Source(Firetower):
    def test_run_names_its_crontinental_job(self):
        self.ft("run", "a/b/x", "--", "true", CRONTINENTAL_JOB="box-health", CRONTINENTAL_RUN="r1", CRONTINENTAL_NEXT="2026-10-31T12:30:00Z")
        self.assertEqual(self.one("a/b/x")["source"], "crontinental:box-health")
        self.ft("run", "a/b/y", "--source", "mine", "--", "true", CRONTINENTAL_JOB="box-health", CRONTINENTAL_RUN="r1")
        self.assertEqual(self.one("a/b/y")["source"], "mine")  # a flag wins
        self.ft("run", "a/b/z", "--", "true")
        self.assertNotIn("source", self.one("a/b/z"))

    def test_set_and_expect_take_it(self):
        self.ft("expect", "a/b/x", "--source", "cron:nightly")
        self.assertEqual(self.one("a/b/x")["source"], "cron:nightly")
        self.ft("set", "a/b/x", "green")
        self.assertEqual(self.one("a/b/x")["source"], "cron:nightly")  # kept when left out


class HistoryEdges(Firetower):
    def history(self, name, *args):
        return json.loads(self.ft("history", name, "--json", *args).stdout)

    def file(self, name="main/main/x"):
        return self.dir / (name + ".json")

    def test_a_watch_from_before_history_starts_where_it_is(self):
        self.ft("set", "x", "red", "down")
        data = json.loads(self.file().read_text())
        del data["history"]  # as 0.3 wrote it
        self.file().write_text(json.dumps(data))
        self.assertEqual([(m["status"], m["message"]) for m in self.history("x")], [("red", "down")])
        self.clock = at(10)
        self.ft("set", "x", "green", "back")
        self.assertEqual([m["status"] for m in self.history("x")], ["red", "green"])

    def test_a_wrong_shaped_history_loses_nothing(self):
        for junk in ('"oops"', '{"a": 1}', '[1, "x", {"at": "nope"}]', 'null'):
            with self.subTest(junk=junk):
                self.ft("set", "x", "red", "down", "--label", "Keep me")
                data = json.loads(self.file().read_text())
                data["history"] = json.loads(junk)
                self.file().write_text(json.dumps(data))
                v = self.one("main/main/x")  # still readable
                self.assertEqual((v["state"], v["label"]), ("red", "Keep me"))
                self.ft("set", "x", "red", "still down")  # and the next report keeps it
                self.assertEqual(self.one("main/main/x")["label"], "Keep me")
                self.ft("rm", "x")

    def test_a_planted_future_move_is_brought_back_to_now(self):
        self.ft("set", "x", "green", "fine")
        data = json.loads(self.file().read_text())
        data["history"].append({"at": "2099-01-01T00:00:00Z", "status": "red", "message": "from the future"})
        self.file().write_text(json.dumps(data))
        moves = self.history("x")
        self.assertEqual(moves[-1]["at"], "2026-10-31T12:00:00Z")  # never ahead of now
        self.clock = at(5)
        self.ft("set", "x", "green", "after")
        self.assertEqual(self.history("x")[-1]["message"], "after")  # and never ahead of what comes next

    def test_standing_down_after_until_is_dated_at_until(self):
        self.ft("set", "x", "green", "fine", "--until", at(30))
        self.clock = at(60)
        self.ft("done", "x")
        moves = self.history("x")
        self.assertEqual((moves[-1]["status"], moves[-1]["at"]), ("done", "2026-10-31T12:30:00Z"))

    def test_set_done_and_done_agree(self):
        self.ft("set", "a/b/x", "green", "fine", "--until", at(30))
        self.ft("set", "a/b/y", "green", "fine", "--until", at(30))
        self.clock = at(60)
        self.ft("done", "a/b/x")
        self.ft("set", "a/b/y", "done", "over")
        x, y = self.one("a/b/x"), self.one("a/b/y")
        self.assertEqual(x["done_at"], y["done_at"])
        self.assertEqual(self.history("a/b/x")[-1]["at"], self.history("a/b/y")[-1]["at"])

    def test_booking_again_is_in_the_history(self):
        self.ft("set", "x", "green", "fine", "--until", at(30))
        self.clock = at(60)
        self.ft("expect", "x", "--next", "1h")
        self.clock = at(70)
        self.ft("set", "x", "green", "fine again")
        self.assertEqual([(m["status"], m["message"]) for m in self.history("x")],
                         [("green", "fine"), ("done", "its --until passed"), ("waiting", "booked again"), ("green", "fine again")])

    def test_a_report_to_a_stood_down_watch_adds_nothing(self):
        self.ft("set", "x", "green", "fine")
        self.ft("done", "x")
        self.ft("set", "x", "red", "a job still booked")
        self.ft("done", "x")  # done on done
        self.assertEqual([m["status"] for m in self.history("x")], ["green", "done"])

    def test_times_are_utc(self):
        self.ft("set", "x", "green")
        data = json.loads(self.file().read_text())
        data["history"][0]["at"] = "2026-10-31T13:00:00+01:00"
        self.file().write_text(json.dumps(data))
        self.assertEqual(self.history("x")[0]["at"], "2026-10-31T12:00:00Z")

    def test_an_unreadable_file_says_why(self):
        self.file().parent.mkdir(parents=True)
        self.file().write_text("{nope")
        p = self.ft("history", "main/main/x", ok=False)
        self.assertIn("can't read", p.stderr)
        self.assertIn("firetower rm", p.stderr)


class SourceEdges(Firetower):
    def test_an_exported_job_name_alone_isnt_a_run(self):
        self.ft("run", "a/b/x", "--", "true", CRONTINENTAL_JOB="box-health")
        self.assertNotIn("source", self.one("a/b/x"))
        self.ft("run", "a/b/y", "--", "true", CRONTINENTAL_JOB="box-health", CRONTINENTAL_RUN="r1")
        self.assertEqual(self.one("a/b/y")["source"], "crontinental:box-health")

    def test_help_says_so(self):
        self.assertIn("crontinental:<job>", self.ft("help").stdout)


class OlderEdges(Firetower):
    def test_run_names_the_checks_own_flag(self):
        p = self.ft("run", "x", "curl", "-fsS", "https://example.com/", ok=False)
        self.assertIn("unknown flag -fsS: is it the check's own?", p.stderr)
        self.assertNotIn("message", p.stderr.splitlines()[0])  # not the advice about messages

    def test_capital_units_are_named(self):
        p = self.ft("set", "x", "green", "--next", "30M", ok=False)
        self.assertIn("units are lowercase: 30m", p.stderr)

    def test_expect_without_next_says_it_waits_for_good(self):
        p = self.ft("expect", "x", FIRETOWER_TERMINAL="1")
        self.assertIn("waits for its first report for good", p.stderr)
        self.assertEqual(self.ft("expect", "y", "--next", "1h", FIRETOWER_TERMINAL="1").stderr.count("for good"), 0)


class HistoryEdges2(Firetower):
    def history(self, name, *args):
        return json.loads(self.ft("history", name, "--json", *args).stdout)

    def file(self, name="main/main/x"):
        return self.dir / (name + ".json")

    def test_a_watch_over_by_until_records_its_end_not_live_moves(self):
        self.ft("set", "x", "green", "fine", "--until", at(30))
        self.clock = at(60)
        self.ft("set", "x", "red", "a job still booked")
        moves = self.history("x")
        self.assertEqual([m["status"] for m in moves], ["green", "done"])
        self.assertEqual(moves[-1]["at"], "2026-10-31T12:30:00Z")
        self.assertEqual(self.one("main/main/x")["state"], "done")

    def test_an_end_already_past_at_the_first_report(self):
        self.clock = at(60)
        self.ft("set", "x", "green", "late", "--until", at(30))
        self.assertEqual(self.one("main/main/x")["state"], "done")
        self.assertEqual([m["status"] for m in self.history("x")], ["done"])

    def test_a_clock_that_steps_back_loses_nothing(self):
        self.clock = at(60)
        self.ft("set", "x", "green", "one")
        self.clock = at(0)  # the clock steps back an hour
        self.ft("set", "x", "red", "two")
        self.assertEqual([m["message"] for m in self.history("x")], ["one", "two"])

    def test_a_move_carries_the_value_the_lookout_keeps(self):
        self.ft("set", "x", "green", "fine", "--value", "a1b2")
        self.clock = at(5)
        self.ft("set", "x", "red", "broke")
        self.assertEqual(self.history("x")[-1]["value"], "a1b2")
        self.assertEqual(self.one("main/main/x")["value"], "a1b2")

    def test_run_keeps_a_source_booked_with_expect(self):
        self.ft("expect", "x", "--next", "1h", "--source", "cron:nightly")
        self.ft("run", "x", "--", "true", CRONTINENTAL_JOB="box-health", CRONTINENTAL_RUN="r1")
        self.assertEqual(self.one("main/main/x")["source"], "cron:nightly")

    def test_run_records_one_move_with_its_message(self):
        self.ft("run", "x", "--message", "all good", "--", "true")
        self.clock = at(5)
        self.ft("run", "x", "--", "sh", "-c", "echo boom; exit 2", ok=False)
        self.clock = at(10)
        self.ft("run", "x", "--", "sh", "-c", "exit 2", ok=False)
        moves = self.history("x")
        self.assertEqual([(m["status"], m["message"]) for m in moves], [("green", "all good"), ("red", "boom")])  # exit 2 is fire, with no code added

    def test_expect_on_a_live_watch_adds_no_move(self):
        self.ft("set", "x", "green", "fine")
        self.ft("expect", "x", "--next", "1h")
        self.assertEqual([m["status"] for m in self.history("x")], ["green"])

    def test_writes_by_an_older_version_are_caught_up(self):
        self.ft("set", "x", "green", "fine")
        data = json.loads(self.file().read_text())
        data.update(status="red", message="written by 0.3", status_since="2026-10-31T12:20:00Z")
        self.file().write_text(json.dumps(data))
        self.clock = at(30)
        self.ft("set", "x", "green", "back")
        self.assertEqual([(m["status"], m["message"]) for m in self.history("x")],
                         [("green", "fine"), ("red", "written by 0.3"), ("green", "back")])

    def test_a_value_with_no_message_reads_cleanly(self):
        self.ft("set", "x", "green", "--value", "42")
        import re
        line = self.ft("history", "x").stdout.splitlines()[1]
        self.assertEqual(re.split(r"\s{2,}", line.strip())[-1], "(42)", line)

    def test_a_refused_report_lands_nothing(self):
        self.ft("set", "x", "green", "fine", "--label", "Original")
        self.ft("done", "x")
        self.ft("set", "x", "red", "still booked", "--label", "Changed", "--link", "https://example.com/")
        v = self.one("main/main/x")
        self.assertEqual((v["label"], v.get("link")), ("Original", None))

    def test_the_expect_note_is_only_for_a_watch_that_waits_for_good(self):
        self.ft("expect", "x", "--next", "1h")
        p = self.ft("expect", "x", FIRETOWER_TERMINAL="1")  # re-booked, its promise kept
        self.assertNotIn("for good", p.stderr)
        self.ft("set", "y", "green")
        self.assertNotIn("for good", self.ft("expect", "y", FIRETOWER_TERMINAL="1").stderr)

    def test_run_with_a_checks_dash_m_without_dashes(self):
        p = self.ft("run", "x", "curl", "-m", "5", "https://example.com/", ok=False)
        self.assertIn("is it the check's own?", p.stderr)


class UntilEdges(Firetower):
    def history(self, name, *args):
        return json.loads(self.ft("history", name, "--json", *args).stdout)

    def test_rebooking_an_until_ended_watch_with_a_new_until_keeps_its_end(self):
        self.ft("set", "x", "green", "fine", "--until", at(30))
        self.clock = at(60)
        self.ft("expect", "x", "--next", "1h", "--until", at(600))
        self.assertEqual([(m["status"], m["message"]) for m in self.history("x")],
                         [("green", "fine"), ("done", "its --until passed"), ("waiting", "booked again")])

    def test_a_report_with_a_later_until_doesnt_revive_an_ended_watch(self):
        self.ft("set", "x", "green", "fine", "--until", at(30))
        self.clock = at(60)
        self.ft("set", "x", "red", "booked job", "--until", at(600))
        self.ft("run", "x", "--until", at(600), "--", "true")
        v = self.one("main/main/x")
        self.assertEqual((v["state"], v["done_by"]), ("done", "until"))
        self.assertEqual([m["status"] for m in self.history("x")], ["green", "done"])

    def test_rebooking_a_watch_stood_down_by_hand(self):
        self.ft("set", "x", "green", "fine")
        self.clock = at(10)
        self.ft("done", "x", "over")
        self.clock = at(20)
        self.ft("expect", "x", "--next", "1h")
        self.clock = at(30)
        self.ft("set", "x", "green", "again")
        self.assertEqual([m["status"] for m in self.history("x")], ["green", "done", "waiting", "green"])

    def test_the_table_has_times_as_well_as_ages(self):
        self.ft("set", "x", "green", "fine")
        line = self.ft("history", "x").stdout.splitlines()[1]
        self.assertRegex(line, r"^[A-Z][a-z]{2} \d{1,2} \d{2}:\d{2} \(")


class SmallFixes(Firetower):
    def test_a_second_seen_keeps_the_first(self):
        self.ft("set", "x", "red", "down")
        self.ft("seen", "x")
        first = self.one("main/main/x")["seen_at"]
        self.clock = at(10)
        p = self.ft("seen", "x", FIRETOWER_TERMINAL="1")
        self.assertIn("already seen", p.stderr)
        self.assertEqual(self.one("main/main/x")["seen_at"], first)

    def test_run_hints_show_a_quiet_curl(self):
        p = self.ft("run", "x", "curl", "-fsS", "https://example.com/", ok=False)
        self.assertIn("curl -fsS -o /dev/null", p.stderr)
