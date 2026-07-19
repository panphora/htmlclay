#!/bin/bash
set -euo pipefail

# Installs a released HTMLClay build into /Applications and relaunches it.
# The DMG is the notarized artifact CI published to R2, so this installs
# exactly what users download. Defaults to the version in main.go.

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

VERSION="${1:-$(grep 'var version' main.go | sed 's/.*"\(.*\)"/\1/')}"
DMG_NAME="HTMLClay-${VERSION}-universal.dmg"
DMG_URL="https://download.htmlclay.com/${DMG_NAME}"

TMP_DIR="$(mktemp -d)"
MOUNT_POINT=""

cleanup() {
  [ -n "$MOUNT_POINT" ] && hdiutil detach "$MOUNT_POINT" -quiet 2>/dev/null || true
  rm -rf "$TMP_DIR"
}
trap cleanup EXIT

info "Downloading ${DMG_NAME}..."
curl -fL --retry 5 --retry-delay 5 --retry-all-errors \
  -o "${TMP_DIR}/${DMG_NAME}" "$DMG_URL"

info "Installing to /Applications..."
pkill -f "HTMLClay.app" || true
sleep 1

MOUNT_POINT="$(hdiutil attach "${TMP_DIR}/${DMG_NAME}" -nobrowse | grep -o '/Volumes/.*' | tail -1)"
rm -rf "/Applications/HTMLClay.app"
ditto "${MOUNT_POINT}/HTMLClay.app" "/Applications/HTMLClay.app"
hdiutil detach "$MOUNT_POINT" -quiet
MOUNT_POINT=""
success "Installed HTMLClay v${VERSION} to /Applications"

open -a "/Applications/HTMLClay.app"
success "Launched HTMLClay"
