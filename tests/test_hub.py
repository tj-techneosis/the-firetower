"""The board as the machine's hub: bar and the CLI's actions ask a running
board first, and do the work themselves only when none is up. And `board
--start`, `--open` and `--stop`, which never tie a board to whoever asked."""

import json
import os
import socket
import subprocess
import threading
import time

from _binary import BIN, Firetower


def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


class HubBase(Firetower):
    def setUp(self):
        super().setUp()
        self.port = free_port()
        self.other = self.tmp / "other"
        self.dials = self.tmp / "dials"
        self.remote = ["env", f"FIRETOWER_DIR={self.other}", "FIRETOWER_CONFIG=/nonexistent", BIN]
        # The source counts every time something dials it.
        dial = ["sh", "-c", f'echo x >> "{self.dials}"; exec "$@"', "dial", *self.remote, "watch", "--json"]
        self.write_config(f'[board]\nport = {self.port}\nhide = ["secret"]\n'
                          f'[[source]]\nname = "box"\ncommand = {json.dumps(dial)}\nact = {json.dumps(self.remote)}\nhide = ["inbox/bob"]\n')
        self.on_other("set", "inbox/carol/new", "yellow", "two unread")
        self.on_other("set", "inbox/bob/new", "red")
        self.ft("set", "app/web/up", "green")
        self.addCleanup(lambda: self.ft("board", "--stop", ok=None, **self.real_clock()))

    def write_config(self, text):
        self.config.write_text(text)

    def real_clock(self):
        return {"FIRETOWER_NOW": ""}

    def env(self, **extra):
        e = super().env(**extra)
        if e.get("FIRETOWER_NOW") == "":
            e.pop("FIRETOWER_NOW")
        return e

    def on_other(self, *args):
        subprocess.run([*self.remote, *args], check=True, capture_output=True)

    def other_one(self, id):
        out = subprocess.run([*self.remote, "ls", "--json", "--all"], check=True, capture_output=True, text=True).stdout
        return {v["id"]: v for v in json.loads(out)}.get(id)

    def dial_count(self):
        return len(self.dials.read_text().splitlines()) if self.dials.exists() else 0

    def start(self):
        t = time.time()
        p = self.ft("board", "--start", **self.real_clock())
        self.assertLess(time.time() - t, 6)
        for _ in range(100):  # until the source's first sync
            s = self.bar()
            if s["sources"] and s["sources"][0]["contact"] == "ok":
                return p
            time.sleep(0.1)
        self.fail("the board's source never synced")

    def bar(self, *args):
        return json.loads(self.ft("bar", "--format", "json", *args, **self.real_clock()).stdout)

    def ids(self, s):
        return sorted(v["id"] for d in s["districts"] for t in d["towers"] for v in t["lookouts"])


class Hub(HubBase):
    def test_bar_reads_a_running_board_and_dials_nothing(self):
        self.start()
        dialed = self.dial_count()
        self.assertEqual(dialed, 1)  # the board's own, once
        for _ in range(3):
            s = self.bar()
            self.assertTrue(s["board"])
            self.assertEqual(self.ids(s), ["app/web/up", "box:inbox/carol/new"])  # the board's hide, too
        self.assertEqual(self.dial_count(), dialed)

    def test_bar_with_no_board_dials_and_says_so(self):
        s = self.bar()
        self.assertFalse(s["board"])
        self.assertEqual(self.ids(s), ["app/web/up", "box:inbox/carol/new"])
        self.assertEqual(self.dial_count(), 1)
        self.assertIn("No board is running", self.ft("bar", "--format", "swiftbar", **self.real_clock()).stdout)

    def test_a_board_that_wont_answer_is_passed_over(self):
        # Something holds the port and never answers: the bar gives up on it
        # quickly and dials the sources itself.
        self.ft("board", "--url")  # a key, as a board that once ran leaves
        srv = socket.socket()
        srv.bind(("127.0.0.1", self.port))
        srv.listen(8)
        held = []
        stop = threading.Event()

        def hold():
            srv.settimeout(0.2)
            while not stop.is_set():
                try:
                    held.append(srv.accept()[0])
                except OSError:
                    pass
        threading.Thread(target=hold, daemon=True).start()
        self.addCleanup(lambda: (stop.set(), srv.close(), [c.close() for c in held]))
        t = time.time()
        s = self.bar("--timeout", "5s")
        self.assertLess(time.time() - t, 8)
        self.assertFalse(s["board"])
        self.assertEqual(self.dial_count(), 1)

    def test_someone_elses_board_is_passed_over(self):
        # Another person's board on the same port: this key isn't its key.
        theirs = self.tmp / "theirs"
        env = {**self.env(**self.real_clock()), "FIRETOWER_DIR": str(theirs), "FIRETOWER_CONFIG": "/nonexistent"}
        proc = subprocess.Popen([BIN, "board", "--port", str(self.port)], env=env, stdout=subprocess.PIPE, text=True)
        self.addCleanup(lambda: (proc.terminate(), proc.wait(5), proc.stdout.close()))
        self.assertIn("is up", proc.stdout.readline())
        self.ft("board", "--url")
        s = self.bar()
        self.assertFalse(s["board"])
        self.assertEqual(self.ids(s), ["app/web/up", "box:inbox/carol/new"])

    def test_seen_on_another_machine_through_the_board(self):
        self.start()
        self.ft("seen", "box:inbox/carol/new", **self.real_clock())
        self.assertTrue(self.other_one("inbox/carol/new")["seen"])
        p = self.ft("seen", "box:inbox/bob/new", ok=False, **self.real_clock())  # hidden here
        self.assertIn("no lookout", p.stderr)
        self.assertFalse(self.other_one("inbox/bob/new")["seen"])
        self.ft("done", "box:inbox/carol/new", **self.real_clock())
        self.assertEqual(self.other_one("inbox/carol/new")["state"], "done")
        self.ft("rm", "box:inbox/carol/new", **self.real_clock())
        self.assertIsNone(self.other_one("inbox/carol/new"))

    def test_seen_on_another_machine_with_no_board(self):
        self.ft("seen", "box:inbox/carol/new")
        self.assertTrue(self.other_one("inbox/carol/new")["seen"])
        self.assertEqual(self.dial_count(), 0)  # straight to its act command; nothing dialed
        p = self.ft("seen", "box:inbox/bob/new", ok=False)
        self.assertIn("no lookout", p.stderr)
        self.assertFalse(self.other_one("inbox/bob/new")["seen"])
        self.assertIn("no source", self.ft("seen", "nope:a/b/c", ok=False).stderr)
        self.ft("seen", "box:../etc/passwd", ok=False)
        self.ft("seen", "box:a/b", ok=False)

    def test_no_message_goes_to_another_machine(self):
        # It would cross a shell over there: refused, and nothing changes.
        p = self.ft("done", "box:inbox/carol/new", "x; touch /tmp/pwned", ok=False)
        self.assertIn("message", p.stderr)
        self.assertEqual(self.other_one("inbox/carol/new")["state"], "yellow")

    def whoami(self):
        key = (self.dir / ".board-key").read_text().strip()
        import http.client
        c = http.client.HTTPConnection("127.0.0.1", self.port, timeout=5)
        c.request("GET", "/api/whoami", headers={"Host": f"127.0.0.1:{self.port}", "Authorization": f"Bearer {key}"})
        r = c.getresponse()
        out = json.loads(r.read())
        c.close()
        return out

    def test_start_twice_is_one_board_and_stop_ends_it(self):
        self.start()
        pid = self.whoami()["pid"]
        self.ft("board", "--start", **self.real_clock())
        self.assertEqual(self.whoami()["pid"], pid)
        self.assertEqual(self.ft("board", "--stop", **self.real_clock()).stdout.strip(), "Stopped the board.")
        self.assertFalse(self.bar()["board"])
        self.assertIn("No board", self.ft("board", "--stop", **self.real_clock()).stdout)

    def test_two_starts_at_once_are_one_board(self):
        procs = [subprocess.Popen([BIN, "board", "--start"], env=self.env(**self.real_clock()),
                                  stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True) for _ in range(3)]
        outs = [p.communicate(timeout=10) for p in procs]
        self.assertEqual([p.returncode for p in procs], [0, 0, 0], outs)
        self.assertTrue(self.bar()["board"])
        self.assertNotIn("\x00", (self.dir / "board.log").read_text())

    def test_a_board_by_hand_is_stopped_with_a_word_about_services(self):
        proc = subprocess.Popen([BIN, "board"], env=self.env(**self.real_clock()), stdout=subprocess.PIPE, text=True)
        self.addCleanup(lambda: (proc.poll() is None and proc.terminate(), proc.wait(5), proc.stdout.close()))
        self.assertIn("is up", proc.stdout.readline())
        self.assertIn("service", self.ft("board", "--stop", **self.real_clock()).stdout)
        self.assertEqual(proc.wait(5), 0)

    def test_someone_elses_board_is_never_stopped_or_opened(self):
        theirs = self.tmp / "theirs"
        env = {**self.env(**self.real_clock()), "FIRETOWER_DIR": str(theirs), "FIRETOWER_CONFIG": "/nonexistent"}
        proc = subprocess.Popen([BIN, "board", "--port", str(self.port)], env=env, stdout=subprocess.PIPE, text=True)
        self.addCleanup(lambda: (proc.terminate(), proc.wait(5), proc.stdout.close()))
        self.assertIn("is up", proc.stdout.readline())
        self.ft("board", "--url")
        self.assertIn("prove it's yours", self.ft("board", "--stop", ok=False, **self.real_clock()).stderr)
        got = self.tmp / "opened"
        p = self.ft("board", "--open", ok=False, FIRETOWER_OPEN_WITH=f"sh -c 'echo $0 >> {got}'", **self.real_clock())
        self.assertIn("prove it's yours", p.stderr)
        self.assertFalse(got.exists())
        self.assertIsNone(proc.poll())

    def test_open_starts_one_in_the_background_and_opens_it(self):
        got = self.tmp / "opened"
        opener = self.tmp / "opener.sh"
        opener.write_text(f'#!/bin/sh\necho "$1" >> "{got}"\n')
        opener.chmod(0o755)
        t = time.time()
        self.ft("board", "--open", FIRETOWER_OPEN_WITH=str(opener), **self.real_clock())
        self.assertLess(time.time() - t, 6)
        for _ in range(50):
            if got.exists():
                break
            time.sleep(0.1)
        url = got.read_text().strip()
        key = (self.dir / ".board-key").read_text().strip()
        # A one-time address, never the key: a command line is everyone's to read.
        self.assertNotIn(key, url)
        self.assertRegex(url, rf"^http://127\.0\.0\.1:{self.port}/\?once=[0-9a-f]{{48}}$")
        once = url.split("once=")[1]
        self.assertEqual(self.claim(once), (200, key))
        self.assertEqual(self.claim(once)[0], 401)  # once
        self.assertTrue(self.bar()["board"])  # still up: it outlived the command that opened it
        self.ft("board", "--open", FIRETOWER_OPEN_WITH=str(opener), **self.real_clock())
        time.sleep(0.3)
        self.assertEqual(len(got.read_text().splitlines()), 2)  # a window again, not a second board

    def claim(self, once):
        import http.client
        c = http.client.HTTPConnection("127.0.0.1", self.port, timeout=5)
        c.request("POST", "/api/claim", body=json.dumps({"once": once}),
                  headers={"Host": f"127.0.0.1:{self.port}", "Content-Type": "application/json"})
        r = c.getresponse()
        body = json.loads(r.read() or b"{}")
        c.close()
        return r.status, body.get("key")

    def test_url_once_is_a_one_time_address(self):
        self.start()
        url = self.ft("board", "--url", "--once").stdout.strip()
        self.assertEqual(self.claim(url.split("once=")[1])[0], 200)
        self.assertEqual(self.claim("0" * 48)[0], 401)
        self.ft("board", "--once", ok=False)

    def test_a_board_that_cant_start_says_why(self):
        self.write_config('[board]\nui = "/no/such/page"\n')
        p = self.ft("board", "--start", ok=False, **self.real_clock())
        self.assertIn("no index.html", p.stderr)

    def test_flags_go_one_at_a_time(self):
        self.ft("board", "--start", "--stop", ok=False)
        self.ft("board", "--open", "--start", ok=False)


class Stranger(HubBase):
    """Someone else's program holds the board's port, answering like a board:
    it never gets the key, and it isn't believed."""

    def setUp(self):
        super().setUp()
        import http.server
        heard = self.heard = []
        forged = json.dumps({"at": "2026-10-31T12:00:00Z", "lookouts": [], "sources": []}).encode()

        class Fake(http.server.BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def answer(self):
                heard.append((self.command, self.path, self.headers.get("Authorization")))
                n = int(self.headers.get("Content-Length") or 0)
                if n:
                    self.rfile.read(n)
                body = json.dumps({"proof": "0" * 64}).encode() if self.path.startswith("/api/proof") else forged
                self.send_response(200)
                self.send_header("X-Firetower-Board", "1")
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
            do_GET = do_POST = answer

        self.srv = http.server.ThreadingHTTPServer(("127.0.0.1", self.port), Fake)
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()
        self.addCleanup(lambda: (self.srv.shutdown(), self.srv.server_close()))
        self.ft("board", "--url")  # a key, as a board that once ran leaves

    def test_bar_never_sends_the_key_or_believes_it(self):
        s = self.bar()
        self.assertFalse(s["board"])
        self.assertEqual(self.ids(s), ["app/web/up", "box:inbox/carol/new"])  # the real picture, not its "nothing"
        self.assertTrue(self.heard)  # it was asked to prove itself
        self.assertEqual([h for h in self.heard if h[2]], [])  # and never handed the key

    def test_actions_go_to_the_machine_not_the_stranger(self):
        self.ft("seen", "box:inbox/carol/new", **self.real_clock())
        self.assertTrue(self.other_one("inbox/carol/new")["seen"])  # through act, since no board of ours answered
        self.assertEqual([h for h in self.heard if h[0] == "POST"], [])
        self.assertEqual([h for h in self.heard if h[2]], [])

    def test_start_says_the_port_is_taken(self):
        p = self.ft("board", "--start", ok=False, **self.real_clock())
        self.assertIn("prove it's yours", p.stderr)


class Crossing(HubBase):
    def test_place_flags_are_refused_with_another_machines_name(self):
        for verb in ("seen", "done", "rm"):
            p = self.ft(verb, "box:inbox/carol/new", "--district", "x", ok=False)
            self.assertIn("already says where", p.stderr)
        self.assertEqual(self.other_one("inbox/carol/new")["state"], "yellow")

    def test_no_message_even_after_dashes(self):
        p = self.ft("done", "box:inbox/carol/new", "--", "--rm -rf", ok=False)
        self.assertIn("message", p.stderr)
        self.assertEqual(self.other_one("inbox/carol/new")["state"], "yellow")

    def test_a_board_that_failed_isnt_tried_again_another_way(self):
        # The board runs act, act fails: that's the answer, and act ran once.
        count = self.tmp / "acts"
        failing = ["sh", "-c", f'echo x >> "{count}"; echo "act failed" >&2; exit 1', "act"]
        dial = ["sh", "-c", 'exec "$@"', "dial", *self.remote, "watch", "--json"]
        self.write_config(f'[board]\nport = {self.port}\n'
                          f'[[source]]\nname = "box"\ncommand = {json.dumps(dial)}\nact = {json.dumps(failing)}\n')
        self.start()
        p = self.ft("seen", "box:inbox/carol/new", ok=False, **self.real_clock())
        self.assertIn("act failed", p.stderr)
        self.assertEqual(len(count.read_text().splitlines()), 1)

    def test_the_ids_on_other_machines_are_held_to_names(self):
        for bad in ("box:../etc/passwd", "box:a/b", "box:a/b/c/d", "box:a/-x/c"):
            p = self.ft("seen", bad, ok=False)
            self.assertTrue(p.stderr.strip(), bad)
        self.assertEqual(self.dial_count(), 0)


class Relay(HubBase):
    """A stranger on board.port passes the proof question to the person's
    real board on another port: the answer is for that port, so it fails."""

    def test_a_relayed_proof_gets_no_key(self):
        import http.server
        import urllib.request
        real = free_port()
        self.ft("board", "--start", "--port", str(real), **self.real_clock())
        self.addCleanup(lambda: self.ft("board", "--stop", "--port", str(real), ok=None, **self.real_clock()))
        heard = []

        class Relay(http.server.BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_GET(self):
                heard.append(self.headers.get("Authorization"))
                body = b"{}"
                if self.path.startswith("/api/proof"):
                    with urllib.request.urlopen(f"http://127.0.0.1:{real}{self.path}") as r:
                        body = r.read()
                self.send_response(200)
                self.send_header("X-Firetower-Board", "1")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
            do_POST = do_GET

        srv = http.server.ThreadingHTTPServer(("127.0.0.1", self.port), Relay)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        self.addCleanup(lambda: (srv.shutdown(), srv.server_close()))
        s = self.bar()
        self.assertFalse(s["board"])
        self.assertEqual([h for h in heard if h], [])


class Guard(HubBase):
    def req(self, method, path, body=None, headers=None):
        import http.client
        c = http.client.HTTPConnection("127.0.0.1", self.port, timeout=5)
        h = {"Host": f"127.0.0.1:{self.port}", **(headers or {})}
        c.request(method, path, body=json.dumps(body) if body is not None else None, headers=h)
        r = c.getresponse()
        out = r.status, r.read().decode()
        c.close()
        return out

    def test_the_new_endpoints_hold(self):
        self.start()
        key = (self.dir / ".board-key").read_text().strip()
        self.assertEqual(self.req("GET", "/api/proof?nonce=xyz")[0], 400)
        self.assertEqual(self.req("GET", "/api/whoami")[0], 401)
        self.assertEqual(self.req("GET", "/api/whoami", headers={"Authorization": f"Bearer {key}"})[0], 200)
        once = self.ft("board", "--url", "--once").stdout.strip().split("once=")[1]
        self.assertEqual(self.req("POST", "/api/claim", {"once": once}, {"Origin": "http://evil.example"})[0], 403)
        self.assertEqual(self.req("POST", "/api/claim", {"once": "g" * 48})[0], 401)
        status, _ = self.req("GET", "/api/proof?nonce=" + "0" * 32, headers={"Host": "evil.example"})
        self.assertEqual(status, 421)

    def test_a_stolen_address_changes_the_key(self):
        # Someone read the one-time address and claimed it first; when the
        # real window arrives, the board makes a new key and ends what the
        # thief opened.
        self.start()
        once = self.ft("board", "--url", "--once").stdout.strip().split("once=")[1]
        status, body = self.req("POST", "/api/claim", {"once": once})
        stolen = json.loads(body)["key"]
        self.assertEqual(self.req("GET", "/api/state", headers={"Authorization": f"Bearer {stolen}"})[0], 200)
        status, body = self.req("POST", "/api/claim", {"once": once})
        self.assertEqual((status, json.loads(body)["reason"]), (401, "stolen"))
        self.assertEqual(self.req("GET", "/api/state", headers={"Authorization": f"Bearer {stolen}"})[0], 401)
        fresh = (self.dir / ".board-key").read_text().strip()
        self.assertNotEqual(fresh, stolen)
        self.assertTrue(self.bar()["board"])  # the CLI reads the new key and carries on

    def test_stop_works_with_a_broken_config(self):
        self.start()
        self.config.write_text(self.config.read_text().replace('hide = ["secret"]', 'hide = ["Secret"]'))
        self.ft("bar", ok=False)  # the config is broken now
        self.ft("seen", "box:inbox/carol/new", **self.real_clock())  # still through the board it started with
        self.assertTrue(self.other_one("inbox/carol/new")["seen"])
        self.assertIn("Stopped", self.ft("board", "--stop", **self.real_clock()).stdout)

    def test_a_machine_out_of_contact_is_refused_fast(self):
        # The board knew its lookouts, then lost it: acting waits for nothing.
        flip = self.tmp / "down"
        dial = ["sh", "-c", f'[ -e "{flip}" ] && exit 255; exec "$@"', "dial", *self.remote, "watch", "--json", "--once"]
        self.write_config(f'[board]\nport = {self.port}\n'
                          f'[[source]]\nname = "box"\ncommand = {json.dumps(dial)}\nact = {json.dumps(["sleep", "20"])}\n')
        self.ft("board", "--start", **self.real_clock())
        for _ in range(50):
            s = self.bar()
            if s["sources"] and any(v.get("origin") == "box" for d in s["districts"] for t in d["towers"] for v in t["lookouts"]):
                break
            time.sleep(0.1)
        flip.touch()
        for _ in range(100):
            if self.bar()["sources"][0]["contact"] == "lost":
                break
            time.sleep(0.1)
        t = time.time()
        p = self.ft("seen", "box:inbox/carol/new", ok=False, **self.real_clock())
        self.assertLess(time.time() - t, 5)
        self.assertIn("out of contact", p.stderr)

    def test_a_lost_machine_counts_even_with_every_tower_hidden(self):
        self.write_config(f'[board]\nport = {self.port}\n'
                          '[[source]]\nname = "gone"\ncommand = ["sh", "-c", "exit 255"]\nhide = ["*"]\n')
        s = self.bar()
        self.assertEqual((s["counts"].get("lost"), s["attention"]), (1, 1))
