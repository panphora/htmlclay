#!/bin/bash
set -euo pipefail

ICON_DIR="$HOME/.local/share/icons/hicolor/256x256/apps"
mkdir -p "$ICON_DIR"
cp dist/linux/htmlclay.png "$ICON_DIR/htmlclay.png"
gtk-update-icon-cache -f -t "$HOME/.local/share/icons/hicolor" 2>/dev/null || true

echo "Icon installed"
