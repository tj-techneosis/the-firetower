"""Passing on: a board's picture as a feed (`watch --json --pass-on`), so one
board can be the one stop for a group of machines.

Three Firetowers: `far` (a machine with lookouts), `hub` (a board that reads
far, with hide rules of its own) and this test's own, which reads hub. The
reader asks for pass-on; a plain `watch --json` is unchanged."""

import json
import os
import subprocess
import time

from _binary import BIN, Firetower
from test_hub import free_port


class PassOnBase(Firetower):
    def setUp(self):
        super().setUp()
        self.clock = ""  # boards run on the real clock
        self.far = self.tmp / "far"
        self.hub = self.tmp / "hub"
        self.hub_config = self.tmp / "hub.toml"
        self.hub_port = free_port()
        self.port = free_port()
        self.dials = self.tmp / "dials"
        self.far_cmd = ["env", f"FIRETOWER_DIR={self.far}", "FIRETOWER_CONFIG=/nonexistent", BIN]
        self.hub_cmd = ["env", f"FIRETOWER_DIR={self.hub}", f"FIRETOWER_CONFIG={self.hub_config}", BIN]
        far_feed = ["sh", "-c", f'echo x >> "{self.dials}"; exec "$@"', "dial", *self.far_cmd, "watch", "--json"]
        self.hub_config.write_text(
            f'[board]\nport = {self.hub_port}\nhide = ["secret"]\n'
            f'[[source]]\nname = "far"\ncommand = {json.dumps(far_feed)}\n'
            f'act = {json.dumps(self.far_cmd)}\nhide = ["inbox/bob"]\n')
        self.write_reader()
        self.run_on(self.far_cmd, "set", "inbox/carol/new", "yellow", "two unread")
        self.run_on(self.far_cmd, "set", "inbox/bob/new", "red", "bob's")
        self.run_on(self.hub_cmd, "set", "app/web/up", "green")
        self.run_on(self.hub_cmd, "set", "app/secret/key", "red", "hidden on hub")
        self.addCleanup(lambda: self.run_on(self.hub_cmd, "board", "--stop", check=False))
        self.addCleanup(lambda: self.ft("board", "--stop", ok=None))

    def env(self, **extra):
        e = super().env(**extra)
        if not e.get("FIRETOWER_NOW"):
            e.pop("FIRETOWER_NOW", None)
        return e

    def write_reader(self, extra=""):
        self.config.write_text(
            f'[board]\nport = {self.port}\n'
            f'[[source]]\nname = "hub"\ncommand = {json.dumps([*self.hub_cmd, "watch", "--json", "--pass-on"])}\n'
            f'act = {json.dumps(self.hub_cmd)}\n' + extra)

    def run_on(self, cmd, *args, check=True):
        # A clean environment: only what a shell needs, so no scheduler or tower variable leaks in.
        env = {k: os.environ[k] for k in ("PATH", "HOME", "TMPDIR", "LANG", "USER") if k in os.environ}
        p = subprocess.run([*cmd, *args], capture_output=True, text=True, env=env, timeout=30)
        if check and p.returncode != 0:
            self.fail(f"{args} exited {p.returncode}: {p.stdout}{p.stderr}")
        return p

    def other(self, cmd, id):
        out = self.run_on(cmd, "ls", "--json", "--all").stdout
        return {v["id"]: v for v in json.loads(out)}.get(id)

    def wait_synced(self, bar):
        for _ in range(100):
            s = bar()
            if s["sources"] and all(x["contact"] == "ok" for x in s["sources"]):
                return s
            time.sleep(0.1)
        self.fail(f"never synced: {s['sources']}")

    def start_hub(self):
        self.run_on(self.hub_cmd, "board", "--start")
        self.wait_synced(lambda: json.loads(self.run_on(self.hub_cmd, "bar", "--format", "json").stdout))

    def start_reader(self):
        self.ft("board", "--start")
        return self.wait_synced(self.bar)

    def bar(self):
        return json.loads(self.ft("bar", "--format", "json").stdout)

    def lookouts(self, s):
        return {v["id"]: v for d in s["districts"] for t in d["towers"] for v in t["lookouts"]}

    def feed(self, cmd, *flags, ok=True):
        p = self.run_on(cmd, "watch", "--json", "--once", *flags, check=ok)
        return [json.loads(line) for line in p.stdout.splitlines()], p

    def acts_log(self, d):
        f = d / "acts.log"
        return f.read_text() if f.exists() else ""


class PlainFeed(PassOnBase):
    def test_a_reader_that_doesnt_ask_gets_the_raw_feed_as_ever(self):
        # A board with sources and hide rules, running: a plain watch is
        # still only the machine's own lookouts, raw, the hidden one too,
        # with nothing a pass-on feed adds.
        self.start_hub()
        events, _ = self.feed(self.hub_cmd)
        self.assertEqual(events[0].keys(), {"type", "v", "version"})
        self.assertEqual({e["type"] for e in events}, {"hello", "lookout", "sync"})
        looks = [e["lookout"] for e in events if e["type"] == "lookout"]
        self.assertEqual(sorted(v["id"] for v in looks), ["app/secret/key", "app/web/up"])
        for v in looks:
            for k in ("origin", "origin_id", "via", "contact", "home_district"):
                self.assertNotIn(k, v)
        self.assertEqual(events[-1]["ids"], ["app/secret/key", "app/web/up"])
        # And it makes nothing new in the state dir.
        self.assertFalse((self.far / ".firetower-id").exists())
        self.feed(self.far_cmd)
        self.assertFalse((self.far / ".firetower-id").exists())


class PassOn(PassOnBase):
    def test_pass_on_is_the_boards_picture(self):
        self.start_hub()
        events, _ = self.feed(self.hub_cmd, "--pass-on")
        hello = events[0]
        self.assertTrue(hello["pass_on"])
        hub_id = (self.hub / ".firetower-id").read_text().strip()
        self.assertEqual(hello["firetower_id"], hub_id)
        self.assertEqual(hello["path"], [hub_id])  # far is read plain: no id of its own
        sources = [e for e in events if e["type"] == "sources"]
        self.assertEqual([s["name"] for s in sources[0]["sources"]], ["far"])
        looks = {e["lookout"]["id"]: e["lookout"] for e in events if e["type"] == "lookout"}
        # Its own, after its [board] hide; far's, after far's hide.
        self.assertEqual(sorted(looks), ["app/web/up", "far:inbox/carol/new"])
        self.assertEqual(looks["far:inbox/carol/new"]["origin"], "far")
        self.assertEqual(looks["app/web/up"]["origin_id"], hub_id)
        self.assertEqual(events[-1]["type"], "sync")

    def test_pass_on_needs_the_board_and_never_dials_itself(self):
        events, p = self.feed(self.hub_cmd, "--pass-on", ok=False)
        self.assertNotEqual(p.returncode, 0)
        self.assertIn("no board", p.stderr)
        self.assertEqual(events, [])
        self.assertFalse(self.dials.exists())

    def test_a_reader_shows_the_trimmed_view_with_its_origins(self):
        self.start_hub()
        s = self.bar()  # no board here: the bar reads hub's pass-on itself
        looks = self.lookouts(s)
        self.assertEqual(sorted(looks), ["hub:app/web/up", "hub:far:inbox/carol/new"])
        carol = looks["hub:far:inbox/carol/new"]
        self.assertEqual(carol["origin"], "far")  # its own word, not relabelled
        self.assertEqual(carol["via"], ["hub"])
        self.assertEqual(looks["hub:app/web/up"]["origin"], "hub")
        names = {x["name"]: x for x in s["sources"]}
        self.assertEqual(names["far"]["via"], ["hub"])
        self.assertEqual(names["far"]["contact"], "ok")
        self.assertTrue(names["hub"]["pass_on"])

    def test_readers_share_the_boards_one_stream_per_source(self):
        self.start_hub()
        self.assertEqual(len(self.dials.read_text().splitlines()), 1)
        self.start_reader()
        for _ in range(3):
            self.bar()
            self.feed(self.hub_cmd, "--pass-on")
        self.assertEqual(len(self.dials.read_text().splitlines()), 1)

    def test_a_machine_out_of_contact_behind_the_board_shows_so(self):
        self.hub_config.write_text(self.hub_config.read_text() +
                                   '[[source]]\nname = "gone"\ncommand = ["false"]\n')
        self.run_on(self.hub_cmd, "board", "--start")
        for _ in range(100):
            s = self.bar()
            names = {x["name"]: x for x in s["sources"]}
            if names.get("far", {}).get("contact") == "ok" and names.get("gone", {}).get("contact") == "lost":
                break
            time.sleep(0.1)
        self.assertEqual(names["gone"]["contact"], "lost")
        self.assertEqual(names["gone"]["via"], ["hub"])
        self.assertEqual(s["counts"].get("lost"), 1)


class Routing(PassOnBase):
    def test_seen_and_done_go_back_the_way_they_came_and_are_written_down(self):
        self.start_hub()
        self.start_reader()
        self.ft("seen", "hub:far:inbox/carol/new")
        self.assertTrue(self.other(self.far_cmd, "inbox/carol/new")["seen"])
        for d, where in ((self.dir, "sent on to hub"), (self.hub, "sent on to far")):
            log = self.acts_log(d)
            self.assertIn("seen", log)
            self.assertIn(where, log)
        hub_log = self.acts_log(self.hub)
        reader_id = (self.dir / ".firetower-id").read_text().strip()
        self.assertIn(f"from board {reader_id}", hub_log)
        self.ft("done", "hub:far:inbox/carol/new")
        self.assertEqual(self.other(self.far_cmd, "inbox/carol/new")["state"], "done")
        # The board's own, one hop: done there, written down there.
        self.ft("done", "hub:app/web/up")
        self.assertEqual(self.other(self.hub_cmd, "app/web/up")["state"], "done")
        self.assertIn("done app/web/up", self.acts_log(self.hub))
        self.assertIn("done here", self.acts_log(self.hub))

    def test_rm_stays_direct(self):
        self.start_hub()
        self.start_reader()
        p = self.ft("rm", "hub:far:inbox/carol/new", ok=False)
        self.assertIn("only seen and done", p.stderr)
        self.assertIsNotNone(self.other(self.far_cmd, "inbox/carol/new"))
        p = self.ft("expect", "hub:far:inbox/carol/new", ok=False)
        self.assertIsNotNone(self.other(self.far_cmd, "inbox/carol/new"))

    def test_hidden_by_the_middle_board_is_never_reached(self):
        self.start_hub()
        self.start_reader()
        p = self.ft("seen", "hub:far:inbox/bob/new", ok=False)
        self.assertIn("no lookout", p.stderr)
        self.assertFalse(self.other(self.far_cmd, "inbox/bob/new")["seen"])
        p = self.ft("seen", "hub:app/secret/key", ok=False)
        self.assertIn("no lookout", p.stderr)
        # Even asked straight, a routed seen is held to hub's own rules.
        p = self.run_on(self.hub_cmd, "seen", "app/secret/key", "--from", "someone@" + "0" * 32, check=False)
        self.assertNotEqual(p.returncode, 0)
        self.assertFalse(self.other(self.hub_cmd, "app/secret/key")["seen"])
        self.assertIn("app/secret/key", self.acts_log(self.hub))

    def test_a_chain_goes_only_through_a_board(self):
        self.start_hub()
        p = self.ft("seen", "hub:far:inbox/carol/new", ok=False)  # no board here
        self.assertIn("board", p.stderr)
        self.assertFalse(self.other(self.far_cmd, "inbox/carol/new")["seen"])

    def test_from_is_held_to_its_alphabet(self):
        p = self.run_on(self.hub_cmd, "seen", "app/web/up", "--from", "x; rm -rf ~", check=False)
        self.assertNotEqual(p.returncode, 0)
        self.assertIn("--from", p.stderr)
        self.assertEqual(self.acts_log(self.hub), "")


class Guards(PassOnBase):
    def test_a_direct_read_wins(self):
        self.start_hub()
        far_feed = [*self.far_cmd, "watch", "--json"]
        self.write_reader(f'[[source]]\nname = "far"\ncommand = {json.dumps(far_feed)}\nact = {json.dumps(self.far_cmd)}\n')
        s = self.bar()
        looks = self.lookouts(s)
        self.assertIn("far:inbox/carol/new", looks)
        self.assertIn("far:inbox/bob/new", looks)  # this board's own rules, not hub's
        self.assertNotIn("hub:far:inbox/carol/new", looks)
        self.assertEqual([x["name"] for x in s["sources"]].count("far"), 1)

    def test_two_boards_reading_each_other_never_loop(self):
        # hub also reads this board, passed on: neither sees its own come back.
        reader_env = ["env", f"FIRETOWER_DIR={self.dir}", f"FIRETOWER_CONFIG={self.config}", BIN]
        self.hub_config.write_text(self.hub_config.read_text() +
                                   f'[[source]]\nname = "back"\ncommand = {json.dumps([*reader_env, "watch", "--json", "--pass-on"])}\n'
                                   f'act = {json.dumps(reader_env)}\n')
        self.ft("set", "mine/here/x", "green")
        self.ft("board", "--start")  # hub isn't up yet: its source is out of contact for now
        self.start_hub()
        for _ in range(100):
            s = self.bar()
            looks = self.lookouts(s)
            if "hub:far:inbox/carol/new" in looks and s["sources"][0]["contact"] == "ok":
                break
            time.sleep(0.1)
        hub_s = json.loads(self.run_on(self.hub_cmd, "bar", "--format", "json").stdout)
        for _ in range(100):
            hub_s = json.loads(self.run_on(self.hub_cmd, "bar", "--format", "json").stdout)
            if "back:mine/here/x" in self.lookouts(hub_s):
                break
            time.sleep(0.1)
        time.sleep(2)  # a few feed rounds both ways
        looks = self.lookouts(self.bar())
        self.assertEqual(sorted(looks), ["hub:app/web/up", "hub:far:inbox/carol/new", "mine/here/x"])
        hub_looks = self.lookouts(json.loads(self.run_on(self.hub_cmd, "bar", "--format", "json").stdout))
        self.assertEqual(sorted(hub_looks), ["app/web/up", "back:mine/here/x", "far:inbox/carol/new"])
        names = [x["name"] for x in self.bar()["sources"]]
        self.assertNotIn("back", names)
        self.assertTrue(any("left out" in (x.get("loop") or "") for x in self.bar()["sources"]))

    def crafted(self, *lines, pass_on=True, self_id=None):
        script = self.tmp / "feed.sh"
        hello = {"type": "hello", "v": 1, "version": "x"}
        if pass_on:
            hello.update(pass_on=True, firetower_id=self_id or "a" * 32, path=[self_id or "a" * 32])
        out = [hello, *lines, {"type": "sync", "at": "2026-10-31T12:00:00Z", "ids": [l["lookout"]["id"] for l in lines if l.get("type") == "lookout"]}]
        script.write_text("cat <<'EOF'\n" + "\n".join(json.dumps(o) for o in out) + "\nEOF\nsleep 5\n")
        self.config.write_text(f'[[source]]\nname = "hub"\ncommand = ["sh", "{script}"]\n')

    def look(self, id, **kw):
        d, t, n = id.split(":")[-1].split("/")
        v = {"id": id, "name": n, "district": d, "tower": t, "label": n, "state": "red"}
        v.update(kw)
        return {"type": "lookout", "lookout": v}

    def test_crafted_feeds(self):
        self.bar()
        me = (self.dir / ".firetower-id").read_text().strip()
        self.crafted(
            self.look("a/b/ok"),
            self.look("x:a/b/passed", origin="x"),
            self.look("a/b/mine", origin_id=me),                      # this board's own, back
            self.look("y:a/b/third", origin="y", via=["z"]),          # a third hop
            self.look("y:a/b/liar", origin="y", name="other"),        # id and names disagree
            self.look("y:a/b/wrong", origin="q"),                     # id and origin disagree
            self.look("y:../b/c", origin="y", district=".."),         # not a name
            {"type": "sources", "sources": [{"name": "x", "contact": "lost", "can_act": True},
                                            {"name": "me", "contact": "ok", "firetower_id": me},
                                            {"name": "deep", "contact": "ok", "via": ["z"]}]},
        )
        s = self.bar()
        self.assertEqual(sorted(self.lookouts(s)), ["hub:a/b/ok", "hub:x:a/b/passed"])
        self.assertEqual(sorted(x["name"] for x in s["sources"]), ["hub", "x"])
        hub = [x for x in s["sources"] if x["name"] == "hub"][0]
        self.assertIn("left out", hub["loop"])
        self.assertFalse([x for x in s["sources"] if x["name"] == "x"][0]["can_act"])  # hub has no act here

    def test_a_feed_that_is_this_board_is_refused(self):
        self.bar()
        me = (self.dir / ".firetower-id").read_text().strip()
        self.crafted(self.look("a/b/ok"), self_id=me)
        s = self.bar()
        self.assertEqual(self.lookouts(s), {})
        self.assertEqual(s["sources"][0]["contact"], "lost")
        self.assertIn("itself", s["sources"][0]["error"])

    def test_a_plain_feed_cant_claim_to_pass_on(self):
        self.crafted(self.look("x:a/b/passed", origin="x"), self.look("a/b/ok", origin="x", via=["q"]), pass_on=False)
        looks = self.lookouts(self.bar())
        self.assertEqual(sorted(looks), ["hub:a/b/ok"])
        self.assertEqual(looks["hub:a/b/ok"]["origin"], "hub")
        self.assertNotIn("via", looks["hub:a/b/ok"])


class Streams(PassOnBase):
    def alive(self, pid):
        try:
            os.kill(pid, 0)
            return True
        except ProcessLookupError:
            return False

    def test_a_reconnect_leaves_one_stream(self):
        pids = self.tmp / "pids"
        cmd = ["sh", "-c", f'echo $$ >> "{pids}"; exec timeout 2 "$@"', "s", *self.far_cmd, "watch", "--json"]
        self.config.write_text(f'[board]\nport = {self.port}\n[[source]]\nname = "far"\ncommand = {json.dumps(cmd)}\n')
        self.ft("board", "--start")
        deadline = time.time() + 20
        while time.time() < deadline and (not pids.exists() or len(pids.read_text().split()) < 3):
            time.sleep(0.2)
        seen = pids.read_text().split()
        self.assertGreaterEqual(len(seen), 3)
        alive = [p for p in seen if self.alive(int(p))]
        self.assertLessEqual(len(alive), 1, alive)
