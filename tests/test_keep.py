"""hide and keep down to one lookout, and by tag (since 0.5.0)."""
import json
import time
import unittest

from test_boards import Remote, free_port


class KeepOne(Remote):
    def setUp(self):
        super().setUp()
        self.on_other("set", "main/heartbeats/alice", "green", "--value", "shared-box", "--tag", "staff,on-shared-box")
        self.on_other("set", "main/heartbeats/bob", "green", "--value", "laptop", "--tag", "staff,on-laptop")
        self.on_other("set", "site/web/up", "green")

    def other_one(self, id):
        out = __import__("subprocess").run([*self.remote_cmd, "ls", "--json"], check=True,
                                           capture_output=True, text=True).stdout
        return next(v for v in json.loads(out) if v["id"] == id)

    def shown(self, **opts):
        self.write_config(name="shared", command=[*self.remote_cmd, "watch", "--json"], **opts)
        return sorted(self.ids(self.bar()))

    def test_keep_one_lookout_in_a_hidden_tower(self):
        self.assertEqual(self.shown(hide=["main/heartbeats"], keep=["main/heartbeats/alice"]),
                         ["shared:main/heartbeats/alice", "shared:site/web/up"])

    def test_hide_one_lookout(self):
        self.assertEqual(self.shown(hide=["*/*/bob"]),
                         ["shared:main/heartbeats/alice", "shared:site/web/up"])

    def test_keep_by_tag(self):
        self.assertEqual(self.shown(hide=["heartbeats"], keep_tags=["on-shared-box"]),
                         ["shared:main/heartbeats/alice", "shared:site/web/up"])

    def test_keep_by_tag_follows_a_move(self):
        self.on_other("set", "main/heartbeats/bob", "green", "--tag", "staff,on-shared-box")
        self.assertEqual(len(self.shown(hide=["heartbeats"], keep_tags=["on-shared-*"])), 3)
        self.on_other("set", "main/heartbeats/bob", "green", "--tag", "staff,on-laptop")
        self.assertEqual(self.shown(hide=["heartbeats"], keep_tags=["on-shared-*"]),
                         ["shared:main/heartbeats/alice", "shared:site/web/up"])

    def test_hide_by_tag_and_keep_wins(self):
        self.assertEqual(self.shown(hide_tags=["staff"]), ["shared:site/web/up"])
        self.assertEqual(self.shown(hide_tags=["staff"], keep=["*/*/bob"]),
                         ["shared:main/heartbeats/bob", "shared:site/web/up"])

    def test_keep_tags_alone_change_nothing(self):
        self.assertEqual(len(self.shown(keep_tags=["nothing-has-this"])), 3)

    def test_never_by_value(self):
        # The value is a check's own word: a tag that happens to equal it keeps nothing.
        self.assertEqual(self.shown(hide=["heartbeats"], keep_tags=["shared-box"]), ["shared:site/web/up"])

    def test_bad_tag_patterns_refuse(self):
        for pat in ["a/b", "", "Staff", "-x", "a!b", "[!]", "a" * 65, "[._]*", "[!a-z0-9]x", "[]",
                    "*" + "a" * 65, "a?" + "b" * 63, "[ab]" * 65, "a[z-a]", "a]*", "x[!-._a-z0-9]", "a^b"]:
            for field in ("hide_tags", "keep_tags"):
                with self.subTest(pat=pat, field=field):
                    self.write_config(name="shared", command=[*self.remote_cmd, "watch", "--json"], **{field: [pat]})
                    p = self.ft("bar", ok=False)
                    self.assertIn(f"{field} pattern {json.dumps(pat if len(pat) <= 40 else pat[:40] + '…', ensure_ascii=False)}", p.stdout)
                    self.config.write_text(f"[board]\n{field} = {json.dumps([pat])}\n")
                    self.assertIn(f"[board]'s {field} pattern", self.ft("bar", ok=False).stdout)
                    if pat in ("Staff", "-x", "a!b", "[._]*", "a[z-a]"):
                        self.assertRegex(p.stdout, r"\btags (are|start|hold)\b")

    def refusal(self, **opts):
        self.write_config(name="shared", command=[*self.remote_cmd, "watch", "--json"], **opts)
        p = self.ft("board", "--port", str(free_port()), ok=False)
        return p.stdout + p.stderr

    def test_too_long_says_how_long(self):
        self.assertIn("needs 65 characters, and tags are at most 64", self.refusal(keep_tags=["a" * 65]))
        self.assertIn("needs 65 characters", self.refusal(keep_tags=["[ab]" * 65]))

    def test_the_reason_names_the_culprit_past_a_cut(self):
        out = self.refusal(keep_tags=["a" * 40 + "A"])
        self.assertIn('has \'A\'', out)
        self.assertIn('"' + "a" * 40 + '…"', out)
        # cut by characters, so nothing is split
        self.assertIn('"' + "é" * 40 + '…"', self.refusal(keep_tags=["é" * 41]))

    def test_each_reason_says_what_it_is(self):
        self.assertIn("tags start with a lowercase letter or digit", self.refusal(keep_tags=["-x"]))
        self.assertIn('"[z-a]" lets in nothing tags hold', self.refusal(keep_tags=["a[z-a]"]))
        self.assertIn('"]" lets in nothing names hold', self.refusal(hide=["a]*"]))
        self.assertIn('"!" lets in nothing names hold', self.refusal(hide=["a!b"]))

    def test_bang_or_caret_later_in_a_class_is_a_character(self):
        # "[a!]" still lets in "a", so it can match a name: not refused.
        self.assertEqual(self.shown(hide=["heartbeats"], keep=["main/heartbeats/[a!]lice"]),
                         ["shared:main/heartbeats/alice", "shared:site/web/up"])
        self.assertEqual(self.shown(hide=["heartbeats"], keep_tags=["on-[s^]hared-box"]),
                         ["shared:main/heartbeats/alice", "shared:site/web/up"])

    def test_a_class_starting_with_a_bracket_is_not_a_pattern(self):
        # path.Match refuses a class whose first character is "]".
        self.assertIn("is not a pattern", self.refusal(keep_tags=["[]o]n-shared-box"]))

    def test_a_pattern_of_64_characters_matches_a_tag_of_64(self):
        self.on_other("set", "main/heartbeats/alice", "green", "--tag", "staff," + "a" * 64)
        self.assertEqual(self.shown(hide=["heartbeats"], keep_tags=["?" + "a" * 62 + "[ab]"]),
                         ["shared:main/heartbeats/alice", "shared:site/web/up"])

    def test_three_part_patterns_under_board(self):
        self.ft("set", "mine/jobs/one", "red")
        self.ft("set", "mine/jobs/two", "red")
        self.config.write_text('[board]\nhide = ["mine/jobs"]\nkeep = ["mine/jobs/one"]\n')
        ids = self.ids(self.bar())
        self.assertIn("mine/jobs/one", ids)
        self.assertNotIn("mine/jobs/two", ids)

    def test_without_a_board_tags_are_asked_of_the_machine(self):
        self.on_other("set", "main/heartbeats/alice", "red", "--tag", "staff,on-shared-box")
        self.on_other("set", "main/heartbeats/bob", "red", "--tag", "staff,on-laptop")
        self.on_other("set", "site/web/up", "red")
        self.write_config(name="shared", command=[*self.remote_cmd, "watch", "--json"],
                          act=self.remote_cmd, hide=["heartbeats"], keep_tags=["on-shared-box"])
        self.ft("seen", "shared:main/heartbeats/alice")
        self.assertTrue(self.other_one("main/heartbeats/alice").get("seen"))
        p = self.ft("seen", "shared:main/heartbeats/bob", ok=False)
        self.assertIn("no lookout", p.stdout + p.stderr)
        self.assertFalse(self.other_one("main/heartbeats/bob").get("seen"))
        self.ft("seen", "shared:site/web/up")  # its places decide: no question asked

    def test_without_a_board_hide_tags_hide_only_the_tagged(self):
        self.on_other("set", "main/heartbeats/alice", "red", "--tag", "staff")
        self.on_other("set", "site/web/up", "red")
        self.write_config(name="shared", command=[*self.remote_cmd, "watch", "--json"],
                          act=self.remote_cmd, hide_tags=["staff"])
        p = self.ft("rm", "shared:main/heartbeats/alice", ok=False)
        self.assertIn("no lookout", p.stdout + p.stderr)
        self.ft("seen", "shared:site/web/up")  # untagged: shown, acted on
        p = self.ft("rm", "shared:no/such/one", ok=False)
        self.assertIn("no lookout", p.stdout + p.stderr)

    def test_without_a_board_hide_and_keep_tags_together(self):
        self.on_other("set", "main/heartbeats/alice", "red", "--tag", "staff,on-shared-box")
        self.on_other("set", "main/heartbeats/bob", "red", "--tag", "staff,on-laptop")
        self.write_config(name="shared", command=[*self.remote_cmd, "watch", "--json"],
                          act=self.remote_cmd, hide_tags=["staff"], keep_tags=["on-shared-box"])
        self.ft("seen", "shared:main/heartbeats/alice")
        p = self.ft("seen", "shared:main/heartbeats/bob", ok=False)
        self.assertIn("no lookout", p.stdout + p.stderr)

    def old_stood_down_alice(self):
        self.on_other("set", "main/heartbeats/alice", "red", "--tag", "on-shared-box")
        self.on_other("done", "main/heartbeats/alice")
        self.remote_cmd.insert(1, "FIRETOWER_NOW=2030-01-01T00:00:00Z")
        return self.other / "main" / "heartbeats" / "alice.json"

    def test_without_a_board_one_stood_down_past_a_day_is_found_by_tag(self):
        # Acting by id reaches it when the place rules decide; a tag rule
        # mustn't change that, so its tags are read with ls --all.
        f = self.old_stood_down_alice()
        self.write_config(name="shared", command=[*self.remote_cmd, "watch", "--json"],
                          act=self.remote_cmd, hide=["heartbeats"], keep_tags=["on-shared-box"])
        self.ft("rm", "shared:main/heartbeats/alice")
        self.assertFalse(f.exists())

    def test_without_a_board_one_stood_down_past_a_day_is_found_by_name(self):
        f = self.old_stood_down_alice()
        self.write_config(name="shared", command=[*self.remote_cmd, "watch", "--json"],
                          act=self.remote_cmd, hide=["heartbeats"], keep=["main/heartbeats/alice"])
        self.ft("rm", "shared:main/heartbeats/alice")
        self.assertFalse(f.exists())

    def test_without_a_board_one_stood_down_past_a_day_still_hidden_by_tag(self):
        f = self.old_stood_down_alice()
        self.write_config(name="shared", command=[*self.remote_cmd, "watch", "--json"],
                          act=self.remote_cmd, hide_tags=["on-shared-box"])
        p = self.ft("rm", "shared:main/heartbeats/alice", ok=False)
        self.assertIn("no lookout", p.stdout + p.stderr)
        self.assertTrue(f.exists())

    def test_without_a_board_ls_refused_says_so(self):
        self.on_other("set", "main/heartbeats/alice", "red", "--tag", "on-shared-box")
        self.write_config(name="shared", command=[*self.remote_cmd, "watch", "--json"],
                          act=["sh", "-c", 'case "$1" in ls) echo "only seen" >&2; exit 1;; esac', "x"],
                          hide=["heartbeats"], keep_tags=["on-shared-box"])
        p = self.ft("seen", "shared:main/heartbeats/alice", ok=False)
        self.assertIn("reading its tags", p.stdout + p.stderr)

    def test_hide_tags_with_keep_tags(self):
        self.assertEqual(self.shown(hide_tags=["staff"], keep_tags=["on-laptop"]),
                         ["shared:main/heartbeats/bob", "shared:site/web/up"])

    def test_tags_past_sixteen_are_chosen_in_order_not_by_the_feed(self):
        feed = "\n".join([
            json.dumps({"type": "hello", "v": 1}),
            json.dumps({"type": "lookout", "lookout": {
                "id": "a/b/c", "district": "a", "tower": "b", "name": "c", "state": "green",
                "tags": [f"t{i:02d}" for i in range(20, 0, -1)]}}),
            json.dumps({"type": "sync", "ids": ["a/b/c"]}),
        ])
        # t01..t16 are kept, whatever order the feed gave them in.
        self.write_config(name="odd", command=["printf", "%s\\n", feed], hide=["b"], keep_tags=["t16"])
        self.assertEqual(self.ids(self.bar()), ["odd:a/b/c"])
        self.write_config(name="odd", command=["printf", "%s\\n", feed], hide=["b"], keep_tags=["t17"])
        self.assertEqual(self.ids(self.bar()), [])

    def test_without_a_board_a_kept_one_by_name_is_acted_on(self):
        self.on_other("set", "main/heartbeats/alice", "red", "--tag", "staff")
        self.write_config(name="shared", command=[*self.remote_cmd, "watch", "--json"],
                          act=self.remote_cmd, hide=["heartbeats"], keep=["main/heartbeats/alice"],
                          hide_tags=["nothing"])
        self.ft("seen", "shared:main/heartbeats/alice")
        p = self.ft("seen", "shared:main/heartbeats/bob", ok=False)
        self.assertIn("no lookout", p.stdout + p.stderr)

    def test_board_acts_by_tag(self):
        self.on_other("set", "main/heartbeats/alice", "red", "--tag", "on-shared-box")
        self.on_other("set", "main/heartbeats/bob", "red", "--tag", "on-laptop")
        self.write_config(name="shared", command=[*self.remote_cmd, "watch", "--json"],
                          act=self.remote_cmd, hide=["heartbeats"], keep_tags=["on-shared-box"])
        req = self.start_board()
        for _ in range(50):
            st = json.loads(req("GET", "/api/state")[1])
            if st["sources"][0]["contact"] == "ok":
                break
            time.sleep(0.2)
        else:
            self.fail("the source never came into contact")
        self.assertNotEqual(req("POST", "/api/seen", {"id": "shared:main/heartbeats/bob"})[0], 200)
        self.assertEqual(req("POST", "/api/seen", {"id": "shared:main/heartbeats/alice"})[0], 200)

    def test_this_machines_own_by_name(self):
        self.ft("set", "main/heartbeats/a", "red")
        self.ft("set", "main/heartbeats/b", "red")
        self.config.write_text('[board]\nhide = ["heartbeats"]\nkeep = ["main/heartbeats/a"]\n')
        self.assertEqual(sorted(self.ids(self.bar())), ["main/heartbeats/a"])

    def test_this_machines_own_by_tag(self):
        self.ft("set", "main/heartbeats/a", "red", "--tag", "here")
        self.ft("set", "main/heartbeats/b", "red", "--tag", "away")
        self.config.write_text('[board]\nhide = ["heartbeats"]\nkeep_tags = ["here"]\n')
        req = self.start_board()
        ids = [v["id"] for v in json.loads(req("GET", "/api/state")[1])["lookouts"]]
        self.assertEqual(ids, ["main/heartbeats/a"])
        self.assertNotEqual(req("POST", "/api/seen", {"id": "main/heartbeats/b"})[0], 200)
        self.assertEqual(req("POST", "/api/seen", {"id": "main/heartbeats/a"})[0], 200)


if __name__ == "__main__":
    unittest.main()
