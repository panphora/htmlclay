#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
cd "$ROOT_DIR"
source "$SCRIPT_DIR/sparkle-common.sh"

IDENTITY="Hyperspace Systems LLC (JC7YGGXYKH)"
APP="HTMLClay.app"
CONTENTS="$APP/Contents"
MACOS="$CONTENTS/MacOS"
RESOURCES="$CONTENTS/Resources"
FRAMEWORKS="$CONTENTS/Frameworks"
SPARKLE_FW="$FRAMEWORKS/Sparkle.framework"
UNSIGNED=false
UNIVERSAL=false

for arg in "$@"; do
  case "$arg" in
    --unsigned) UNSIGNED=true ;;
    --universal) UNIVERSAL=true ;;
  esac
done

# Load .env if present
if [ -f .env ]; then
  set -a
  source .env
  set +a
fi

# Determine version and architecture
VERSION="${VERSION:-$(grep 'var version' cmd/htmlclay/main.go | sed 's/.*"\(.*\)"/\1/')}"
if [ "$UNIVERSAL" = true ]; then
  ARCH="universal"
else
  ARCH="$(uname -m)"
  [ "$ARCH" = "x86_64" ] && ARCH="amd64"
fi

echo "Building HTMLClay v${VERSION} (${ARCH})..."

# Build .app bundle
rm -rf "$APP"
mkdir -p "$MACOS" "$RESOURCES" "$FRAMEWORKS"

# UPDATE_CHANNEL is set only by the release workflow. A local build leaves it
# empty, so it never updates itself from a published release.
LDFLAGS="-s -w -X main.version=${VERSION} -X main.updateChannel=${UPDATE_CHANNEL:-}"
require_tailwind() {
  go version -m "$1" | grep -q -- '-tags=tailwind_bundle' || { echo "error: $1 was built without the Tailwind bundle" >&2; exit 1; }
}
if [ "$UNIVERSAL" = true ]; then
  GOOS= GOARCH= go run ./cmd/fetchtailwind -goos darwin -goarch arm64
  GOOS= GOARCH= go run ./cmd/fetchtailwind -goos darwin -goarch amd64
  CGO_ENABLED=1 GOARCH=arm64 go build -tags tailwind_bundle -trimpath -ldflags="$LDFLAGS" -o "$MACOS/htmlclay-arm64" ./cmd/htmlclay
  CGO_ENABLED=1 GOARCH=amd64 go build -tags tailwind_bundle -trimpath -ldflags="$LDFLAGS" -o "$MACOS/htmlclay-amd64" ./cmd/htmlclay
  require_tailwind "$MACOS/htmlclay-arm64"
  require_tailwind "$MACOS/htmlclay-amd64"
  lipo -create -output "$MACOS/htmlclay" "$MACOS/htmlclay-arm64" "$MACOS/htmlclay-amd64"
  rm -f "$MACOS/htmlclay-arm64" "$MACOS/htmlclay-amd64"
else
  target_goarch="$(go env GOARCH)"
  GOOS= GOARCH= go run ./cmd/fetchtailwind -goos darwin -goarch "$target_goarch"
  CGO_ENABLED=1 go build -tags tailwind_bundle -trimpath -ldflags="$LDFLAGS" -o "$MACOS/htmlclay" ./cmd/htmlclay
  require_tailwind "$MACOS/htmlclay"
fi

# Copy Info.plist and inject version using plutil
cp packaging/macos/Info.plist "$CONTENTS/"
plutil -replace CFBundleVersion -string "$VERSION" "$CONTENTS/Info.plist"
plutil -replace CFBundleShortVersionString -string "$VERSION" "$CONTENTS/Info.plist"

# The EdDSA public key Sparkle checks every update and appcast against. Without
# one the app never starts Sparkle and keeps linking to the download page.
if [ -n "${SPARKLE_PUBLIC_ED_KEY:-}" ]; then
  plutil -insert SUPublicEDKey -string "$SPARKLE_PUBLIC_ED_KEY" "$CONTENTS/Info.plist"
fi

# Sparkle is loaded at run time from Contents/Frameworks, not linked, so the
# binary needs no rpath. HTML Clay is not sandboxed, so Sparkle runs its
# installer without the XPC services; they are removed the way Sparkle's own
# strip-framework.sh does, along with the headers, which only a compiler reads.
fetch_sparkle
ditto "$SPARKLE_CACHE/Sparkle.framework" "$SPARKLE_FW"
for part in XPCServices Headers PrivateHeaders Modules; do
  rm -rf "$SPARKLE_FW/$part" "$SPARKLE_FW/Versions/B/$part"
done

echo -n "APPL????" > "$CONTENTS/PkgInfo"

[ -f packaging/macos/htmlclay.icns ] && cp packaging/macos/htmlclay.icns "$RESOURCES/"
[ -f packaging/macos/doc.icns ] && cp packaging/macos/doc.icns "$RESOURCES/"

# Sign from the inside out, in the order Sparkle documents for Developer ID:
# its two helpers, then the framework, then the app. Never --deep, which would
# sign the helpers with the app's entitlements.
if [ "$UNSIGNED" = true ]; then
  SIGN=(codesign -f -s -)
  SIGN_APP=("${SIGN[@]}")
else
  SIGN=(codesign -f -s "$IDENTITY" --options runtime)
  SIGN_APP=("${SIGN[@]}" --entitlements packaging/macos/entitlements.plist)
fi
"${SIGN[@]}" "$SPARKLE_FW/Versions/B/Autoupdate"
"${SIGN[@]}" "$SPARKLE_FW/Versions/B/Updater.app"
"${SIGN[@]}" "$SPARKLE_FW"
"${SIGN_APP[@]}" "$APP"
if [ "$UNSIGNED" = true ]; then
  echo "Signed $APP (ad-hoc, unsigned)"
else
  echo "Signed $APP with $IDENTITY"
fi

# Verify signature
codesign -v --deep --strict "$APP"
echo "Signature verified"

# Create DMG with drag-to-Applications layout via appdmg
# (deterministic, no AppleScript/Finder scripting; reads packaging/macos/dmg/appdmg.json).
DMG_NAME="HTMLClay-${VERSION}-${ARCH}.dmg"
rm -f "$DMG_NAME"

npx --yes appdmg packaging/macos/dmg/appdmg.json "$DMG_NAME"

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

# The DMG's ticket covers the app inside it, so the bare app staples from it
# with no second submission. Stapled before it is zipped, so a Sparkle update
# opens offline too. The ticket can take a moment to reach the CDN; an app that
# still will not staple ships unstapled, and Gatekeeper checks it online.
for attempt in 1 2 3 4 5; do
  if xcrun stapler staple "$APP"; then
    break
  fi
  if [ "$attempt" = 5 ]; then
    echo "::warning::Could not staple $APP; the update archive ships without a ticket"
    break
  fi
  sleep 15
done

# The archive Sparkle installs. ditto keeps the bundle's symlinks, extended
# attributes and signature intact, which zip does not.
UPDATE_ZIP="HTMLClay-${VERSION}-${ARCH}.app.zip"
rm -f "$UPDATE_ZIP"
ditto -c -k --sequesterRsrc --keepParent "$APP" "$UPDATE_ZIP"
echo "Created $UPDATE_ZIP"
