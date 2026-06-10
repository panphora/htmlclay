#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ICON_SRC="$SCRIPT_DIR/htmlclay.png"

if [ ! -f "$ICON_SRC" ]; then
  echo "No icon at $ICON_SRC, skipping icon install" >&2
  exit 0
fi

ICON_DIR="$HOME/.local/share/icons/hicolor/256x256/apps"
mkdir -p "$ICON_DIR"
cp "$ICON_SRC" "$ICON_DIR/htmlclay.png"
gtk-update-icon-cache -f -t "$HOME/.local/share/icons/hicolor" 2>/dev/null || true

echo "Icon installed"
