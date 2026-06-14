#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

BIN_DIR="${BIN_DIR:-/usr/local/bin}"
DATA_DIR="$HOME/.local/share"

echo "Installing htmlclay to $BIN_DIR (may prompt for sudo)..."
sudo install -m 755 htmlclay "$BIN_DIR/htmlclay"

mkdir -p "$DATA_DIR/applications" "$DATA_DIR/mime/packages"
cp htmlclay.desktop "$DATA_DIR/applications/"
cp htmlclay-mime.xml "$DATA_DIR/mime/packages/"

bash "$SCRIPT_DIR/install-icon.sh"

update-mime-database "$DATA_DIR/mime" 2>/dev/null || true
update-desktop-database "$DATA_DIR/applications" 2>/dev/null || true

echo ""
echo "Installed. Double-click a .htmlclay file, or run: htmlclay yourfile.htmlclay"
