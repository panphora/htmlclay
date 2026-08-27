#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ICON_PNG="$SCRIPT_DIR/htmlclay.png"
ICON_SVG="$SCRIPT_DIR/htmlclay.svg"
# The document icon's filename is fixed, not a name we chose: freedesktop looks
# up a MIME type's icon under mimetypes/ by the type with its slash turned into a
# dash, so application/x-htmlclay resolves to application-x-htmlclay and to
# nothing else. Installing the app icon alone, which is what this script did
# until now, left every .htmlclay file drawn as a blank page.
DOC_PNG="$SCRIPT_DIR/application-x-htmlclay.png"
DOC_SVG="$SCRIPT_DIR/application-x-htmlclay.svg"
ICONS_ROOT="${XDG_DATA_HOME:-$HOME/.local/share}/icons/hicolor"

installed=0
install_icon() {
  local src="$1" dir="$2"
  [ -f "$src" ] || return 0
  mkdir -p "$ICONS_ROOT/$dir"
  cp "$src" "$ICONS_ROOT/$dir/$(basename "$src")"
  installed=1
}

install_icon "$ICON_PNG" 256x256/apps
install_icon "$ICON_SVG" scalable/apps
install_icon "$DOC_PNG" 256x256/mimetypes
install_icon "$DOC_SVG" scalable/mimetypes

if [ "$installed" -eq 0 ]; then
  echo "No icon found next to install-icon.sh, skipping icon install" >&2
  exit 0
fi

gtk-update-icon-cache -f -t "$ICONS_ROOT" 2>/dev/null || true
echo "Icons installed"
