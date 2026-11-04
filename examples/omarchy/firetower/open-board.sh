#!/bin/sh
# Open the board once: focus its window when one is open, else open one.
# `firetower board --start` starts the board in the background if it isn't
# up (and does nothing if it is), so a bar click never piles up servers;
# only the window is Omarchy's to find.
#
# The window is a web app window (Omarchy's omarchy-launch-webapp) onto
# http://127.0.0.1:<port>/, so Hyprland knows it by a class starting
# chrome-127.0.0.1 and a title ending in "Firetower" (the title carries a
# count when something needs a look).
ft="${1:-$HOME/.local/bin/firetower}"

err=$("$ft" board --start 2>&1) || { notify-send "Firetower" "$err"; exit 1; }

addr=$(hyprctl clients -j | jq -r '.[] | select((.class | startswith("chrome-127.0.0.1")) and (.title | test("Firetower$"))) | .address' | head -n1)
if [ -n "$addr" ]; then
  # Hyprland's Lua dispatcher first, then the older one.
  hyprctl dispatch "hl.dsp.focus({ window = \"address:$addr\" })" >/dev/null 2>&1 \
    || hyprctl dispatch focuswindow "address:$addr" >/dev/null
  exit 0
fi
# Only now, with no window to focus: a one-time address. The key itself would
# sit on the window's command line, which everyone on the machine can read.
url=$("$ft" board --url --once) || { notify-send "Firetower" "No address for the board."; exit 1; }
exec omarchy-launch-webapp "$url"
