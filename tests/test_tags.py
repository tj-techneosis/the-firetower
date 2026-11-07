"""Tags: words a lookout carries, for grouping and for people's own scripts.
The Firetower only keeps and shows them; ls, rm and gc --tag are how a
script acts on them."""

import json
import subprocess

from _binary import BIN, Firetower, at


class Tags(Firetower):
    def tags(self, id):
        return self.one(id).get("tags", [])

    def test_set_keeps_replaces_and_clears(self):
        self.ft("set", "a/b/x", "green", "--tag", "health", "--tag", "web,health")
        self.assertEqual(self.tags("a/b/x"), ["health", "web"])  # sorted, no repeats
        self.ft("set", "a/b/x", "yellow")  # left out: kept
        self.assertEqual(self.tags("a/b/x"), ["health", "web"])
        self.ft("set", "a/b/x", "green", "--tag", "temporary")  # given: replaced
        self.assertEqual(self.tags("a/b/x"), ["temporary"])
        self.ft("set", "a/b/x", "green", "--tag", "none")
        self.assertEqual(self.tags("a/b/x"), [])
        self.assertNotIn("tags", (self.dir / "a/b/x.json").read_text())

    def test_bad_tags_are_refused(self):
        for bad in (["Health"], ["a b"], [""], ["none", "--tag", "x"], ["x,none"], [",".join(f"t{i}" for i in range(17))]):
            with self.subTest(bad=bad):
                p = self.ft("set", "x", "green", "--tag", *bad, ok=False)
                self.assertIn("tag", p.stderr)
        self.assertEqual(self.ls(), {})

    def test_run_and_expect_take_them(self):
        self.ft("expect", "a/b/x", "--next", "1h", "--tag", "health")
        self.assertEqual(self.tags("a/b/x"), ["health"])
        self.ft("run", "a/b/x", "--tag", "health,nightly", "--", "true")
        self.assertEqual(self.tags("a/b/x"), ["health", "nightly"])
        # A bad tag is a mistake in the run line: refused before the check runs.
        marker = self.tmp / "ran"
        p = self.ft("run", "a/b/y", "--tag", "Bad", "--", "touch", str(marker), ok=False)
        self.assertEqual(p.returncode, 64)
        self.assertFalse(marker.exists())

    def test_ls_filters_and_shows(self):
        self.ft("set", "a/b/x", "green", "--tag", "health,web")
        self.ft("set", "c/d/y", "red", "--tag", "health")
        self.ft("set", "c/d/z", "green")
        ids = lambda *a: sorted(json.loads(self.ft("ls", "--json", *a).stdout), key=lambda v: v["id"])
        self.assertEqual([v["id"] for v in ids("--tag", "health")], ["a/b/x", "c/d/y"])
        self.assertEqual([v["id"] for v in ids("--tag", "health", "--tag", "web")], ["a/b/x"])  # every one must match
        self.assertEqual(ids("--tag", "nope"), [])
        out = self.ft("ls").stdout
        self.assertIn("TAGS", out.splitlines()[0])
        self.assertIn("health,web", out)
        self.ft("ls", "--tag", "none", ok=False)

    def test_ls_without_tags_has_no_tags_column(self):
        self.ft("set", "x", "green")
        self.assertNotIn("TAGS", self.ft("ls").stdout)

    def test_rm_by_tag(self):
        self.ft("set", "a/b/x", "green", "--tag", "temporary")
        self.ft("set", "c/d/y", "red", "--tag", "temporary,web")
        self.ft("set", "c/e/z", "green", "--tag", "temporary")
        self.ft("set", "c/d/keep", "green", "--tag", "web")
        p = self.ft("rm", "--tag", "temporary", "--district", "c", "--tower", "d")
        self.assertIn("removed 1 lookout", p.stdout)
        self.ft("rm", "--tag", "temporary")
        self.assertEqual(set(self.ls()), {"c/d/keep"})
        self.ft("rm", "x", "--tag", "web", ok=False)  # a NAME and --tag: which?

    def test_gc_by_tag_forgets_only_whats_over(self):
        self.ft("set", "a/b/over", "green", "--tag", "temporary")
        self.ft("done", "a/b/over")
        self.ft("set", "a/b/ended", "green", "--tag", "temporary", "--until", at(10))
        self.ft("set", "a/b/live", "green", "--tag", "temporary")
        self.ft("set", "a/b/other", "green", "--tag", "web")
        self.ft("done", "a/b/other")
        self.clock = at(20)  # the until passed; the stand-down is minutes old, not a day
        p = self.ft("gc", "--tag", "temporary")
        self.assertIn("cleared 2", p.stdout)
        self.assertEqual(set(self.ls()), {"a/b/live", "a/b/other"})
        self.ft("gc", "now", ok=False)

    def test_tags_are_a_known_field(self):
        # A file a check didn't write through the CLI keeps its tags and
        # anything else it carries.
        self.ft("set", "a/b/x", "green", "--tag", "web")
        f = self.dir / "a/b/x.json"
        data = json.loads(f.read_text())
        data["custom"] = 1
        f.write_text(json.dumps(data))
        self.ft("set", "a/b/x", "yellow")
        data = json.loads(f.read_text())
        self.assertEqual((data["tags"], data["custom"]), (["web"], 1))

    def test_bar_filters_by_tag(self):
        self.ft("set", "a/b/x", "red", "--tag", "health")
        self.ft("set", "c/d/y", "yellow")
        s = json.loads(self.ft("bar", "--format", "json", "--tag", "health").stdout)
        self.assertEqual((s["count"], s["worst"]), (1, "red"))
        s = json.loads(self.ft("bar", "--format", "json", "--tag", "nope").stdout)
        self.assertEqual(s["count"], 0)


class RemoteTags(Firetower):
    def test_a_sources_tags_come_through_cleaned(self):
        other = self.tmp / "other"
        env = ["env", f"FIRETOWER_DIR={other}", "FIRETOWER_CONFIG=/nonexistent", BIN]
        subprocess.run([*env, "set", "a/b/x", "red", "--tag", "health"], check=True, capture_output=True)
        feed = "\n".join([
            json.dumps({"type": "hello", "v": 1}),
            json.dumps({"type": "lookout", "lookout": {"id": "c/d/y", "district": "c", "tower": "d", "name": "y",
                                                       "state": "green", "tags": ["ok", "Bad", "../x", "ok", "\u202e"],
                                                       "home_district": "forged"}}),
            json.dumps({"type": "sync", "ids": ["c/d/y"]}),
        ])
        self.config.write_text(
            '[[source]]\nname = "m"\ncommand = ' + json.dumps([*env, "watch", "--json"]) + "\n"
            '[[source]]\nname = "odd"\ncommand = ' + json.dumps(["printf", "%s\\n", feed]) + "\n")
        s = json.loads(self.ft("bar", "--format", "json").stdout)
        vs = {v["id"]: v for d in s["districts"] for t in d["towers"] for v in t["lookouts"]}
        self.assertEqual(vs["m:a/b/x"]["tags"], ["health"])
        self.assertEqual(vs["odd:c/d/y"]["tags"], ["ok"])
        self.assertNotIn("home_district", vs["odd:c/d/y"])  # only a board sets it, when it folds


class TagEdges(Firetower):
    def test_a_files_own_tags_are_held_to_names(self):
        self.ft("set", "a/b/x", "green")
        f = self.dir / "a/b/x.json"
        data = json.loads(f.read_text())
        data["tags"] = ["ok", "Bad", "<x>", "ok"] + [f"t{i}" for i in range(40)]
        f.write_text(json.dumps(data))
        tags = self.one("a/b/x")["tags"]
        self.assertIn("ok", tags)
        self.assertNotIn("Bad", tags)
        self.assertEqual(len(tags), 16)
        data["tags"] = "web"  # not a list: read as the one tag, not a broken file
        f.write_text(json.dumps(data))
        v = self.one("a/b/x")
        self.assertEqual((v["state"], v["tags"]), ("green", ["web"]))

    def test_none_and_empty_tags(self):
        self.ft("set", "x", "green", "--tag", "a")
        self.ft("set", "x", "green", "--tag", " none ")
        self.assertEqual(self.one("main/main/x").get("tags", []), [])
        p = self.ft("set", "x", "green", "--tag", "a,", ok=False)
        self.assertIn("--tag", p.stderr)
        self.assertIn("empty", p.stderr)

    def test_a_tag_nothing_carries_is_said_but_not_an_error(self):
        # A tidy-up run twice finds nothing the second time: not a failure,
        # but a typo'd tag shows in the log.
        self.ft("set", "x", "green", "--tag", "web")
        p = self.ft("rm", "--tag", "wbe")
        self.assertIn("no lookout carries wbe", p.stderr)
        self.assertIn("removed 0", p.stdout)
        p = self.ft("gc", "--tag", "wbe")
        self.assertIn("no lookout carries wbe", p.stderr)
        p = self.ft("gc", "--tag", "web")  # carried, just not over: a normal day
        self.assertEqual(p.stderr, "")

    def test_files_that_cant_be_read_are_mentioned(self):
        self.ft("set", "a/b/x", "green", "--tag", "temporary")
        (self.dir / "a/b/broken.json").write_text("{nope")
        p = self.ft("rm", "--tag", "temporary")
        self.assertIn("can't be read", p.stderr)
        self.assertIn("removed 1", p.stdout)

    def test_a_stood_down_watch_keeps_its_tags(self):
        self.ft("set", "x", "green", "--tag", "web")
        self.ft("done", "x")
        self.ft("set", "x", "green", "--tag", "temporary")  # a booked job, still reporting
        self.ft("run", "x", "--tag", "temporary", "--", "true")
        self.assertEqual(self.one("main/main/x")["tags"], ["web"])
        self.assertEqual(self.ft("gc", "--tag", "temporary").stdout.strip(), "cleared 0 stood-down lookouts tagged temporary")

    def test_bar_by_tag_leaves_out_machines_with_none_of_it(self):
        self.ft("set", "a/b/x", "green", "--tag", "health")
        self.config.write_text('[[source]]\nname = "gone"\ncommand = ["sh", "-c", "exit 255"]\n')
        s = json.loads(self.ft("bar", "--format", "json", "--tag", "health").stdout)
        self.assertEqual((s["worst"], s["attention"], s["sources"]), ("green", 0, []))
        s = json.loads(self.ft("bar", "--format", "json").stdout)
        self.assertEqual(s["attention"], 1)  # unfiltered, it still counts


class FeedEdges(Firetower):
    def feed(self, *lookouts):
        lines = [json.dumps({"type": "hello", "v": 1})]
        lines += [json.dumps({"type": "lookout", "lookout": l}) for l in lookouts]
        lines.append(json.dumps({"type": "sync", "ids": [l["id"] for l in lookouts]}))
        self.config.write_text('[[source]]\nname = "m"\ncommand = ' + json.dumps(["printf", "%s\\n", "\n".join(lines)]) + "\n")
        s = json.loads(self.ft("bar", "--format", "json").stdout)
        return {v["id"]: v for d in s["districts"] for t in d["towers"] for v in t["lookouts"]}

    def test_tags_of_the_wrong_type_dont_lose_the_lookout(self):
        vs = self.feed({"id": "a/b/c", "district": "a", "tower": "b", "name": "c", "state": "red", "tags": "deploy"},
                       {"id": "a/b/d", "district": "a", "tower": "b", "name": "d", "state": "red", "tags": [1, {"x": 2}, "ok"]})
        self.assertEqual((vs["m:a/b/c"]["state"], vs["m:a/b/c"]["tags"]), ("red", ["deploy"]))
        self.assertEqual(vs["m:a/b/d"]["tags"], ["ok"])

    def test_a_feeds_word_for_what_the_board_works_out_isnt_taken(self):
        vs = self.feed({"id": "a/b/c", "district": "a", "tower": "b", "name": "c", "state": "yellow",
                        "contact": "lost", "movement": "x" * 5000, "done_by": "me"})
        v = vs["m:a/b/c"]
        self.assertNotIn("contact", v)
        self.assertNotIn("movement", v)
        self.assertNotIn("done_by", v)

    def test_an_id_that_isnt_its_names_is_dropped(self):
        vs = self.feed({"id": "inbox/bob/x", "district": "app", "tower": "web", "name": "x", "state": "red"},
                       {"id": "app/web/y", "district": "app", "tower": "web", "name": "y", "state": "green"})
        self.assertEqual(list(vs), ["m:app/web/y"])
