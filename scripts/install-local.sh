#!/bin/bash
set -euo pipefail

# Installs a released HTMLClay build into /Applications and relaunches it.
# The DMG is the notarized artifact CI published to R2, so this installs
# exactly what users download. Defaults to the version in cmd/htmlclay/main.go.

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT_DIR"

GREEN='\033[0;32m'
BLUE='\033[0;34m'
RESET='\033[0m'

info()    { echo -e "${BLUE}→ $1${RESET}"; }
success() { echo -e "${GREEN}✓ $1${RESET}"; }

if [ "$(uname -s)" != "Darwin" ]; then
  echo "install-local.sh only runs on macOS"
  exit 1
fi

VERSION="${1:-$(grep 'var version' cmd/htmlclay/main.go | sed 's/.*"\(.*\)"/\1/')}"
DMG_NAME="HTMLClay-${VERSION}-universal.dmg"
DMG_URL="https://download.htmlclay.com/${DMG_NAME}"

# Tests point this at a fixture folder. It is never set otherwise.
APPS_DIR="${HTMLCLAY_INSTALL_APPS_DIR:-/Applications}"
DEST="$APPS_DIR/HTMLClay.app"
STAGE="$APPS_DIR/.HTMLClay.app.new-$$"
ASIDE="$APPS_DIR/.HTMLClay.app.old-$$"
STAGED=""

TMP_DIR="$(mktemp -d)"
MOUNT_POINT=""

# The new bundle is copied beside the app first and switched in with two
# renames, so a complete app is at DEST, or aside for the next run to put back,
# at every point. settle runs on every exit, TERM included (bash runs the EXIT
# trap then): it finishes a switch that was cut short, forward when the staged
# copy is complete and back otherwise, and removes both copies.
settle() {
  if [ ! -e "$DEST" ]; then
    if [ -n "$STAGED" ] && [ -d "$STAGE" ]; then
      mv "$STAGE" "$DEST"
    elif [ -d "$ASIDE" ]; then
      mv "$ASIDE" "$DEST"
    fi
  fi
  rm -rf "$STAGE" "$ASIDE"
}

# A run killed outright (hypersave sends KILL 5 s after TERM) leaves its copies
# behind. Between the two renames the aside copy is the only complete app.
recover() {
  local old
  for old in "$APPS_DIR"/.HTMLClay.app.old-*; do
    [ -e "$old" ] || continue
    if [ -e "$DEST" ]; then
      rm -rf "$old"
    else
      mv "$old" "$DEST"
    fi
  done
  rm -rf "$APPS_DIR"/.HTMLClay.app.new-*
}

cleanup() {
  settle
  [ -n "$MOUNT_POINT" ] && hdiutil detach "$MOUNT_POINT" -quiet 2>/dev/null || true
  rm -rf "$TMP_DIR"
}
trap cleanup EXIT

recover

info "Downloading ${DMG_NAME}..."
curl -fL --connect-timeout 20 --max-time 600 --retry 5 --retry-delay 5 --retry-all-errors \
  -o "${TMP_DIR}/${DMG_NAME}" "$DMG_URL"

info "Installing to ${APPS_DIR}..."
pkill -f "HTMLClay.app" || true
sleep 1

MOUNT_POINT="$(hdiutil attach "${TMP_DIR}/${DMG_NAME}" -nobrowse | grep -o '/Volumes/.*' | tail -1)"
ditto "${MOUNT_POINT}/HTMLClay.app" "$STAGE"
STAGED=1
if [ -e "$DEST" ]; then
  mv "$DEST" "$ASIDE"
fi
mv "$STAGE" "$DEST"
rm -rf "$ASIDE"
hdiutil detach "$MOUNT_POINT" -quiet
MOUNT_POINT=""
success "Installed HTMLClay v${VERSION} to ${APPS_DIR}"

open -a "$DEST"
success "Launched HTMLClay"
