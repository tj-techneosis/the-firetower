#!/bin/sh
# A SwiftBar (or xbar) plugin: the Firetower in the macOS menu bar.
# Copy it into SwiftBar's plugin folder; the 30s in the name is how often it
# refreshes. The icon is a pair of binoculars, coloured by the worst state,
# with the count of lookouts needing a look; the menu lists every lookout it shows.
#
# <xbar.title>Firetower</xbar.title>
# <xbar.desc>Every background watch at a glance.</xbar.desc>
# <swiftbar.hideRunInTerminal>true</swiftbar.hideRunInTerminal>
# <swiftbar.hideLastUpdated>false</swiftbar.hideLastUpdated>
exec "$HOME/.local/bin/firetower" bar --format swiftbar
