#!/bin/bash
set -euo pipefail

# Undoes install.sh. It deliberately does not touch ~/.config/htmlclay: that
# directory holds the trusted-folder list and every saved version of every file
# HTML Clay has ever written, which is user data that happens to live next to the
# program rather than part of the installation.

BIN_DIR="${BIN_DIR:-$HOME/.local/bin}"
DATA_DIR="${XDG_DATA_HOME:-$HOME/.local/share}"
CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}"
ICONS_ROOT="$DATA_DIR/icons/hicolor"

removed=0

remove() {
  [ -e "$1" ] || return 0
  rm -f "$1"
  echo "Removed $1"
  removed=1
}

# Both defaults are checked, not only the current one. Which directory holds the
# binary depends on whether the user ran install.sh with --system, and nothing on
# disk records that, so an uninstall that only looked in one of them would leave
# a working htmlclay behind and report success.
for dir in "$BIN_DIR" "$HOME/.local/bin" /usr/local/bin; do
  target="$dir/htmlclay"
  [ -e "$target" ] || continue
  if [ -w "$dir" ]; then
    rm -f "$target"
  else
    echo "Removing $target (may prompt for sudo)..."
    sudo rm -f "$target"
  fi
  echo "Removed $target"
  removed=1
done

remove "$DATA_DIR/applications/htmlclay.desktop"
remove "$DATA_DIR/mime/packages/htmlclay-mime.xml"
remove "$ICONS_ROOT/256x256/apps/htmlclay.png"
remove "$ICONS_ROOT/scalable/apps/htmlclay.svg"
remove "$ICONS_ROOT/256x256/mimetypes/application-x-htmlclay.png"
remove "$ICONS_ROOT/scalable/mimetypes/application-x-htmlclay.svg"
remove "$CONFIG_DIR/autostart/htmlclay.desktop"

update-mime-database "$DATA_DIR/mime" 2>/dev/null || true
update-desktop-database "$DATA_DIR/applications" 2>/dev/null || true
gtk-update-icon-cache -f -t "$ICONS_ROOT" 2>/dev/null || true

echo ""
if [ "$removed" -eq 0 ]; then
  echo "Nothing to remove; HTML Clay was not installed for this user."
else
  echo "Uninstalled."
fi
echo "Your files, your trusted folders and your saved versions are still in $CONFIG_DIR/htmlclay."
echo "Delete that directory too if you want nothing left."
