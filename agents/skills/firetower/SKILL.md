---
name: firetower
description: Put a background watch on the Firetower (`firetower`) — a check that runs on a timer and reports one line (clear, smoke, fire) to a quiet board, instead of a Claude session kept open to poll. Use when asked to "keep an eye on", "watch", "monitor", "check every N minutes whether", "let me know if X breaks", "make sure tonight's deploy goes out", "is the site still up", "tell me when this order goes through", or to see what's being watched ("what's on the board", "is anything on fire", "firetower ls"). The Firetower only shows watches; something else runs them (Crontinental, cron, launchd, systemd, CI). The scheduler owns all timing; the Firetower keeps no clock, and only compares the due time a report gives (`--next`) with now to show "no word". Covers naming a watch, writing the check, `set` vs `run`, `expect`, `--next`/`--until`, tags (`--tag`, grouping, a temporary watch that's tidied away), booking it with a scheduler, and reading the board.
---

# Putting a watch on the Firetower

The **Firetower** (`firetower`, on PATH) is a board for background watches.
A check runs on a timer, reports one line, and the board shows every watch at
a glance: clear, smoke, fire, or no word. It runs no checks, schedules
nothing and keeps no clock: the scheduler runs the check and owns all the
timing (how often, how long, when it ends), the check says what it saw, and
the Firetower is where that lands. A report passes on two of the
scheduler's times, when the next report is due (`--next`) and when the watch
ends (`--until`); the board only compares them with now. Nothing pops up,
and no session stays open to poll.

Never keep this session open (or loop) to watch something. Book a check.

## The five steps

1. **Name it.** A lookout lives at `district/tower/name`: a district is the
   widest group (a product, a client), a tower one part of it (a site, a
   service), the name one watch. Lowercase letters, digits, `.`, `_` and
   `-`, starting with a letter or digit, at most 64 characters. If the
   project already has lookouts, match them (`firetower ls`); otherwise
   leave the district out (it defaults to `main`), name the tower after the
   thing watched and the lookout after the question: `shop/up`,
   `shop/deploy-landed`, `orders/stuck`.
2. **Write the check** so it says what it saw (rules below).
3. **Expect it**, so a check that never starts shows "waiting", then "no
   word yet", instead of nothing:
   `firetower expect shop/deploy-landed --next 30m --until 6h --label "Deploy lands"`
   (the same end the booking gets).
   What `expect` sets (label, link, until) stays: later reports that leave a
   flag out keep it. Its `--next` holds until the first report, which makes
   its own promise (under Crontinental, `run` does that for you). In a
   booked command, use the full `district/tower/name`.
4. **Book it** with whatever scheduler this machine has (below). The
   scheduler owns the timing. Under Crontinental, `firetower run` picks up
   when the next report is due and when the watch ends by itself; under
   anything else, pass `--next` (the interval plus a little slack) so the
   board can tell a dead job from a quiet one.
5. **Run it once now**, so the board shows a real report instead of waiting
   a whole interval: run the booked command by hand, or `crontinental run
   NAME`. Then `firetower ls`.

## Two ways to report

**The check reports itself**, ending in `firetower set`:

```sh
#!/bin/sh
# Has the fix landed? Movement vs no movement is the value.
sha=$(git ls-remote https://example.com/repo.git refs/heads/main | cut -c1-7)
if [ -z "$sha" ]; then
  firetower set web/checkout/fix-landed red "Can't reach the repo." --next 35m
elif [ "$sha" = "$WANT" ]; then
  firetower set web/checkout/fix-landed green "Landed: $sha." --value "$sha" --next 35m
else
  firetower set web/checkout/fix-landed yellow "Not yet; main is at $sha." --value "$sha" --next 35m
fi
```

**Or wrap a check that knows nothing about it** in `firetower run`. Its exit
code is the status (0 clear, 1 smoke, 2 fire, anything else fire too), its last line of output the message, a `VALUE: x` line the value:

```sh
firetower run web/site/up --next 6m -- curl -fsS -o /dev/null --max-time 20 https://example.com/
```

`run` passes the command's output and exit code through, reports fire when the
command can't start or is stopped, and takes `--message "..."` (a fixed
message, when the output could hold something private). If a check can hang,
give the scheduler a timeout: timing is its job, not the Firetower's. Under
Crontinental, `run` takes `--next` and `--until` from the job's
`$CRONTINENTAL_NEXT` (plus five minutes for the run itself) and
`$CRONTINENTAL_UNTIL`; a check that takes longer than five minutes passes its
own `--next`, which always wins. (A script that ends in `firetower set` doesn't
pick these up: wrap it in `run` and have it exit 0, 1 or 2, or pass
`--next` itself.)

## Rules for a check

1. **Fresh is not healthy.** Report failure as `red`/`yellow`, never as
   silence and never as `green` with a sad message. The board keeps "fire
   since Tue, last clear 3 days ago" itself.
2. **Promise the next report.** Under Crontinental, `run` does it for you.
   Otherwise give `--next` the scheduler's interval plus a little slack
   (every 30m → `--next 35m`). Once it passes with nothing new,
   the board shows "no word": that's how a dead job shows up. A report with
   no `--next` is never judged. Don't time anything else: how often, how long
   a run may take and when the watch ends are the scheduler's.
3. **A check that can't check is fire.** Network down, API 500, can't parse:
   `red` with what went wrong, not an exit that reports nothing.
4. **Put the state in `--value`.** A commit, an order status, a count: the
   board shows "still since 2:10 pm" or "moved", with no memory in the check.
   Someone marking a smoke "seen" dims it until the status or the value
   changes, so a *new* kind of trouble goes in the value to break through.
   Keep times out of the value.
5. **Messages are on screen.** One short line, never a secret, a token, a
   person's name or anything private. Use labels and ids, not names.
6. **Give it an end.** Tell the board when the watch ends with `--until`, the
   same end the scheduler was given: a span (`6h`, counted from when the
   watch was booked, not from each run), a clock time (`23:59`), `tomorrow
   07:00`, a date, or a full time. Or report `done` when the event is seen
   (`firetower done NAME "Landed."`, which stays down until `expect`) and let the
   booking end: with Crontinental's `--until-done`, the check script runs
   `firetower done NAME` and then exits 99 (under `firetower run`, exit 99
   would read as fire). To watch the same thing again later, `expect` it
   again with its `--next` and `--until`: that starts the end afresh. A
   stood-down lookout greys out and leaves the board after a day.

## Booking it

Use whichever the machine has; the Firetower doesn't care.

```sh
# Crontinental (if installed: see its skill). --notify never: the board is the
# notification. Its own grammar for an end: "5 hours", "23:59".
crontinental every 30m --name fix-landed --until "6 hours" --notify never -- ~/checks/fix-landed.sh
# `run` takes its next due time and end from Crontinental: say the timing once.
crontinental every 10m --name shop-up --until "5 hours" --notify never -- \
  firetower run shop/up -- curl -fsS -o /dev/null --max-time 20 https://example.com/
crontinental every 5m --name site-up --forever --notify never -- \
  firetower run web/site/up -- curl -fsS -o /dev/null --max-time 20 https://example.com/

# cron (crontab -e): it hands over no times, so say --next
*/5 * * * * $HOME/.local/bin/firetower run web/site/up --next 6m -- curl -fsS -o /dev/null https://example.com/

# launchd (macOS): a LaunchAgent with StartInterval 300 and ProgramArguments
#   [~/.local/bin/firetower, run, web/site/up, --next, 6m, --, curl, ...]
# systemd (Linux): a user .timer (OnUnitActiveSec=5m) and a .service whose
#   ExecStart is the same command line
```

Use the full path (`~/.local/bin/firetower`) where the scheduler's PATH may be
bare (cron, launchd, systemd). Crontinental runs jobs in a login shell, so
`firetower` is enough there.

## A watch that needs judgment

When the answer isn't a query (read a page and say whether it looks right),
the job runs a one-shot agent and maps its answer to a status. It's still a
timer job, not a session kept open:

```sh
answer=$(claude -p "Read https://example.com/status. Reply with exactly one word: green, yellow or red." 2>/dev/null)
case $answer in green|yellow|red) ;; *) answer=red ;; esac
firetower set web/status/page "$answer" "Status page read by an agent." --next 70m
```

## Tags

A watch can carry tags, the person's own words: `--tag health` on `set`,
`run` or `expect` (repeat it or join with commas; `--tag none` clears). Use
them when the person groups watches by kind rather than by place ("every
machine's health check"), or when a watch is temporary. Use the words the
person uses, and reuse tags that exist (`firetower ls` shows them).

The Firetower does nothing with a tag by itself. For a temporary watch that
should go once it's over, tag it and book the tidy-up too, if it isn't
booked already (`crontinental list`):

```sh
firetower run deploy/web/rollout --tag temporary --until 2h -- ./check-rollout.sh   # the booked line
crontinental every 1h --name tidy-temporary --forever --notify never -- firetower gc --tag temporary
```

## Reading the board

```
firetower ls                  every lookout: state, id, when it checked, message
firetower ls --json           the same with every field (label, next_by, until, link, tags…), for a script
firetower ls --tag health     only what carries a tag
firetower bar                 one line: "1 fire · 2 smoke · 9 clear"
firetower board --open        the board, in a window
firetower seen NAME           acknowledge a smoke or fire (dims until it changes)
firetower done NAME ["MSG"]   stand a watch down (keeps its last message if none given)
firetower rm NAME             take it off the board (a job still booked puts it back:
                              to end a watch, stand it down)
```

Another machine's lookout is `source:district/tower/name`, as the board shows
it; `seen`, `done` and `rm` take that too (no message goes with `done`).

States in `ls` and JSON use plain words: `green` (clear), `yellow` (smoke),
`red` (fire), `stale` (no word: past its `next_by`), `waiting` (expected,
not reported yet), `done` (stood down). When asked "is anything wrong", read
`firetower ls`, and say what's fire, smoke or no word, with its age. No word
means the job isn't running: check the scheduler (`crontinental list`).
