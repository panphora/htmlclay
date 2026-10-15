# Sourced by build.sh and sparkle-appcast.sh: the pinned Sparkle release and a
# cached, checksum-verified copy of it.
#
# 2.9.6 is the last line that supports macOS 11, HTML Clay's minimum; 2.10
# needs macOS 12. The sha256 is GitHub's own digest for the release asset.
SPARKLE_VERSION="2.9.6"
SPARKLE_SHA256="52bf9e88cdd972fc0c81501377a880e90d47031bd8ca5462488f843e2609e192"
SPARKLE_URL="https://github.com/sparkle-project/Sparkle/releases/download/${SPARKLE_VERSION}/Sparkle-${SPARKLE_VERSION}.tar.xz"
SPARKLE_CACHE="${SPARKLE_CACHE:-$HOME/Library/Caches/htmlclay-build/sparkle-${SPARKLE_VERSION}}"

# fetch_sparkle leaves the release extracted in $SPARKLE_CACHE: the framework
# and bin/sign_update. The tarball is checked before anything is extracted, and
# the cache counts only once .verified exists, so an interrupted run starts over.
fetch_sparkle() {
  if [ -f "$SPARKLE_CACHE/.verified" ]; then
    return
  fi
  rm -rf "$SPARKLE_CACHE"
  mkdir -p "$SPARKLE_CACHE"
  local tarball="$SPARKLE_CACHE.tar.xz"
  curl -fsSL --retry 3 -o "$tarball" "$SPARKLE_URL"
  echo "$SPARKLE_SHA256  $tarball" | shasum -a 256 -c -
  tar -xJf "$tarball" -C "$SPARKLE_CACHE"
  rm -f "$tarball"
  touch "$SPARKLE_CACHE/.verified"
}
