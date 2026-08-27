#!/usr/bin/env bash
#
# render-background.sh — render the DMG background PNGs from background.svg.
#
# Run this after editing background.svg. The outputs are committed so the CI
# release (build-macos) needs only `appdmg`, not librsvg. appdmg auto-detects
# background@2x.png next to background.png and packs both into the retina TIFF.
#
# Requires: rsvg-convert (brew install librsvg).
set -euo pipefail
cd "$(dirname "$0")"

command -v rsvg-convert >/dev/null || { echo "need rsvg-convert (brew install librsvg)"; exit 1; }

rsvg-convert -w 660  -h 400 background.svg -o background.png
rsvg-convert -w 1320 -h 800 background.svg -o "background@2x.png"

echo "rendered background.png (660x400) + background@2x.png (1320x800)"
