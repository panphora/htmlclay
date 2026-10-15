#!/bin/bash
# Usage: sparkle-appcast.sh VERSION ZIP OUT_DIR
#
# Writes OUT_DIR/appcast.xml, a signed one-item Sparkle feed for ZIP, and a copy
# of ZIP beside it. The EdDSA private key comes from SPARKLE_ED_PRIVATE_KEY (the
# base64 seed `generate_keys -x` exports) and must be the private half of
# SPARKLE_PUBLIC_ED_KEY, the key build.sh put in the app. The enclosure names
# $SPARKLE_BASE_URL/VERSION/<zip>?sha256=<hex>: the query is only a cache key,
# so a rebuilt archive is never served from a cached copy of the old bytes.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/sparkle-common.sh"

VERSION="$1"
ZIP="$2"
OUT="$3"
BASE_URL="${SPARKLE_BASE_URL:-https://download.htmlclay.com/update}"

if ! printf '%s' "$VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$'; then
  echo "version must be X.Y.Z: $VERSION" >&2
  exit 1
fi
if [ -z "${SPARKLE_ED_PRIVATE_KEY:-}" ] || [ -z "${SPARKLE_PUBLIC_ED_KEY:-}" ]; then
  echo "SPARKLE_ED_PRIVATE_KEY and SPARKLE_PUBLIC_ED_KEY must both be set" >&2
  exit 1
fi

# A feed signed with any other key than the one in the app would be refused by
# every installed copy, so a mismatched pair stops the release here.
DERIVED=$(swift "$SCRIPT_DIR/sparkle-pubkey.swift")
if [ "$DERIVED" != "$SPARKLE_PUBLIC_ED_KEY" ]; then
  echo "SPARKLE_ED_PRIVATE_KEY is not the private half of SPARKLE_PUBLIC_ED_KEY" >&2
  exit 1
fi

fetch_sparkle
SIGN_UPDATE="$SPARKLE_CACHE/bin/sign_update"

NAME="$(basename "$ZIP")"
SHA256="$(shasum -a 256 "$ZIP" | cut -d' ' -f1)"
LENGTH="$(stat -f%z "$ZIP")"
SIGNATURE="$(printf '%s' "$SPARKLE_ED_PRIVATE_KEY" | "$SIGN_UPDATE" --ed-key-file - -p "$ZIP")"
MIN_OS="$(plutil -extract LSMinimumSystemVersion raw "$SCRIPT_DIR/Info.plist")"
PUB_DATE="$(LC_ALL=C date -u '+%a, %d %b %Y %H:%M:%S +0000')"

mkdir -p "$OUT"
cp "$ZIP" "$OUT/$NAME"
cat > "$OUT/appcast.xml" <<XML
<?xml version="1.0" encoding="utf-8"?>
<rss version="2.0" xmlns:sparkle="http://www.andymatuschak.org/xml-namespaces/sparkle">
  <channel>
    <title>HTML Clay</title>
    <item>
      <title>HTML Clay ${VERSION}</title>
      <pubDate>${PUB_DATE}</pubDate>
      <sparkle:version>${VERSION}</sparkle:version>
      <sparkle:shortVersionString>${VERSION}</sparkle:shortVersionString>
      <sparkle:minimumSystemVersion>${MIN_OS}</sparkle:minimumSystemVersion>
      <enclosure url="${BASE_URL}/${VERSION}/${NAME}?sha256=${SHA256}" length="${LENGTH}" type="application/octet-stream" sparkle:edSignature="${SIGNATURE}"/>
    </item>
  </channel>
</rss>
XML

# sign_update embeds the feed's own signature as a trailing comment, which
# SURequireSignedFeed makes every installed app check before reading the feed.
printf '%s' "$SPARKLE_ED_PRIVATE_KEY" | "$SIGN_UPDATE" --ed-key-file - --disable-signing-warning "$OUT/appcast.xml"
printf '%s' "$SPARKLE_ED_PRIVATE_KEY" | "$SIGN_UPDATE" --ed-key-file - --verify "$OUT/$NAME" "$SIGNATURE"
printf '%s' "$SPARKLE_ED_PRIVATE_KEY" | "$SIGN_UPDATE" --ed-key-file - --verify "$OUT/appcast.xml"
echo "Wrote $OUT/appcast.xml for $NAME ($LENGTH bytes)"
