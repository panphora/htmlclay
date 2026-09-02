# Shared setup for the Linux checks. Every check runs HTML Clay as an ordinary user
# inside a throwaway HOME, with xdg-open and notify-send replaced by stubs that
# record what the app asked the desktop to do. Source this, do not run it.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BIN="${HTMLCLAY_BIN:-$REPO/htmlclay}"
OUT="${HTMLCLAY_CHECK_OUT:-$REPO/linux-check-artifacts}"
CHECK="$(basename "${BASH_SOURCE[1]:-check}" .sh)"
mkdir -p "$OUT/$CHECK"

[ "$(id -u)" -ne 0 ] || { echo "FAIL: run as an ordinary user; four permission tests skip under root" >&2; exit 1; }
[ -x "$BIN" ] || { echo "FAIL: no htmlclay binary at $BIN (build with packaging/linux/build.sh or set HTMLCLAY_BIN)" >&2; exit 1; }

LAB="$(mktemp -d /tmp/htmlclay-lab.XXXXXX)"
export HTMLCLAY_LAB="$LAB"
export PLAYWRIGHT_BROWSERS_PATH="${PLAYWRIGHT_BROWSERS_PATH:-$HOME/.cache/ms-playwright}"
export HOME="$LAB/home"
export XDG_CONFIG_HOME="$HOME/.config" XDG_DATA_HOME="$HOME/.local/share" XDG_CACHE_HOME="$HOME/.cache"
mkdir -p "$HOME" "$XDG_CONFIG_HOME" "$XDG_DATA_HOME"
STUBS="$LAB/stubs"; mkdir -p "$STUBS"
export PATH="$STUBS:$PATH"

# The app opens URLs with xdg-open and shows banners with notify-send. Record both.
printf '#!/bin/sh\nprintf "%%s\\n" "$1" >> "$HTMLCLAY_LAB/opened-urls"\n' > "$STUBS/xdg-open"
printf '#!/bin/sh\nprintf "%%s\\n" "$*" >> "$HTMLCLAY_LAB/notifications"\n' > "$STUBS/notify-send"
chmod +x "$STUBS/xdg-open" "$STUBS/notify-send"

pass() { echo "ok   [$CHECK] $*"; }
fail() { echo "FAIL [$CHECK] $*" >&2; [ -f "$LAB/htmlclay.log" ] && { echo "--- htmlclay.log" >&2; tail -40 "$LAB/htmlclay.log" >&2; }; exit 1; }
wait_for_file() { local f="$1" t="${2:-30}" i; for ((i=0; i<t*10; i++)); do [ -s "$f" ] && return 0; sleep 0.1; done; return 1; }
keep() { cp -r "$@" "$OUT/$CHECK/" 2>/dev/null || true; }

# Pre-trust a folder the way the tray's Add Trusted Folder would: path plus the
# device:inode identity the app checks before granting anything.
trust_folder() {
  local dir="$1" extra="${2:-}"
  mkdir -p "$dir" "$XDG_CONFIG_HOME/htmlclay"
  printf '{%s"workspaceFolders":[{"path":"%s","identity":"%s"}]}\n' "$extra" "$dir" "$(stat -c '%d:%i' "$dir")" \
    > "$XDG_CONFIG_HOME/htmlclay/config.json"
}

# Start the app on a file; its PID lands in APP_PID and the URL it asked the browser
# for in $LAB/opened-urls. APP_PATH, when set, is the PATH the app itself sees.
start_app() {
  rm -f "$LAB/opened-urls"
  PATH="${APP_PATH:-$PATH}" "$BIN" "$@" > "$LAB/htmlclay.log" 2>&1 &
  APP_PID=$!
}

# Stop the app with SIGTERM the way a session logout does and leave its exit code in APP_RC.
stop_app() {
  APP_RC=0
  kill -TERM "$1" 2>/dev/null || true
  wait "$1" || APP_RC=$?
}

finish() { keep "$LAB/htmlclay.log" "$LAB/opened-urls" "$LAB/notifications"; }
trap finish EXIT
