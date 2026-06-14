#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ICON_PNG="$SCRIPT_DIR/htmlclay.png"
ICON_SVG="$SCRIPT_DIR/htmlclay.svg"
ICONS_ROOT="$HOME/.local/share/icons/hicolor"

installed=0
if [ -f "$ICON_PNG" ]; then
  mkdir -p "$ICONS_ROOT/256x256/apps"
  cp "$ICON_PNG" "$ICONS_ROOT/256x256/apps/htmlclay.png"
  installed=1
fi
if [ -f "$ICON_SVG" ]; then
  mkdir -p "$ICONS_ROOT/scalable/apps"
  cp "$ICON_SVG" "$ICONS_ROOT/scalable/apps/htmlclay.svg"
  installed=1
fi

if [ "$installed" -eq 0 ]; then
  echo "No icon found next to install-icon.sh, skipping icon install" >&2
  exit 0
fi

gtk-update-icon-cache -f -t "$ICONS_ROOT" 2>/dev/null || true
echo "Icon installed"
