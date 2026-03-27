#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
cd "$ROOT_DIR"

IDENTITY="Hyperspace Systems LLC (JC7YGGXYKH)"
APP="HTMLClay.app"
CONTENTS="$APP/Contents"
MACOS="$CONTENTS/MacOS"
RESOURCES="$CONTENTS/Resources"
UNSIGNED=false

for arg in "$@"; do
  case "$arg" in
    --unsigned) UNSIGNED=true ;;
  esac
done

# Load .env if present
if [ -f .env ]; then
  set -a
  source .env
  set +a
fi

# Determine version and architecture
VERSION="${VERSION:-$(grep 'var version' main.go | sed 's/.*"\(.*\)"/\1/')}"
ARCH="$(uname -m)"
[ "$ARCH" = "x86_64" ] && ARCH="amd64"

echo "Building HTMLClay v${VERSION} (${ARCH})..."

# Build .app bundle
rm -rf "$APP"
mkdir -p "$MACOS" "$RESOURCES"

CGO_ENABLED=1 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o "$MACOS/htmlclay" .

# Copy Info.plist and inject version using plutil
cp dist/macos/Info.plist "$CONTENTS/"
plutil -replace CFBundleVersion -string "$VERSION" "$CONTENTS/Info.plist"
plutil -replace CFBundleShortVersionString -string "$VERSION" "$CONTENTS/Info.plist"

echo -n "APPL????" > "$CONTENTS/PkgInfo"

[ -f dist/macos/htmlclay.icns ] && cp dist/macos/htmlclay.icns "$RESOURCES/"
[ -f dist/macos/doc.icns ] && cp dist/macos/doc.icns "$RESOURCES/"

# Sign
if [ "$UNSIGNED" = true ]; then
  codesign -f -s - --deep "$APP"
  echo "Signed $APP (ad-hoc, unsigned)"
else
  codesign -f -s "$IDENTITY" \
    --options runtime \
    --entitlements dist/macos/entitlements.plist \
    --deep \
    "$APP"
  echo "Signed $APP with $IDENTITY"
fi

# Verify signature
codesign -v --deep --strict "$APP"
echo "Signature verified"

# Create DMG
DMG_NAME="HTMLClay-${VERSION}-${ARCH}.dmg"
rm -f "$DMG_NAME"

hdiutil create -volname "HTMLClay" \
  -srcfolder "$APP" \
  -ov -format UDZO \
  "$DMG_NAME"

if [ "$UNSIGNED" = false ]; then
  codesign -f -s "$IDENTITY" "$DMG_NAME"
fi

echo "Created $DMG_NAME"

# Notarize
if [ "$UNSIGNED" = true ]; then
  echo "Skipping notarization (unsigned build)"
  exit 0
fi

if [ -z "${APPLE_ID:-}" ] || [ -z "${APPLE_TEAM_ID:-}" ] || [ -z "${APPLE_APP_SPECIFIC_PASSWORD:-}" ]; then
  echo "Skipping notarization (missing Apple credentials)"
  echo "Set APPLE_ID, APPLE_TEAM_ID, APPLE_APP_SPECIFIC_PASSWORD in .env"
  exit 0
fi

echo "Submitting $DMG_NAME for notarization..."
SUBMIT_OUTPUT=$(xcrun notarytool submit "$DMG_NAME" \
  --apple-id "$APPLE_ID" \
  --team-id "$APPLE_TEAM_ID" \
  --password "$APPLE_APP_SPECIFIC_PASSWORD" \
  --wait \
  --output-format json)

STATUS=$(echo "$SUBMIT_OUTPUT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('status','Unknown'))")

if [ "$STATUS" != "Accepted" ]; then
  echo "Notarization failed with status: $STATUS"
  echo "$SUBMIT_OUTPUT"
  exit 1
fi

xcrun stapler staple "$DMG_NAME"
echo "Notarized and stapled $DMG_NAME"
