"""The boards: bar, the web board's API and its guard, and other machines'
feeds (sources)."""

import http.client
import json
import os
import socket
import subprocess
import time
import urllib.parse
from pathlib import Path

from _binary import BIN, Firetower


def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


class Remote(Firetower):
    """A second state dir stands in for another machine."""

    def setUp(self):
        super().setUp()
        self.other = self.tmp / "other"
        self.remote_cmd = ["env", f"FIRETOWER_DIR={self.other}", "FIRETOWER_CONFIG=/nonexistent", BIN]

    def on_other(self, *args):
        subprocess.run([*self.remote_cmd, *args], check=True, capture_output=True)

    def write_config(self, **source):
        lines = ["[[source]]"]
        for k, v in source.items():
            lines.append(f"{k} = {json.dumps(v)}")
        self.config.write_text("\n".join(lines) + "\n")

    def bar(self, *args):
        return json.loads(self.ft("bar", "--format", "json", *args).stdout)

    def ids(self, s):
        return [v["id"] for d in s["districts"] for t in d["towers"] for v in t["lookouts"]]

    def start_board(self):
        """A board on this config; returns req(method, path, body) -> (status, data)."""
        port = free_port()
        env = self.env()
        env.pop("FIRETOWER_NOW")
        proc = subprocess.Popen([BIN, "board", "--port", str(port)], env=env,
                                stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        self.addCleanup(lambda: (proc.terminate(), proc.wait(5), proc.stdout.close()))
        self.assertIn("is up", proc.stdout.readline())
        key = (self.dir / ".board-key").read_text().strip()

        def req(method, path, body=None):
            c = http.client.HTTPConnection("127.0.0.1", port, timeout=20)
            c.request(method, path, body=json.dumps(body) if body is not None else None,
                      headers={"Host": f"127.0.0.1:{port}", "Authorization": f"Bearer {key}"})
            r = c.getresponse()
            data = r.read().decode()
            c.close()
            return r.status, data
        return req


class Sources(Remote):
    def test_source_as_is(self):
        self.ft("set", "app/signup/otp", "green")
        self.on_other("set", "website/checkout/deploy", "red", "boom")
        self.write_config(name="work-mac", command=[*self.remote_cmd, "watch", "--json"])
        s = self.bar()
        self.assertEqual(self.ids(s), ["app/signup/otp", "work-mac:website/checkout/deploy"])
        self.assertEqual(s["worst"], "red")
        self.assertEqual(s["sources"][0]["contact"], "ok")

    def test_source_folded_into_one_district(self):
        self.on_other("set", "website/checkout/deploy", "green")
        self.on_other("set", "app/signup/otp", "yellow")
        self.write_config(name="work-mac", command=[*self.remote_cmd, "watch", "--json"], district="work")
        s = self.bar()
        self.assertEqual([d["name"] for d in s["districts"]], ["work"])
        vs = {v["id"]: v for d in s["districts"] for t in d["towers"] for v in t["lookouts"]}
        self.assertEqual(vs["work-mac:website/checkout/deploy"]["home_district"], "website")
        self.assertEqual(vs["work-mac:app/signup/otp"]["home_district"], "app")

    def test_lost_machines_count_once_each(self):
        self.write_config(name="gone-mac", command=["sh", "-c", "exit 255"])
        s = self.bar()
        self.assertEqual((s["counts"].get("lost"), s["attention"]), (1, 1))
        self.assertIn("gone-mac out of contact", self.ft("bar").stdout)

    def test_failing_source_is_out_of_contact(self):
        self.ft("set", "x", "green")
        self.write_config(name="gone-mac", command=["sh", "-c", "echo 'ssh: connect: no route' >&2; exit 255"])
        s = self.bar()
        self.assertEqual(s["sources"][0]["contact"], "lost")
        self.assertIn("no route", s["sources"][0]["error"])
        self.assertNotEqual(s["worst"], "green")  # silence is never clear
        self.assertIn("out of contact", self.ft("bar").stdout)

    def test_missing_command_is_out_of_contact(self):
        self.write_config(name="m", command=["/no/such/ssh"])
        self.assertEqual(self.bar()["sources"][0]["contact"], "lost")

    def test_slow_source_times_out(self):
        self.write_config(name="slow", command=["sleep", "30"])
        t = time.time()
        s = self.bar("--timeout", "1s")
        self.assertLess(time.time() - t, 10)
        self.assertEqual(s["sources"][0]["contact"], "lost")

    def test_hostile_feed(self):
        # Another machine's feed is another program's output: junk lines are
        # skipped, text is cleaned again, bad places are renamed.
        feed = "\n".join([
            "Welcome to work-mac!",
            json.dumps({"type": "hello", "v": 1}),
            json.dumps({"type": "lookout", "lookout": {"id": "../../x/b/c", "district": "../../x", "tower": "b", "name": "c",
                                                       "label": "L\u001b[31m", "state": "red", "message": "m\nm\u202e"}}),
            "{broken",
            json.dumps({"type": "sync", "ids": ["../../x/b/c"]}),
        ])
        self.write_config(name="evil", command=["printf", "%s\\n", feed])
        s = self.bar()
        v = s["districts"][0]["towers"][0]["lookouts"][0]
        self.assertEqual((v["district"], v["label"], v["message"]), ("unnamed", "L", "m m"))

    def test_bad_config_says_so_in_the_bar(self):
        self.config.write_text("[[source]]\nname = 'x'\n")  # no command
        p = self.ft("bar", ok=False)
        self.assertIn("no command", p.stdout)
        p = self.ft("bar", "--format", "waybar", ok=False)
        self.assertEqual(json.loads(p.stdout)["class"], "red")


class Shown(Remote):
    """hide and keep: which of a source's towers (or this machine's) a board shows."""

    def setUp(self):
        super().setUp()
        for who, state in [("alice", "green"), ("bob", "red"), ("carol", "yellow")]:
            self.on_other("set", f"inbox/{who}/new", state)
        self.on_other("set", "site/web/up", "green")

    def shown(self, **opts):
        self.write_config(name="shared", command=[*self.remote_cmd, "watch", "--json"], **opts)
        return self.bar()

    def test_hide_only(self):
        s = self.shown(hide=["inbox/*"])
        self.assertEqual(self.ids(s), ["shared:site/web/up"])

    def test_keep_wins_over_hide(self):
        s = self.shown(hide=["inbox/*"], keep=["inbox/alice"])
        self.assertEqual(sorted(self.ids(s)), ["shared:inbox/alice/new", "shared:site/web/up"])

    def test_keep_without_hide_changes_nothing(self):
        self.assertEqual(len(self.ids(self.shown(keep=["inbox/alice"]))), 4)

    def test_pattern_without_a_slash_is_the_tower(self):
        s = self.shown(hide=["bob", "c*"])
        self.assertEqual(sorted(self.ids(s)), ["shared:inbox/alice/new", "shared:site/web/up"])

    def test_hidden_fire_doesnt_colour_its_district(self):
        s = self.shown(hide=["inbox/bob"])
        self.assertEqual(s["worst"], "yellow")
        self.assertNotIn("red", [d["worst"] for d in s["districts"]])
        s = self.shown(hide=["inbox/*"], district="box")  # matched on the source's own names, before folding
        self.assertEqual((s["worst"], self.ids(s)), ("green", ["shared:site/web/up"]))

    def test_shell_negation(self):
        s = self.shown(hide=["inbox/[!b]*"])  # the shell's [!...], not only Go's [^...]
        self.assertEqual(sorted(self.ids(s)), ["shared:inbox/bob/new", "shared:site/web/up"])

    def test_matched_on_the_names_shown(self):
        # A feed's name that isn't one shows as "unnamed", and is hidden by that name.
        feed = "\n".join([
            json.dumps({"type": "hello", "v": 1}),
            json.dumps({"type": "lookout", "lookout": {"id": "inbox/x/bob/new", "district": "inbox/x",
                                                       "tower": "bob", "name": "new", "state": "red"}}),
            json.dumps({"type": "lookout", "lookout": {"id": "a/b/c", "district": "a", "tower": "b",
                                                       "name": "c", "state": "green"}}),
            json.dumps({"type": "sync", "ids": ["inbox/x/bob/new", "a/b/c"]}),
        ])
        self.write_config(name="odd", command=["printf", "%s\\n", feed], hide=["unnamed/*"])
        s = self.bar()
        self.assertEqual((self.ids(s), s["worst"]), (["odd:a/b/c"], "green"))

    def test_hidden_lookouts_cant_be_acted_on(self):
        self.write_config(name="shared", command=[*self.remote_cmd, "watch", "--json"],
                          act=self.remote_cmd, hide=["inbox/bob"])
        req = self.start_board()
        for _ in range(50):  # until the source's first sync
            st = json.loads(req("GET", "/api/state")[1])
            if st["sources"][0]["contact"] == "ok":
                break
            time.sleep(0.2)
        ids = [v["id"] for v in st["lookouts"]]
        self.assertIn("shared:inbox/carol/new", ids)
        self.assertNotIn("shared:inbox/bob/new", ids)
        self.assertNotEqual(req("POST", "/api/seen", {"id": "shared:inbox/bob/new"})[0], 200)
        self.assertEqual(req("POST", "/api/seen", {"id": "shared:inbox/carol/new"})[0], 200)

    def test_hidden_or_unknown_ids_reach_no_button_and_no_link(self):
        (self.tmp / "log").write_text("secret log\n")
        self.ft("set", "inbox/alice/new", "red", "--link", str(self.tmp / "log"))
        self.config.write_text('[board]\nhide = ["inbox/*"]\n')
        req = self.start_board()
        for verb in ("done", "rm"):
            self.assertNotEqual(req("POST", f"/api/{verb}", {"id": "inbox/alice/new"})[0], 200, verb)
        status, body = req("GET", "/api/link?id=inbox/alice/new")
        self.assertNotEqual(status, 200)
        self.assertNotIn("secret", body)
        # Watch again books one that's there: a button never makes a new one.
        self.assertNotEqual(req("POST", "/api/expect", {"id": "app/new/one"})[0], 200)
        self.assertNotIn("app/new/one", self.ls())

    def test_this_machines_hidden_lookouts_cant_be_acted_on(self):
        self.ft("set", "inbox/alice/new", "red")
        self.ft("set", "site/web/up", "yellow")
        self.config.write_text('[board]\nhide = ["inbox/*"]\n')
        req = self.start_board()
        for verb in ("seen", "done", "rm", "expect"):
            self.assertNotEqual(req("POST", f"/api/{verb}", {"id": "inbox/alice/new"})[0], 200, verb)
        self.assertNotEqual(req("POST", "/api/expect", {"id": "inbox/new/one"})[0], 200)
        self.assertEqual(self.one("inbox/alice/new")["state"], "red")
        self.assertNotIn("inbox/new/one", self.ls())
        self.assertEqual(req("POST", "/api/seen", {"id": "site/web/up"})[0], 200)
        self.config.write_text('[board]\nlocal = false\n')
        req = self.start_board()
        for verb in ("seen", "done", "rm", "expect"):
            self.assertNotEqual(req("POST", f"/api/{verb}", {"id": "site/web/up"})[0], 200, verb)
        self.assertEqual(self.one("site/web/up")["state"], "yellow")  # not done, not removed

    def test_bad_patterns_refuse_to_start(self):
        bad = ["[a", "a[", "*[", "[]a]", "a/b/c", "", "inbox/", "/bob", "Bob", " bob", "inbox\\*",
               "_*", ".x", "-x", "a!b", "!", "^x", "a" * 65]
        for pat in bad:
            for field in ("hide", "keep"):
                for where in ("source", "board"):
                    with self.subTest(pat=pat, field=field, where=where):
                        if where == "source":
                            self.write_config(name="shared", command=[*self.remote_cmd, "watch", "--json"], **{field: [pat]})
                            named = 'source "shared"'
                        else:
                            self.config.write_text(f"[board]\n{field} = {json.dumps([pat])}\n")
                            named = "[board]"
                        p = self.ft("bar", ok=False)
                        self.assertIn(f"{named}'s {field} pattern {json.dumps(pat)}", p.stdout)
                        p = self.ft("board", "--port", str(free_port()), ok=False)
                        self.assertIn("pattern", p.stdout + p.stderr)

    def test_wrong_types_refuse_and_empty_lists_are_nothing(self):
        self.config.write_text('[board]\nhide = "inbox/*"\n')
        self.ft("bar", ok=False)
        self.write_config(name="shared", command=[*self.remote_cmd, "watch", "--json"], hide=[], keep=[])
        self.assertEqual(len(self.ids(self.bar())), 4)

    def test_misspelt_setting_refuses(self):
        self.write_config(name="shared", command=[*self.remote_cmd, "watch", "--json"], hid=["inbox/*"])
        self.assertIn("unknown setting", self.ft("bar", ok=False).stdout)

    def test_this_machines_own_towers(self):
        self.ft("set", "inbox/alice/new", "red")
        self.ft("set", "inbox/bob/new", "yellow")
        self.ft("set", "site/web/up", "green")
        self.config.write_text('[board]\nhide = ["inbox/*"]\n')
        s = self.bar()
        self.assertEqual((self.ids(s), s["worst"]), (["site/web/up"], "green"))
        self.config.write_text('[board]\nhide = ["inbox/*"]\nkeep = ["bob"]\n')
        s = self.bar()
        self.assertEqual((self.ids(s), s["worst"]), (["inbox/bob/new", "site/web/up"], "yellow"))
        # ls is the files, not a board: it still shows everything.
        self.assertIn("inbox/alice/new", self.ft("ls").stdout)


class Examples(Firetower):
    def test_example_settings_are_valid_turned_on(self):
        # The optional lines are comments; turned on, they still load.
        text = (Path(__file__).parent.parent / "examples/config.toml").read_text()
        on = "\n".join(l[2:] if l.startswith(("# hide", "# keep", "# local")) else l for l in text.splitlines())
        self.assertIn("\nhide = ", on)
        self.config.write_text(on)
        p = self.ft("bar", "--format", "json", "--timeout", "1s", ok=None)
        self.assertNotIn("config", p.stdout + p.stderr)

    def test_example_config_is_valid(self):
        from _binary import ROOT
        p = self.ft("bar", "--format", "json", "--timeout", "2s", FIRETOWER_CONFIG=str(ROOT / "examples/config.toml"))
        self.assertEqual(json.loads(p.stdout)["sources"][0]["name"], "work-mac")


class Bar(Firetower):
    def test_plain(self):
        self.assertEqual(self.ft("bar").stdout.strip(), "no lookouts")
        self.ft("set", "a", "green")
        self.assertEqual(self.ft("bar").stdout.strip(), "all clear (1)")
        self.ft("set", "b", "red")
        self.ft("set", "c", "yellow")
        self.ft("seen", "c")
        self.assertEqual(self.ft("bar").stdout.strip(), "1 fire · 1 seen · 1 clear")

    def test_waybar_escapes_markup(self):
        self.ft("set", "a", "red", "<b>bold</b> & co", "--label", "<i>x</i>")
        w = json.loads(self.ft("bar", "--format", "waybar").stdout)
        self.assertEqual(w["class"], "red")
        self.assertIn("&lt;b&gt;bold&lt;/b&gt; &amp; co", w["tooltip"])
        self.assertNotIn("<i>", w["tooltip"])

    def test_swiftbar_cant_be_hijacked(self):
        self.ft("set", "a", "red", "--label", "-- evil", "--link", "javascript:alert(1)",
                "--", "--- | bash=/bin/rm param1=-rf")
        self.ft("set", "b", "green", "ok", "--link", "https://example.com/a b\" href=x")
        out = self.ft("bar", "--format", "swiftbar").stdout
        for line in out.splitlines():
            if "evil" in line or "bash" in line:
                text, _, params = line.partition(" | ")
                self.assertNotIn("bash=", params)
                self.assertNotIn("href", params)
        self.assertNotIn("javascript", out)
        self.assertNotIn(" href=x", out)  # encoded into the one URL, never a second parameter

    def test_template(self):
        self.ft("set", "a", "red")
        out = self.ft("bar", "--template", "{{.Worst}} {{word .Worst}} {{.Count}}").stdout
        self.assertEqual(out, "red fire 1")
        self.ft("bar", "--template", "{{.Nope}}", ok=False)


class Board(Firetower):
    def setUp(self):
        super().setUp()
        self.port = free_port()
        env = self.env()
        env.pop("FIRETOWER_NOW")
        self.proc = subprocess.Popen([BIN, "board", "--port", str(self.port)], env=env,
                                     stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        self.addCleanup(lambda: (self.proc.terminate(), self.proc.wait(5), self.proc.stdout.close()))
        self.assertIn("is up", self.proc.stdout.readline())
        self.key = (self.dir / ".board-key").read_text().strip()
        self.ft("set", "app/signup/otp", "red", "down", "--link", str(self.tmp / "check.log"))
        (self.tmp / "check.log").write_text("the log\n")
        time.sleep(1.5)  # the board reads the files every second

    def req(self, method, path, body=None, host=None, headers=None, key=True):
        c = http.client.HTTPConnection("127.0.0.1", self.port, timeout=10)
        h = {"Host": host or f"127.0.0.1:{self.port}"}
        if key is True:
            h["Authorization"] = f"Bearer {self.key}"
        elif key:
            h["Authorization"] = f"Bearer {key}"
        h.update(headers or {})
        c.request(method, path, body=json.dumps(body) if body is not None else None, headers=h)
        r = c.getresponse()
        data = r.read().decode()
        c.close()
        self.assertIsNone(r.getheader("Set-Cookie"))  # never: cookies on 127.0.0.1 reach every port
        return r, data

    def test_state(self):
        r, d = self.req("GET", "/api/state")
        self.assertEqual(r.status, 200)
        self.assertEqual([v["id"] for v in json.loads(d)["lookouts"]], ["app/signup/otp"])

    def test_page_and_its_headers(self):
        r, d = self.req("GET", "/", key=False)  # the page is only code; the data needs the key
        self.assertEqual(r.status, 200)
        self.assertIn("board.js", d)
        self.assertNotIn("otp", d)
        self.assertIn("frame-ancestors 'none'", r.getheader("Content-Security-Policy"))
        self.assertEqual(r.getheader("X-Frame-Options"), "DENY")

    def test_no_key_no_data(self):
        # Anyone on the machine can reach loopback; only the key holder sees it.
        for path in ("/api/state", "/api/feed", "/api/link?id=app/signup/otp"):
            for key in (False, "wrong" * 10):
                r, d = self.req("GET", path, key=key)
                self.assertEqual(r.status, 401, path)
                self.assertNotIn("the log", d)
        r, _ = self.req("GET", "/api/state", key=False, headers={"Cookie": f"firetower={self.key}"})
        self.assertEqual(r.status, 401)  # a cookie is nothing
        r, _ = self.req("GET", "/api/state?key=" + self.key, key=False)
        self.assertEqual(r.status, 401)  # the address carries it only where headers can't

    def test_key_in_the_address_where_headers_cant_go(self):
        r, d = self.req("GET", "/api/link?id=app/signup/otp&key=" + self.key, key=False)
        self.assertEqual((r.status, d), (200, "the log\n"))

    def test_other_hosts_get_nothing(self):
        # DNS rebinding: another name pointed at 127.0.0.1.
        r, _ = self.req("GET", "/api/state", host=f"evil.example:{self.port}")
        self.assertEqual(r.status, 421)
        r, _ = self.req("GET", "/api/state", host="127.0.0.1")
        self.assertEqual(r.status, 421)

    def test_writes(self):
        body = {"id": "app/signup/otp"}
        r, _ = self.req("POST", "/api/seen", body, key=False)
        self.assertEqual(r.status, 401)
        r, _ = self.req("POST", "/api/seen", body, headers={"Origin": "http://127.0.0.1:8000"})
        self.assertEqual(r.status, 403)  # another local server's page, even holding the key
        self.assertFalse(self.one("app/signup/otp")["seen"])
        r, _ = self.req("POST", "/api/seen", body, headers={"Origin": f"http://127.0.0.1:{self.port}"})
        self.assertEqual(r.status, 200)
        self.assertTrue(self.one("app/signup/otp")["seen"])
        r, _ = self.req("POST", "/api/done", body)  # a script: no Origin
        self.assertEqual(r.status, 200)
        self.assertEqual(self.one("app/signup/otp")["state"], "done")

    def test_actions_refuse_bad_ids(self):
        for bad in ("../../etc/passwd", "a/b", "x:y/z/w", "app/signup/../otp"):
            r, _ = self.req("POST", "/api/rm", {"id": bad})
            self.assertNotEqual(r.status, 200, bad)
        r, _ = self.req("POST", "/api/nuke", {"id": "app/signup/otp"})
        self.assertIn(r.status, (404, 405))
        self.assertIn("app/signup/otp", self.ls())

    def test_link_shows_only_its_own_file(self):
        r, d = self.req("GET", "/api/link?id=app/signup/otp")
        self.assertEqual((r.status, d), (200, "the log\n"))
        self.assertTrue(r.getheader("Content-Type").startswith("text/plain"))
        self.assertIn("sandbox", r.getheader("Content-Security-Policy"))
        r, _ = self.req("GET", "/api/link?id=" + urllib.parse.quote("../../../etc/passwd"))
        self.assertEqual(r.status, 404)
        fifo = self.tmp / "fifo"
        os.mkfifo(fifo)
        self.ft("set", "app/signup/dir", "green", "--link", str(self.tmp))
        self.ft("set", "app/signup/web", "green", "--link", "https://example.com")
        self.ft("set", "app/signup/fifo", "green", "--link", str(fifo))
        time.sleep(1.5)
        for id in ("app/signup/dir", "app/signup/web", "app/signup/fifo"):
            r, _ = self.req("GET", "/api/link?id=" + id)  # a FIFO must not hang it
            self.assertEqual(r.status, 404, id)

    def test_huge_log_is_tailed(self):
        big = self.tmp / "big.log"
        big.write_bytes(b"x" * (1024 * 1024) + b"\nTHE END\n")
        self.ft("set", "app/signup/big", "green", "--link", str(big))
        time.sleep(1.5)
        r, d = self.req("GET", "/api/link?id=app/signup/big")
        self.assertLess(len(d), 300 * 1024)
        self.assertTrue(d.endswith("THE END\n"))

    def test_feed_streams(self):
        c = http.client.HTTPConnection("127.0.0.1", self.port, timeout=10)
        self.addCleanup(c.close)
        c.request("GET", "/api/feed?key=" + self.key, headers={"Host": f"127.0.0.1:{self.port}"})
        r = c.getresponse()
        self.assertEqual(r.getheader("Content-Type"), "text/event-stream")
        self.assertEqual(r.readline().strip(), b"event: state")
        first = json.loads(r.readline().decode()[len("data: "):])
        self.assertEqual(len(first["lookouts"]), 1)
        self.ft("set", "app/signup/new", "green")
        deadline = time.time() + 5
        while time.time() < deadline:
            line = r.readline().decode()
            if line.startswith("data: ") and len(json.loads(line[6:])["lookouts"]) == 2:
                break
        else:
            self.fail("the feed didn't send the new lookout")
        c.close()

    def test_second_board_finds_the_first(self):
        env = self.env()
        env.pop("FIRETOWER_NOW")
        p = subprocess.run([BIN, "board", "--port", str(self.port)], env=env, capture_output=True, text=True, timeout=10)
        self.assertEqual(p.returncode, 0)
        self.assertIn("already up", p.stdout)

    def test_url(self):
        out = self.ft("board", "--url", "--port", str(self.port)).stdout.strip()
        self.assertEqual(out, f"http://127.0.0.1:{self.port}/?key={self.key}")
        self.assertEqual(oct((self.dir / ".board-key").stat().st_mode & 0o777), "0o600")


class Key(Firetower):
    def test_made_once_however_many_ask(self):
        # SwiftBar runs bar every few seconds; a board may start at the same
        # moment. They must agree on one key.
        env = self.env()
        ps = [subprocess.Popen([BIN, "board", "--url"], env=env, stdout=subprocess.PIPE, text=True) for _ in range(12)]
        urls = {p.communicate()[0].strip() for p in ps}
        self.assertEqual(len(urls), 1, urls)


class CustomUI(Firetower):
    def test_own_page(self):
        ui = self.tmp / "ui"
        ui.mkdir()
        (ui / "index.html").write_text("<h1>mine</h1>")
        port = free_port()
        env = self.env()
        env.pop("FIRETOWER_NOW")
        p = subprocess.Popen([BIN, "board", "--port", str(port), "--ui", str(ui)], env=env,
                             stdout=subprocess.PIPE, text=True)
        self.addCleanup(lambda: (p.terminate(), p.wait(5), p.stdout.close()))
        p.stdout.readline()
        c = http.client.HTTPConnection("127.0.0.1", port, timeout=5)
        c.request("GET", "/")
        self.assertIn("mine", c.getresponse().read().decode())
        c.close()

    def test_ui_needs_an_index(self):
        self.ft("board", "--ui", str(self.tmp), "--port", str(free_port()), ok=False)
