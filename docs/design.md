# The Firetower: the design

Why each piece is the way it is. The README is how to use it.

A lookout for background watches. Anything on a timer (a cron job, a
Crontinental job, a launchd agent, a CI job, a Claude session asked to "keep
an eye on this") reports one line to the Firetower, and a small board shows
every watch at a glance: which are quiet, which saw smoke, which went silent.
Nothing pops up, and no Claude session stays open just to poll.

The Firetower is only the board and the way to report to it. It runs no
checks, schedules nothing, keeps no clock and knows nothing about what is
being watched. The scheduler runs the check and owns all the timing (how
often, how long a run may take, when the watch ends); the check says what it
saw; the Firetower is where that lands and is shown. One clock, the
scheduler's: a report carries two of its times (when the next report is
due, when the watch ends), and readers only compare them with now. The
Firetower never times, schedules or kills anything.

## Why

A lot of watches are the same small job: "check every 30 minutes whether the
fix landed", "make sure tonight's deploy goes out", "is the site up and its
keepalive still running", "tell me when this order goes through". Most answers
are a status, or just movement vs no movement. Today each one lives in its own
session, log or daily report, so there's no one place to see that twelve
watches are running and all is quiet. Desktop notifications are not wanted.

## The frame

A fire lookout service, in three layers (like a terminal's spaces and tabs):

- A **district** is the widest group: a product, a client, a machine,
  whatever someone wants as one box. Most people have one and never name it.
- A **tower** is a group of watches inside a district: one part of the
  product, one site, one service.
- A **lookout** is one watch.

For example, a team with an app and a website might keep two districts, `app`
(towers `signup`, `checkout`) and `website` (towers `admin`, `questionnaire`,
`checkout`). Nothing in the tool knows what a district stands for: districts
and towers exist because lookouts name them, and go when their last lookout
does.

| file status | board says | means |
|---|---|---|
| `green` | clear | the check ran and all is fine |
| `yellow` | smoke | worth a look |
| `red` | fire | something's wrong |
| `done` | stood down | the watch finished; greys out, then leaves |
| (computed) | no word | past its `next_by`: the report it promised didn't come |
| (computed) | waiting | `expect`ed, no first report yet |
| (computed) | moved / still | what it watches changed / hasn't, and since when |
| (computed) | seen | someone acknowledged this smoke; dim until it changes |

The frame is in the boards only. The file format and the CLI use plain words,
so scripts stay readable.

## Lessons this is built on

From years of heartbeat checks that lied:

1. **A fresh report is not a healthy one.** A job that reports on failure too
   is always fresh. So status and freshness are separate, and the Firetower
   keeps `last_clear_at` itself: "fire since Tue, last clear 3 days ago".
2. **Silence needs a promise to mean anything.** A report that says when the
   next is due (`next_by`) is held to it: past that time with nothing new,
   the board says "no word". The time comes from the scheduler, which knows
   it; the Firetower only compares it with now. A report with no promise (a
   manual run) is a record of a moment, never judged.
3. **Never reported is not healthy.** `firetower expect` puts a lookout on the
   board before its first report, so a check that never starts shows
   "waiting", then "no word yet" once the `--next` it was given has passed.
4. **Acknowledged is not fixed.** "Seen" dims a smoke until its status or
   value changes, so a known problem stops shouting without being hidden, and
   a new one gets through. Not the message: messages often carry a time
   ("still down at 4:57 pm"), so a seen would never last a single check. A
   check that wants a new kind of trouble to break through puts it in `value`.

## Pieces

### 1. The report (the contract)

Each lookout is one file, `<dir>/<district>/<tower>/<name>.json`, where `<dir>` is
`~/.local/state/firetower` (or `$FIRETOWER_DIR`):

```json
{
  "name": "deploy-landed",
  "district": "website",
  "tower": "checkout",
  "label": "Deploy lands",
  "status": "green",
  "message": "Live at 4:57 pm.",
  "value": "a1b2c3d",
  "moved_at": "2026-10-31T22:50:00Z",
  "checked_at": "2026-10-31T22:57:09Z",
  "last_clear_at": "2026-10-31T22:57:09Z",
  "status_since": "2026-10-31T22:57:09Z",
  "next_by": "2026-10-31T23:27:09Z",
  "until": "2026-11-01T06:00:00Z",
  "seen": null,
  "link": "https://example.com/deploys/123",
  "source": "crontinental:deploy-landed",
  "tags": ["deploys", "health"]
}
```

- `district` and `tower` are free text. Without them, a lookout goes to the
  defaults (`$FIRETOWER_DISTRICT` / `$FIRETOWER_TOWER`, or `default_district` /
  `default_tower` in `~/.config/firetower/config.toml`, else `main`). A board
  with one district doesn't show the district at all.
- `status` and `message` are what the check saw. One short line: the boards
  show it on screen, so never a secret, a person's name or anything private.
- `value` is optional: anything that identifies the state being watched (a
  commit, an order status, a row count). The Firetower keeps `moved_at`: it
  changes only when `value` changes. That is "movement vs no movement" without
  the check remembering anything.
- `next_by` is when the next report is due, set with `--next` (a span from
  this report, like `30m`, or a time). Past it, readers show "no word". A
  report that leaves `--next` out keeps the promise it had: the deadline is
  the schedule's, and a report by hand in between doesn't move it. `--next
  none` clears it.
- `until` is when the watch ends, set with `--until` (a span or a time). A
  span is anchored once: by `expect`, or by the first report that gives one,
  since a booked `run X --until 6h` repeats the flag on every run and the
  watch must still end six hours after it began. A time always applies;
  `expect` re-anchors a span (booking the watch again). Past it, readers show
  the lookout as stood down, exactly as if it had written `done`; a report
  after that, without a new `expect`, doesn't revive it. A clock time
  (`18:00`, `tomorrow 07:00`) is worked out from now, so it anchors like a
  span.
- Stood down sticks, however it happened (`done`, the board's button,
  `until`). A job still booked that reports again only leaves a sign
  of life (so the lookout isn't tidied away and started over) and is told
  why on stderr. `expect` books the watch again: it starts over, waiting.
- `run`'s own mistakes (a bad flag, a bad name) exit 64, the usage code: 1
  and 2 mean smoke and fire there, so a typo'd booking mustn't look like a
  failing check in the scheduler's history.
- `done` with no message keeps what the watch last said. A seen shows when
  it was seen. A machine out of contact counts once, on the bar and the
  board, however many lookouts it had; one that flaps (answers, drops)
  keeps the time it first went.
- Spans are capped at 100 years, so none can overflow into the past. What a
  scheduler hands over (`CRONTINENTAL_NEXT`, `_UNTIL`) is taken only as
  RFC 3339 or `none`; a NEXT already past counts from now.
- `firetower run` takes `--next` and `--until` from `$CRONTINENTAL_NEXT` and
  `$CRONTINENTAL_UNTIL` (Crontinental 0.8 and later sets both), so the timing
  is said once, to the scheduler. NEXT is when the next run *starts*; its
  report lands after it finishes, so `next_by` is NEXT plus five minutes. A
  check that takes longer passes `--next` itself (a flag always wins). Run by
  Crontinental with no NEXT (a one-off, a repeat's last run) means nothing
  more is promised, so `next_by` is cleared. A value it can't read is
  skipped with a warning, never a lost report.
- Stood-down lookouts stay on the boards, grey, for a day, then readers stop
  showing them. `gc` deletes their files once they have also gone a day
  without a report (a job nobody unbooked must not have its lookout deleted
  and started over as a new watch), and so does any write, at most once an
  hour, so nobody has to book `gc`.
- `link` is what a click opens (a URL, a file, or a log). `source` is free text
  shown on hover: who reports.
- `tags` are words of the person's own (`--tag`, names like every other
  name, at most 16, kept sorted): a report that gives `--tag` carries exactly
  those, one that leaves it out keeps them, `--tag none` clears them. They're
  for grouping across districts, towers and machines (the board's By tag
  view, `ls`/`bar --tag`) and for a person's own workflow to act on (`ls`,
  `rm` and `gc --tag`). The Firetower does nothing with a tag by itself:
  "tear down what's tagged temporary once it's over" is a scheduled `gc
  --tag temporary` the person books, never a rule inside the tool. The feed
  carries them, so another machine's lookouts group the same way.
- The Firetower owns `moved_at`, `last_clear_at`, `status_since` and `seen`
  (when reports landed, never timers);
  checks never write them. A `set` that leaves a flag out keeps that field
  (label, link, source); status and message are always the new report's.
- Label and message are one line of plain text: newlines become spaces,
  control characters (terminal escapes) are dropped, and they are cut at 200
  characters, so nothing a check prints can garble a terminal or a board.
- Writes are atomic (temp file + rename). Fields the CLI doesn't know are
  kept, so the format can grow.

### 2. `firetower` (one CLI, the only writer)

```
firetower set NAME STATUS "MESSAGE" [--district D] [--tower T] [--label L] [--value V]
              [--next 30m|TIME] [--until 6h|TIME] [--link X] [--source S] [--tag T]...
firetower run NAME [same flags] -- CMD ...
firetower expect NAME [--next 30m] [--until 6h] [--district D] [--tower T] [--label L] [--tag T]...
firetower seen NAME             # acknowledge the current smoke or fire
firetower done NAME ["MESSAGE"]
firetower rm NAME | --district D [--tower T] | --tag T...   # a lookout, tower, district or tag
firetower ls [--district D] [--tower T] [--tag T]... [--json]
firetower watch --json          # the board's feed: a line on every change
firetower gc [--tag T]...       # clears stood-down lookouts older than a day (tagged: now)
firetower bar [--format plain|json|waybar|swiftbar] [--template T] [--tag T]...
firetower board [--port N] [--ui DIR] | board --open | --start | --stop | --url
firetower upgrade | version
```

- `run` is for checks that don't know about the Firetower: it runs CMD and
  reports from its exit code (0 clear, 1 smoke, 2 fire; anything else is fire
  too: no code stands a watch down, since stood down sticks and tools exit 3
  for their own reasons, like curl on a bad URL) with the last line of output as the message, and a `VALUE:`
  line as `value`. A check that can't run at all is fire, never silence. If
  `run` is stopped (a scheduler's timeout, a kill), it reports fire, "stopped",
  before it goes. It keeps no
  time of its own: how long a check may run, and when it runs, are the
  scheduler's business. The Firetower is where reports land and are shown.
- A NAME is unique within its tower; `district/tower/name` (or `tower/name`
  in the default district) works anywhere a NAME does.
- `watch --json` prints every lookout first, then a line per change, and
  re-sends every 60s so a board knows the feed is alive. A lookout passing
  its `next_by` or `until` is a change too, sent without anyone writing. It is what boards read,
  locally or over ssh.
- Two reports to the same lookout at once can't lose an owned field: each
  write holds the state dir's lock while it reads and replaces the file
  (writes are rare, so one lock for the whole dir costs nothing).
- Names are lowercase file names (`[a-z0-9][a-z0-9._-]*`): a case-insensitive
  file system would make `Deploy` and `deploy` one file, and nothing can climb
  out of the state dir. A message that starts with dashes goes after `--`.
- One static binary (Go, like its siblings), with `install.sh`, release
  archives and `SHA256SUMS`. The files are the state: no daemon is needed to
  report or to read. The board, when it runs, is a hub for reading (below),
  never a second keeper of state.

### 3. Ways in

The Firetower depends on no scheduler and no other tool. Anything reaches it
one of three ways, and all three end in the same file:

1. **The check reports.** A script ends in `firetower set`. Works under any
   scheduler, or none (a person running it by hand).
2. **The scheduler wraps it.** The booked command is `firetower run NAME
   --next 30m -- ./check.sh`; the check itself knows nothing. With
   Crontinental: `crontinental every 10m --until "5 hours" --name
   deploy-landed --notify never -- firetower run deploy-landed -- ./check.sh`:
   Crontinental hands the run its next due time and its end, so the timing
   is said once. With cron or launchd, the same line plus `--next` (and
   `--until`), which they don't hand over.
3. **A bridge reports for a whole system.** Anything that keeps its own record
   of jobs (a scheduler's history, a job registry, CI runs, a database table
   of heartbeats) gets a small job that reads that record and runs one
   `firetower set` per job, under its own tower. The bridge lives with that
   system, not in the Firetower.

A scheduler with its own notification hooks can also call `firetower set`
from one, set up by its user, in neither tool. Crontinental, for example, runs
every script in `~/.config/crontinental/notify.d/` with the message and the
job's tags (`CRONTINENTAL_TAG_<KEY>`), so a hook reading a tower tag could put
each outcome on the board. But a hook only hears what the job's `--notify`
lets through (failures, by default), so it can't say "clear" or "still".
Route 2 stays the default; a hook is for jobs that weren't booked with it.
Nothing here waits for a scheduler feature.

### 4. Boards (they only read)

Everything a board needs is in the files and the feed (`watch --json`), so
anyone can build their own. Nobody has to: the binary carries two boards of
its own, in plain Go with no GUI toolkit, and each can be swapped out without
giving up the rest.

- **`firetower board`**: a small web board, served on `127.0.0.1` from the
  binary itself. `--open` opens it as an app window (a browser window without
  tabs or a toolbar) where the browser can, else a normal tab, or with
  `$FIRETOWER_OPEN_WITH` (a desktop's own launcher). Districts as
  boxes, towers inside, lookouts as rows: a dot, the label, the message (one
  line, truncated) and "checked 4:57 pm · still since 2:10 pm". A tower shows
  its worst status. Click a row to open its `link`; "seen", "stand down" and
  "remove" do what the CLI does. Nothing ever pops up. Two more layouts, from
  the same state: by tag (a lookout under each tag it carries), and "needs a
  look" (only fire, smoke, no word and out of contact, worst first: the
  short list for a small window where there's no bar plugin). The address
  carries the choice (`?view=tag|attention`, `?tag=T`).
  *Your own page instead:* `--ui DIR` (or `ui` in config) serves your files in
  place of the built-in page, with the same small API (below).
- **`firetower bar`**: one line for any tray or toolbar, printed and done, for
  whatever runs it on a timer: SwiftBar or xbar for a macOS menu-bar icon,
  Waybar, Omarchy's own bar (a plugin in `examples/omarchy`), polybar, a tmux
  status line. Binoculars (as on the board), the
  count, coloured by the worst status, and a dropdown or tooltip per district.
  `--format` picks the shape; *your own shape instead:* `--template` takes a
  Go template over the same summary `--format json` prints.
- **Your own board entirely:** read the files, or run `firetower watch
  --json` (locally or over ssh) and draw what you like.

**The board's API**, for a custom page: `GET /api/state` (every lookout the
board shows, as `ls --json` prints them), `GET /api/feed` (the same, as server-sent events),
`POST /api/seen|done|rm|expect` with `{"id": "district/tower/name"}` (or
`source:district/tower/name`, which the board routes through that source's
`act`), and `GET /api/link?id=…` (a lookout's `link` when it is a local
file: shown read-only, never run).

**The board is the machine's hub.** It already follows every source, holds
the key and routes actions, so everything else on the machine asks it before
doing that work itself. `firetower bar` reads `/api/state` over loopback with
the key (two seconds, then it gives up on it) and only dials the sources
itself when no board of this person's answers: one that isn't up, one that
hangs, or another person's on the port, whose key this isn't. "This
person's" is proved before the key is sent: the client sends a fresh nonce
to `GET /api/proof` and checks the HMAC the board answers with, so a
stranger holding the port neither sees the key nor is believed. Once a board
has proved itself, a failure is reported, never retried another way: a slow
`act` must not run twice on the other machine. `firetower
seen|done|rm source:…` posts to the board, or with none runs the source's
`act` itself, held to the same `hide`. That is what keeps a bar refreshing
every few seconds from being an ssh login per source per refresh. `board
--open` and `--start` never tie a board to whoever asked: the one up is
used, or one is started in the background (its own session, its output
added to the state dir's `board.log`), and `--stop` asks the proven board
for its own pid, so it never signals anything on a stale note's word. The
window `--open` makes gets a one-time address (`?once=`, good once for a
minute, traded at `POST /api/claim`): a browser's command line is readable
by everyone on the machine, and the key would be good forever. A poller on
`/proc` could still claim the address first; the real window then arrives
with a spent token, and the board answers by making a new key and ending
every open feed, so the stolen key is worthless within seconds. The proof
signs the port as well as the nonce (a stranger can't relay the question to
the person's board on another port), and the key goes over the one
connection the proof came back on. A running board reads its config once,
at start.

The waits, and why: a board on loopback answers in milliseconds, so it gets
half a second to prove itself (one that can't is busy or isn't ours, and
the bar dials the sources instead, so a stranger that hangs costs a refresh
half a second), and two seconds for `/api/state`. A background start gets
five seconds to answer before the command says so (the board may still be
starting; its output is in `board.log`), and `--stop` five seconds for the
board to go. None of these times anything the person booked: they're how
long one command waits on another, and nothing keeps running after. Plain
`board` stays in the foreground for a service, which is the recommended way
to run it (`examples/`).

**Who can reach it.** Loopback answers anyone on the machine, so a port alone
would show a shared machine's other users this person's board and the files
its lookouts link to. The board has a key, a secret in the private state dir
(`.board-key`, 0600). `board --open` and `board --url` carry it in the
address once; the page keeps it in its own origin's localStorage and takes it
out of the address bar, and sends it as `Authorization: Bearer` (or `?key=`
on the two GETs a browser opens without headers: the event stream and a log
in a new tab). Scripts send the same header. Everything under `/api` needs
it; the pages themselves are only code.

No cookie, on purpose: a browser sends 127.0.0.1's cookies to every port on
it, so any other local server (a dev server running someone's npm code) would
receive one. With no ambient credential there is nothing for another page to
borrow either, so cross-site request forgery has nothing to work with. On top
of that: the server answers only to `127.0.0.1`/`localhost` on its port as
the Host (DNS rebinding gets nothing), a write carrying another page's Origin
is refused, and no page can be framed. All of it is tested, including from a
hostile page in a real browser.

**Other machines come in through config.** Most people have one machine and
need none. Someone with more lists them in `~/.config/firetower/config.toml`
(or `$FIRETOWER_CONFIG`), which also holds the defaults:

```toml
default_district = "main"
default_tower = "main"

[board]
port = 7480
# ui = "~/my-firetower-page"

[[source]]
name = "work-mac"
command = ["ssh", "work-mac", "firetower", "watch", "--json"]
district = "work"        # optional: put everything from it in one district
act = ["ssh", "work-mac", "firetower"]   # optional: lets seen/done/rm reach it
hide = ["inbox/*"]       # optional: towers this board leaves out
keep = ["inbox/alice"]   # optional: back in, even if hidden
```

A source is any command that prints `watch --json`. Without `district`, its
districts show as they are; with it, its lookouts all go in that district,
each keeping its own district in `home_district` (a "district …" chip on
the board), so nothing is lost. A source
whose command fails, or whose feed goes quiet past 2 minutes (the feed
re-sends every 60s), shows "out of contact" on its lookouts instead of
looking clear, and the board reconnects with a backoff. Both boards read the
same sources. (One district per machine is then just a config choice, not
something the tool assumes.)

`hide` and `keep` choose which of a source's towers this board shows, for a
shared machine that reports for everyone on it: each board keeps its own,
and the machine never needs to know who is looking. A lookout shows unless
its tower matches `hide`, and `keep` wins over `hide` (so `keep` alone does
nothing). Patterns are shell globs (`[!x]` negates, as in the shell) against
`district/tower`, or the tower alone when there's no `/`, matched on the
names the board would show (a feed's name that isn't one is `unnamed`),
before any folding. A hidden lookout is dropped as the feed comes in, so it
reaches nothing: not the board, the bar, the counts, a district's colour,
the API or the buttons. The one exception is the machine itself: a source
out of contact still shows, and counts, as out of contact even with every
tower of its hidden, since silence must never look clear. The same two settings under `[board]` apply to this
machine's own lookouts, and the buttons refuse those too. It's what a board
shows, not who may read what: anyone on that machine can still `firetower
ls` there. A pattern that could never match a name (a capital, a space, an
empty side of the `/`) or can't be read is a config error, like a bad port:
the boards refuse to start, rather than quietly showing everything. A board
already running keeps the config it started with, so `seen|done|rm source:…`
still go through it, and `--stop` (which needs only the port) still stops it.

A broken config never stops a report, and a slip in a board's settings
never moves one. A file with a bad setting (a port, a pattern, an unknown
key, a value of the wrong type) keeps its `default_district` and
`default_tower` for reports: when the whole file won't decode, the two are
read on their own. Only a file that isn't TOML, or a bad default itself,
leaves `main`, each default on its own. Every command that reads the config
for a lookout says so on stderr each time, not only at a terminal, since a
scheduler's log is where it's found. `rm --tower` takes its district the way
every command does (`--district`, `$FIRETOWER_DISTRICT`, the default), and
refuses rather than guess when only a lost default would name it.

### 5. For agents

`agents/skills/firetower/SKILL.md` tells an agent how to put a watch on the
Firetower: pick a district (if the project has one), a tower and a name, write the check as a script that ends in
`firetower set` (or wrap it in `firetower run`), `expect` it first, book it
with whatever scheduler the machine uses (which owns the timing), and pass
the board the next report's due time with `--next`.
The skill requires no scheduler; it shows cron, launchd and Crontinental side
by side, and the four lessons above as rules for writing a check. When a watch
needs judgment rather than a query, its job runs a one-shot agent
(`claude -p "…"`) and maps the answer to a status. That's still a timer job,
not a session kept open. Also `agents/claude-md-snippet.md`, like the
Crontinental's.

## How it's checked

`tests/` is black-box: it drives the real binary through its CLI against a
throwaway state dir and config, with a pinned clock, and is the
specification. The board is also tested by the Administration
(`.administration/`): browser users at desktop and phone widths, keyboard
only, and critics who read the code and the docs as a newcomer would, each
finding checked against the code before it counts.

## With Crontinental

Crontinental 0.8 and later hands each run `CRONTINENTAL_NEXT` (the next due
time, unset when the job won't come round again) and `CRONTINENTAL_UNTIL`
(unset without a deadline), ISO-8601 UTC, so a booking states its timing
once and `firetower run` passes it on.

## Rules for the repo

- Nothing about any one project, client, person or machine goes in the code,
  the boards, the skill or the docs: no client names, host names or private
  paths. Examples use `example.com`, `example-shop`, `work-mac`.
- The tool ships no checks. Checks live beside the project they watch.
- Status files are runtime state, never in git.

## Out of scope

A native GUI app or toolkit (the boards are a page and a line of text),
history graphs, phone or webhook delivery, editing checks from a board, a
server that collects other people's sources (each board reaches the ones it lists itself),
reports pushed in over the network (a check on the machine reads them and
reports).
