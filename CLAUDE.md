# Working on the Firetower

This file is for working **on** the Firetower. The instructions for **using**
it (what an agent does when asked to watch something) are the skill in
`agents/skills/firetower/SKILL.md`, which `install.sh` links into a user's
Claude Code setup.

## Layout

| path | what |
|---|---|
| `cmd/firetower/` | the whole tool, in Go: one static binary (`CGO_ENABLED=0`) |
| `cmd/firetower/lookout.go` | the report format, names, cleaning text, spans |
| `cmd/firetower/store.go` | the files: paths, the lock, atomic writes |
| `cmd/firetower/view.go` | what readers work out: no word (past next_by), stood down (past until), waiting, seen, moved |
| `cmd/firetower/commands.go` | set, expect, seen, done, rm, ls, gc |
| `cmd/firetower/run.go` | `run`: a wrapped check, its exit code, its signals |
| `cmd/firetower/watch.go` | the feed (`watch --json`) |
| `cmd/firetower/sources.go` | the post both boards read: this machine and the sources |
| `cmd/firetower/hub.go` | asking a running board first: `bar`'s state, `seen`/`done`/`rm` on another machine's lookout |
| `cmd/firetower/bar.go`, `board.go`, `ui/` | the boards |
| `agents/` | what ships to users' Claude Code: the skill and the CLAUDE.md snippet |
| `examples/` | SwiftBar, Waybar, the Omarchy bar plugin (`omarchy/firetower`), board-at-login, a config |
| `install.sh` | three modes: piped fetches a release, a checkout builds, an unpacked release installs its binary |
| `scripts/package.sh` | builds and packages every release target, plus `SHA256SUMS` and `VERSION` (what `upgrade --check` reads) |
| `tests/` | `python3 -m unittest discover -s tests`: run before every commit (it builds the binary) |
| `docs/design.md` | the design: why each piece is the way it is |

## Rules

- **Run the tests.** They are black-box and drive the real CLI against a
  throwaway state dir and config (`FIRETOWER_DIR`, `FIRETOWER_CONFIG`) with a
  pinned clock (`FIRETOWER_NOW`), so they never touch the machine's own
  lookouts. `FIRETOWER_BIN` points them at any build. A behaviour change comes
  with a test that fails without it.
- **One static binary.** The standard library and BurntSushi/toml. A new
  dependency has to keep `CGO_ENABLED=0` builds working for every target. No
  GUI toolkit: the boards are a page and a line of text.
- **The report format and the feed are contracts.** Other people's boards
  read them. Fields are added, never renamed or repurposed; unknown fields in
  a file are kept. A breaking feed change bumps `feedVersion`.
- **The Firetower owns** `moved_at`, `last_clear_at`, `status_since`,
  `seen` and `history`. Checks never write them, and every write goes
  through `update` (the lock, then read-change-write). Tags are the
  person's: the Firetower keeps and shows them and acts on one only when
  told to (`rm`/`gc --tag`), never by a rule of its own.
- **No clock.** The scheduler owns every time: how often a check runs, how
  long it may, when the watch ends. The Firetower never times, schedules or
  kills anything. A report carries two of the scheduler's times, `next_by`
  and `until`, and readers only compare them with now. No `--every`, no
  `--timeout`.
- **Readers decide, writers don't have to.** No word, stood down, waiting,
  seen and moved are worked out in `view.go`, the same way for `ls`, the feed
  and both boards. Nothing runs in the background to change a file.
- **Anything a check or a feed wrote is hostile** until cleaned: names hold
  to `nameRE`, text goes through `cleanText`, the board uses `textContent`,
  the bar escapes for its format. The board's guard (host, the key and never a
  cookie, origin, no framing) is tested in `tests/test_boards.py`; keep it that way.
- **The README stays in the lookout frame**: districts, towers, lookouts;
  clear, smoke, fire, no word, waiting, stood down, seen. Every word maps to real
  behaviour; if the behaviour changes, so does the README.
- **This repo is public.** Nothing machine-, person- or client-specific in any
  file, commit message or PR: examples use `example.com`, `work-mac`.
- **Commit messages carry no AI attribution.**

## Cutting a release

1. `git tag vX.Y.Z && git push origin vX.Y.Z`. A tag with a `-` publishes as
   a prerelease, which the installer's "latest" skips.
2. `.github/workflows/release.yml` packages all four targets, verifies them on
   Linux amd64, Linux arm64 and macOS (checksums, the version, the full suite
   against the packaged binary, the one-liner end to end), and only then
   publishes.

To rehearse locally: `scripts/package.sh v0.0.0-test /tmp/rel/latest/download`
and `FIRETOWER_RELEASES=file:///tmp/rel bash -s -- --no-claude < install.sh`
(with a throwaway `HOME`).
