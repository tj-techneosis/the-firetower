# The Firetower

*A lookout for background watches.*

The Firetower is one board for every background check you have running. Each
job reports one line, and you can see at a glance what's fine, what's broken
and what's gone quiet. No pop-ups and no sessions left open, just one small
tool over plain JSON.

```
$ firetower ls
STATE          LOOKOUT                         CHECKED  MESSAGE
green          website/checkout/deploy-landed  2m ago   Live, build a1b2c3d. (still since 3h ago)
yellow (seen)  app/signup/otp                  4m ago   SMS delivery slow (40s). (since 3h ago, last clear 3h ago)
stale          website/admin/backup            5h ago   Backed up 2.1 GB. (no word since 5h ago, due 4h ago)
red            website/questionnaire/intake    1m ago   Form submit returns 500. (since 40m ago, never clear)
```

It runs no checks, schedules nothing and keeps no clock. A scheduler runs the
check (cron, launchd, systemd, CI, [Crontinental](https://github.com/Techneosis/the-crontinental))
and owns all the timing: how often, for how long, when it stops. The check
says what it saw, and the Firetower is where that lands and is shown. A
report can carry two of the scheduler's times, when the next report is due
and when the watch ends, and the board only compares them with now.

## The service

A fire lookout service, in three layers:

- A **district** is the widest group: a product, a client, a machine. Most
  people have one and never name it (it's `main`).
- A **tower** is one part of a district: a site, a service, a feature.
- A **lookout** is one watch: `website/checkout/deploy-landed`.

Districts and towers exist because lookouts name them, and go when their last
lookout does.

What a lookout can say, and what the board makes of it:

| reported | the board says | means |
|---|---|---|
| `green` | **clear** | the check ran and all is fine |
| `yellow` | **smoke** | worth a look |
| `red` | **fire** | something's wrong |
| `done` | **stood down** | the watch is over; it greys out, then leaves after a day. It stays over: a job still booked can't undo it by reporting again, only `firetower expect` books it again |
| *(past its `next_by`)* | **no word** | the report it promised didn't come |
| *(expected, nothing yet)* | **waiting** | on the board, no first report yet |
| *(its value)* | **moved** / **still** | what it watches changed, or hasn't, and since when |
| *(someone looked)* | **seen** | acknowledged: dim until its status or value changes |

The files and the CLI keep the plain words (`green`, `stale`), so scripts stay
readable. The boards use the lookout's.

## How the lookouts work

Four rules, from years of heartbeat checks that lied:

1. **A fresh report is not a healthy one.** A job that reports its own
   failure is always fresh. So the Firetower keeps the status and its age
   apart: "fire since Tue, last clear 3 days ago".
2. **Silence needs a promise.** A report that says when the next one is due
   (`--next 30m`) is held to it: once that time passes with nothing new, the
   board says **no word**. That's how a dead job shows up. The time is the
   scheduler's; the Firetower keeps no clock and only compares it with now. A
   report with no promise is a record of a moment, never judged.
3. **Never reported is not healthy.** `firetower expect` puts a lookout on the
   board before its first report, so a check that never starts shows
   "waiting", then "no word yet" once its `--next` has passed.
4. **Seen is not fixed.** Acknowledging a smoke dims it until its status or
   value changes, so a known problem stops shouting and a new one gets
   through.

## Reporting

A check reports itself:

```sh
firetower set website/checkout/deploy-landed green "Live, build a1b2c3d." \
  --value a1b2c3d --next 30m --until 23:59 --label "Deploy lands"
```

Or a check that knows nothing about the Firetower is wrapped. Its exit code
is the status (0 clear, 1 smoke, 2 fire, anything else fire too),
its last line of output the message, and a `VALUE: x` line the value:

```sh
firetower run website/site/up --next 5m -- curl -fsS -o /dev/null https://example.com/
```

A check that can't start is fire, never silence. If the scheduler stops it
(its own timeout, a kill), it reports fire on its way out. A mistake in the
`run` line itself (a bad flag, a bad name) is refused before anything runs,
records nothing, and exits 64, so the scheduler's log shows it as a booking
error rather than a failing check. How long a check may
run is the scheduler's business, not the Firetower's: it only takes reports. The command's output and exit
code pass straight through, so the scheduler's own history stays true too.

Book either with whatever runs things on a timer. The scheduler owns the
timing; the report just passes on what the board needs to know:

```sh
crontinental every 10m --until "5 hours" --name site-up --notify never -- \
  firetower run website/site/up -- curl -fsS -o /dev/null https://example.com/
*/5 * * * *  ~/.local/bin/firetower run website/site/up --next 7m -- curl -fsS ...   # cron
```

`--next` and `--until` take a span (`10m`, `5h`, `2w`, `"5 hours"`), a clock
time (`23:59`, the next one; `tomorrow 07:00`), a date (`2026-11-01`, meaning
through that day), a full time (`2026-11-01 06:00`, read in this machine's time zone unless it gives its own,
as Crontinental's do), or `none` to clear it. Units are `s`, `m`, `h`, `d`,
`w` and their words (`hrs`, `days`). A capital `S`, `M` or `H` is refused
rather than guessed (`30M`: minutes, or months?), and the error says to
write it in lowercase; `2D` and `2W` read as days and weeks.
A time already past is accepted, and said so on stderr. For `--until`, a
span or a clock time counts from when the watch was booked (`expect`, or its
first report), not from each run: a booking that repeats `--until 6h` every
run still ends six hours after it began. To move the end of a watch still
on, `expect` it again with a new `--until`, or give a later date or full
time. Once it's over, only `expect` books it again: a report's own times
don't bring it back.

Under Crontinental (0.8 and later) `run` needs neither: Crontinental hands
each run its next start time and its end (`$CRONTINENTAL_NEXT`,
`$CRONTINENTAL_UNTIL`), and the report is due five minutes after that next
start. A check that takes longer than that says its own `--next`, which
always wins. Under cron, launchd or anything else, give `--next` the
interval plus a little slack. Crontinental's last run of a booking (and a
one-off) hands over no next time, so that report promises nothing more: the
lookout keeps its last word and is never judged. A next start already past
(a machine that slept) is due five minutes from now.

A script that ends in `firetower set` gets none of this from Crontinental:
wrap it in `run` (exiting 0, 1 or 2), or give `set` its own `--next`.
Without either, nothing promised a next report, so a dead job's lookout
stays as it last was, never "no word".

The watch ends when its `--until` passes, or when someone stands it down
(`firetower done`, or the board's button). Either way it stays over: a job
still booked that reports again is told so, and the board keeps showing it
over, with what it last said. `firetower expect NAME --next ... --until ...`
books it again. Stopping a hand-run `firetower run` with Ctrl-C reports it as
fire, as a scheduler's stop would; `firetower done` or `rm` clears that.
`rm` takes a lookout off the board, but a job still booked puts it back as a
new watch on its next report: to end a watch, stand it down.

### Who reports, and how it got here

`--source` says who feeds a watch, so whoever reads the board doesn't have
to guess from names: `firetower set backup/nightly/db green "Backed up." --source
cron:nightly-backup`. Under Crontinental, `run` fills it in from the job's
name (`crontinental:<job>`) by itself, for a run Crontinental started (it
sets both `$CRONTINENTAL_JOB` and `$CRONTINENTAL_RUN`), and only when the
watch has no source yet. `firetower ls --json` carries it as
`source`, and the board shows it on hover.

Each lookout also keeps its last 50 changes of status: every time a report's
status differs from the last, the Firetower notes when, the status and what
the check said. `firetower history NAME` shows them, `--since 7d` only
recent ones, and `--json` gives them to a board drawing a timeline. A report
that didn't come isn't an entry; it's the gap between two. A stand-down is
dated when it took effect (an `--until` that passed shows then, not when
someone stood it down), booking a watch again with `expect` shows as
"waiting, booked again", and a watch from before 0.4.0 starts its history at
its current status.

### Tags

A lookout can carry tags, words of your own: `--tag health` on `set`, `run`
or `expect` (repeat it, or join words with commas). Give a report `--tag`
and it carries exactly those; leave it out and it keeps what it had;
`--tag none` clears them.

A tag is for grouping the lookouts the way you think of them, across
districts, towers and machines. A health lookout on every machine shows under
one `#health` on the board's **By tag** view, and `firetower ls --tag health`
or `firetower bar --tag health` reads just those.

It's also how your own workflow hooks in. The Firetower does nothing with a
tag by itself: it keeps it and shows it, and `ls`, `rm` and `gc --tag` are
the handles for a script or a scheduler. A watch tagged `temporary`, gone
once it's over:

```sh
firetower run deploy/web/rollout --tag temporary --until 2h -- ./check-rollout.sh
crontinental every 1h --name tidy-temporary --forever --notify never -- firetower gc --tag temporary
```

`gc --tag temporary` forgets every stood-down lookout carrying the tag right
away (plain `gc` waits a day); `rm --tag` removes them in any state. Both
say on stderr when no lookout carries the tag at all, and when a file they
couldn't read might have, and still exit 0: a tidy-up that finds nothing to
do has done its job.

### All of it

The commands, as `firetower help` lists them (it also explains names,
messages and times):

```
firetower set NAME STATUS ["MESSAGE"] [--district D] [--tower T] [--label L]
              [--value V] [--next 30m|TIME|none] [--until 6h|TIME|none] [--link X] [--source S]
              [--tag T]...
firetower run NAME [the same flags but --value] [--message M] -- CMD ...
firetower expect NAME [--next 30m] [--until 6h] [--district D] [--tower T] [--label L] [--link X]
              [--source S] [--tag T]...

firetower ls [--district D] [--tower T] [--tag T]... [--all] [--json]
firetower history NAME [--since 7d] [--json]
                                   how it got here: its last 50 changes of status
firetower seen NAME              acknowledge the current smoke or fire
firetower done NAME ["MESSAGE"]  stand a watch down (it stays down until expect;
                                   with no MESSAGE it keeps what the watch last said)
firetower rm NAME | --district D [--tower T] | --tag T... [--district D] [--tower T]
firetower gc [--tag T]...        forget lookouts stood down over a day ago
                                   (with --tag: every stood-down one carrying it, now)

firetower board [--port N] [--ui DIR]   the board, here, until Ctrl-C (for a service)
firetower board --open | --start | --stop | --url [--once]
                                   open it in a window (starting it in the background if
                                   it isn't up), only start it, stop it, or print its address
                                   (--once: an address good for one browser, for a minute)
firetower bar [--format plain|json|waybar|swiftbar|xbar] [--template T] [--tag T]...
              [--timeout 8s]
firetower watch --json [--once]  the feed boards read

firetower upgrade [--check] | firetower version | firetower help
```

A NAME is `name`, `tower/name` or `district/tower/name`; the district and
the tower both default to `main`, so a first `firetower set deploy green`
lands at `main/main/deploy`. A bare name that already exists in some tower
reports there (and if two towers have it, says to name which): in a booked
job, use the full `district/tower/name`, so a second tower with the same
name later can't break it. A flag left
out of a report keeps what the lookout had, with one exception: a `--next`
that has already passed is spent once the late report arrives, so the
lookout isn't stuck on "no word". `none` clears `--next` or `--until`. A
lookout expected without `--next` waits for its first report for good:
give `expect` a `--next` for it to turn to "no word yet". At a terminal, a
first report to a new name says where it landed, and a past time or a
stood-down watch is pointed out; under a scheduler those notes stay out of
its log. A message of more than one word goes in quotes; one that starts
with dashes goes after `--`: `firetower set x red -- "--force failed"`.

Messages are on screen, so keep them to one short line with no secrets or
names. The Firetower makes them safe to show either way: newlines become
spaces, terminal escapes and control characters are dropped, and messages are
cut at 200 characters (labels at 80, sources at 120, links at 2000). `run`
records the check's last line of output; with no output it records "Ran
clean." or "Exited N.", and on a failure it adds "(exit N)".

## The boards

Everything a board needs is in the files and the feed, so you can build your
own. You don't have to: two come with it, and each can be swapped out alone.

**The board, at login.** The board is this machine's hub: it follows every
other machine you list, holds the key and sends each button press to the
machine it belongs to. The bar, a bar plugin and `firetower seen` on another
machine's lookout all ask it first, so run it all the time: start it at login
with `examples/board.plist` (macOS) or `examples/firetower-board.service`
(Linux). Without one, `firetower board --open` (or `--start`, which only
starts it) runs it in the background the first time it's needed, with its
output added to `board.log` in the state folder. `firetower board --stop`
stops your board, however it was started; one a service runs may be started
again by the service, so stop that instead.

**The window.** `firetower board --open` opens the board as an app window
(or `$FIRETOWER_OPEN_WITH`, a command of your own that opens the address):
districts as boxes, towers inside, a row per lookout with its dot, message
and times. **By tag** groups them by tag instead, whatever their district or
machine, and the tag list shows only one tag's; **Needs a look** is only what
needs a look, worst first, the short list for a small window. The address
keeps the choice (`?view=tag`, `?view=attention`, `?tag=health`), so a
bookmark or a bar opens straight to it. Click a lookout's name to open its
link (a URL, or a local log shown as plain text); on a phone, tap a row for
its buttons (the ⋯). Seen, Stand down and Remove are buttons; Remove asks for
a second press, and a stood-down row offers Watch again. Messages show in
full. The tab title counts what needs a look, the same number the summary
starts with.
It follows your light or dark setting and works at phone width.

**The bar.** `firetower bar` prints one line for whatever shows one on a
timer. With the board up it reads the board, so a refresh every few seconds
costs one request on this machine; with none it dials every machine itself,
each time, and says so (`"board": false` in `--format json`):

- [SwiftBar](https://swiftbar.app) or xbar on macOS: `examples/firetower.30s.sh`
  puts binoculars in the menu bar, coloured by the worst state, with a menu
  of every lookout it shows.
- Waybar: `examples/waybar.jsonc` and `waybar.css`.
- Omarchy 4, whose bar is its own Quickshell shell: `examples/omarchy/firetower`
  is a bar plugin. Binoculars, with the count when something needs a look,
  yellow on smoke and red on fire, and nothing while there are no lookouts;
  a click opens a panel with the summary, every lookout shown that isn't clear
  (smoke or fire can be marked seen there, on any machine the board can
  reach), sources out of contact, and the board, which it focuses if it's
  already open. Copy the folder into Omarchy's plugins and turn it on
  (copy it again after an upgrade):
  `cp -R examples/omarchy/firetower ~/.config/omarchy/plugins/`, then
  `omarchy plugin enable firetower`. Fire is the theme's
  red and smoke its yellow; a theme can choose its own in a `firetower.toml`
  (`fire`, `smoke`, `no_word`, as `"#rrggbb"`), and for a built-in theme that
  file goes in `~/.config/omarchy/themes/<theme>/`.
- tmux, polybar, anything: `firetower bar` prints `1 fire · 2 smoke · 9 clear`
  (or `all clear (12)`, or `no lookouts`).

**Your own.** `firetower board --ui DIR` serves your page in place of the
built-in one, with the same API. `firetower bar --template '{{.Worst}} {{.Count}}'`
shapes the line (the data is what `bar --format json` prints). Or read
`firetower watch --json`, or the files in `~/.local/state/firetower`, and
draw what you like.

### The board's API

| | |
|---|---|
| `GET /api/state` | every lookout the board shows (less any `hide`), as `ls --json` prints them, and the sources |
| `GET /api/feed` | the same, as server-sent events, on every change |
| `POST /api/seen`, `/api/done`, `/api/rm`, `/api/expect` | `{"id": "district/tower/name"}` (or `source:district/tower/name`, which the board sends to that machine), as JSON |
| `GET /api/link?id=…` | a local lookout's linked file, as plain text (the last 256 KB) |

The board answers only to this machine, and only to whoever holds its key, a
secret in the private state folder. Loopback answers anyone on the machine,
so the key is what keeps a shared machine's other users out.
`firetower board --open` hands the window a one-time address (good once,
for a minute), which the page trades for the key: the key never sits on a
browser's command line, which everyone on the machine can read. Someone who
reads the address there could still beat the window to it; when the window
then arrives with a spent address, the board changes its key, so what they
got stops working, and the page says to open it again.
`firetower board --url` prints the address with the key itself, for a
script; `--url --once` prints a one-time one. The page keeps the key in its
own origin's storage and sends it with every request, as
`Authorization: Bearer <key>` (or `?key=` on the feed and log links, which a
browser opens without headers). A script does the same. There are no
cookies: a browser would hand them to every other server on 127.0.0.1. Other
sites, other local servers and framed copies of the page are all turned
away.

A page of your own (`--ui`) gets the key the same way: the address it's
opened with carries `?key=`, or `?once=` to trade at `POST /api/claim`
(`{"once": "…"}` gives `{"key": "…"}`, once).

The bar and the CLI ask the board on `board.port` to prove it holds the key
(`GET /api/proof?nonce=…`, which only the key can answer) before they send
it, so a stranger who took the port while the board was down never sees the
key, and isn't believed. A board started by hand on another `--port` isn't
found by them; set `board.port` instead.

A running board reads the config when it starts: after changing `hide`,
`keep` or a source, restart it (`firetower board --stop`, then `--open`, or
restart the service), or the bar and the board keep the old one.

### Other machines

A board can show other machines' lookouts too. List them in
`~/.config/firetower/config.toml` (see `examples/config.toml`):

```toml
[[source]]
name = "work-mac"
command = ["ssh", "-o", "BatchMode=yes", "work-mac", ".local/bin/firetower", "watch", "--json"]
district = "work"   # optional: fold all of it into one district
act = ["ssh", "-o", "BatchMode=yes", "work-mac", ".local/bin/firetower"]   # optional: lets the buttons reach it
```

A shared machine that reports for everyone on it can be trimmed to what's
yours. A lookout shows unless its tower matches `hide`, and `keep` wins over
`hide` (so `keep` alone does nothing). Patterns are shell globs (`*`, `?`,
`[abc]`, `[!abc]`) against `district/tower`, or the tower alone with no `/`,
on that machine's own names (one that isn't a name shows, and matches, as
`unnamed`), before any `district` folding:

```toml
[[source]]
name = "shared-box"
command = ["ssh", "-o", "BatchMode=yes", "shared-box", ".local/bin/firetower", "watch", "--json"]
hide = ["inbox/*"]       # everyone's inbox towers...
keep = ["inbox/alice"]   # ...except this one
```

A hidden lookout is left out of everything on this board: the list, the bar,
the counts, the colours, the API and the buttons. The same two settings under
`[board]` trim this machine's own. A machine out of contact still shows as
out of contact, even if every tower of its is hidden: silence never looks
clear. It's what your board shows, not who may
read what: `firetower ls` and `watch` still show everything. A pattern that
could never match a name (a capital, a space, an empty side of the `/`) is an
error like any other in the config: the board and the bar refuse to start
until it's fixed (a board already running keeps the config it started
with, and `seen`, `done` and `rm` on another machine's lookout still go
through it; `--stop` still stops it). Reports still land in your `default_district` and
`default_tower`, with a warning in their output, whatever the slip (a bad
value, a wrong type, an unknown key). Only a file that isn't TOML at all, or
a bad default itself, sends them to `main` instead; and `rm --tower` without
`--district` then refuses rather than guess the district.

Another machine's lookout is `source:district/tower/name`, as the boards show
it. `firetower seen`, `done` and `rm` take that too: through the running
board, or with none, straight through the source's `act` command, and a
tower the source hides can't be reached either way. A message never goes
along (`act` usually crosses a shell on the other machine), so `done` there
keeps what the watch last said. Through the board, a lookout must be one
the board has heard of; straight through `act`, the other machine says if
there's no such lookout.

A source that fails, or goes quiet for two minutes, is **out of contact**:
its lookouts stay, marked, and the board keeps trying. It never looks clear.

## Checking in

You need macOS or Linux.

```
curl -fsSL https://raw.githubusercontent.com/tj-techneosis/the-firetower/main/install.sh | bash
```

That fetches the latest release for your machine, checks it against the
release's SHA-256 sums and puts `firetower` in `~/.local/bin`. With Claude
Code installed, it links the `firetower` skill and offers a short CLAUDE.md
section, so agents put watches on the Firetower rather than keeping a session
open to poll. To build from source, clone the repo and run its `install.sh`
(Go 1.26 or newer, or mise).

`firetower upgrade` installs the latest release, the same way the one-liner
does; your lookouts and config carry over. `firetower upgrade --check` only
asks: it exits 0 when this is the latest, 3 when a newer release is out and
1 when it can't tell, so a weekly check can ring you:

```
crontinental every weekly --name firetower-upgrade --forever -- firetower upgrade --check
```

A build from a checkout upgrades there: `git pull`, then `./install.sh`.
`install.sh --uninstall` removes it and leaves your lookouts and config.

There is no daemon. The files are the state: one JSON file per lookout in
`~/.local/state/firetower/<district>/<tower>/<name>.json` (or
`$FIRETOWER_DIR`), written atomically under a lock, and read by anything that
wants to.
