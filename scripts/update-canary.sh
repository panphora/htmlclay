#!/bin/bash
set -uo pipefail

# Asks the HTML Clay running on this Mac to update itself to VERSION through
# its own update path (Sparkle, then the same restart as the tray's Restart to
# Update row), and waits for the new version to serve. Only when the trigger
# cannot be delivered (the app is not running, or the installed or running
# build predates update-now) does it fall back to install-local.sh, which
# replaces the bundle by hand. A running app that refuses, for any reason, is
# a failure and is left alone, and so is everything after it accepted: a pkill
# in the middle of a Sparkle install can leave a half-swapped bundle, and the
# recovery state in updates/ is the record of what went wrong. A version the
# recovery supervisor rolled back is never installed again, by either path.
#
# Usage: scripts/update-canary.sh X.Y.Z
#
# Exit codes: 0 updated itself, 10 fell back to an install that succeeded,
# 1 failed. The last line on stdout is one JSON object:
#   {"app":"htmlclay","outcome":"self-updated"|"installed"|"failed",
#    "from":"1.13.3","to":"1.14.0","reason":"...","seconds":41,"retry":false}
# retry is true only when the app was left untouched on the old version and
# running the canary again later is safe and likely to work. Everything else
# goes to stderr. It never prompts, and the trigger and the
# wait for proof end at the deadline (HTMLCLAY_CANARY_DEADLINE, 240 s).

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"

TARGET="${1:-}"
APP="${HTMLCLAY_CANARY_APP:-/Applications/HTMLClay.app}"
CONFIG_DIR="${HTMLCLAY_CANARY_CONFIG_DIR:-$HOME/Library/Application Support/htmlclay}"
INSTALL="${HTMLCLAY_CANARY_INSTALL:-$ROOT_DIR/scripts/install-local.sh}"
DEADLINE="${HTMLCLAY_CANARY_DEADLINE:-240}"
RESEND="${HTMLCLAY_CANARY_RESEND:-30}"
POLL="${HTMLCLAY_CANARY_POLL:-2}"

PLIST="$APP/Contents/Info.plist"
BIN="$APP/Contents/MacOS/htmlclay"
HEALTH="$CONFIG_DIR/updates/health.json"
STATE="$CONFIG_DIR/updates/state.json"
LOCK="$CONFIG_DIR/lock"
START=$SECONDS
FROM=""

say() { echo "[update-canary] $*" >&2; }

json_string() {
  local s="$1"
  s="${s//\\/\\\\}"
  s="${s//\"/\\\"}"
  s="$(printf '%s' "$s" | tr -d '\000-\037')"
  printf '"%s"' "$s"
}

finish() {
  local outcome="$1" reason="$2" code="$3" retry="${4:-false}"
  printf '{"app":"htmlclay","outcome":%s,"from":%s,"to":%s,"reason":%s,"seconds":%d,"retry":%s}\n' \
    "$(json_string "$outcome")" "$(json_string "$FROM")" "$(json_string "$TARGET")" \
    "$(json_string "$reason")" "$((SECONDS - START))" "$retry"
  exit "$code"
}

# plutil reads JSON as well as plists, so the canary needs no jq or python.
read_key() { plutil -extract "$2" raw -o - "$1" 2>/dev/null; }

lock_pid() { head -n 1 "$LOCK" 2>/dev/null | tr -d '[:space:]'; }

stop_if_rolled_back() {
  if [ -f "$STATE" ] && [ "$(read_key "$STATE" failed.version)" = "$TARGET" ]; then
    finish failed "v$TARGET was rolled back: $(read_key "$STATE" failed.reason)" 1
  fi
}

fallback() {
  local why="$1"
  stop_if_rolled_back
  say "Falling back to install-local.sh: $why"
  if bash "$INSTALL" "$TARGET" >&2; then
    finish installed "$why" 10
  fi
  finish failed "$why; install-local.sh failed too" 1
}

# The new version is serving: health.json, written by the process that holds
# the single-instance lock, names TARGET in phase serving under a live pid that
# is not the one that took the request, and the bundle on disk is TARGET.
updated() {
  [ -f "$HEALTH" ] || return 1
  [ "$(read_key "$HEALTH" version)" = "$TARGET" ] || return 1
  [ "$(read_key "$HEALTH" phase)" = "serving" ] || return 1
  local pid
  pid="$(read_key "$HEALTH" pid)"
  [[ "$pid" =~ ^[0-9]+$ ]] || return 1
  [ "$pid" != "$OLD_PID" ] || return 1
  kill -0 "$pid" 2>/dev/null || return 1
  [ "$(read_key "$PLIST" CFBundleShortVersionString)" = "$TARGET" ]
}

if ! [[ "$TARGET" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
  say "usage: scripts/update-canary.sh X.Y.Z"
  finish failed "not a version: '$TARGET'" 1
fi
if [ "$(uname -s)" != "Darwin" ]; then
  finish failed "update-canary.sh runs on macOS only" 1
fi

FROM="$(read_key "$PLIST" CFBundleShortVersionString || true)"

# An older binary would forward "update-now" to the running app as a file to
# open, or start the app with it when nothing is running, so only a bundle that
# declares the command is ever sent it.
if [ "$(read_key "$PLIST" HCUpdateNowCommand)" != "1" ]; then
  fallback "the installed HTML Clay ${FROM:-(none)} predates update-now"
fi

OLD_PID="$(lock_pid)"
ACCEPTED=false
STARTING=false
NEXT_SEND=0
REASON=""

while [ $((SECONDS - START)) -lt "$DEADLINE" ]; do
  stop_if_rolled_back
  if updated; then
    finish self-updated "" 0
  fi
  if [ $((SECONDS - START)) -ge "$NEXT_SEND" ]; then
    NEXT_SEND=$((SECONDS - START + RESEND))
    ANSWER="$("$BIN" update-now --version "$TARGET" </dev/null 2>/dev/null)"
    CODE=$?
    ANSWER="$(printf '%s' "$ANSWER" | tail -n 1)"
    case "$CODE" in
      0)
        if [ "$ANSWER" = "already $TARGET" ]; then
          if [ "$ACCEPTED" != true ]; then
            finish self-updated "already running v$TARGET" 0
          fi
          # The process that holds the lock now is not the one that took the
          # request, and it runs TARGET: the restart happened.
          NOW_PID="$(lock_pid)"
          if [ -n "$NOW_PID" ] && [ "$NOW_PID" != "$OLD_PID" ]; then
            finish self-updated "" 0
          fi
        fi
        [ "$ACCEPTED" = true ] || say "HTML Clay accepted the update to v$TARGET"
        ACCEPTED=true ;;
      2|4)
        # Not running, or too old to answer. After an accept this is the
        # restart in progress, so keep waiting.
        if [ "$ACCEPTED" != true ]; then
          if [ "$CODE" = 2 ]; then fallback "HTML Clay is not running"; fi
          fallback "the running HTML Clay predates update-now"
        fi ;;
      3)
        # A running app that can answer chose not to update, so a hand install
        # would override it: that is a failure, and the app is left alone.
        REASON="${ANSWER#refused }"
        if [ "$ACCEPTED" != true ]; then finish failed "HTML Clay refused: $REASON" 1; fi
        finish failed "HTML Clay accepted the update, then refused: $REASON" 1 ;;
      5)
        # Either the person cancelled the restart on screen, which the app
        # remembers until it restarts, or an install is already set up, which
        # a hand install would race. Neither is retried by running this again.
        REASON="${ANSWER#failed }"
        case "$REASON" in
          *"cancelled on screen"*) finish failed "$REASON; restart HTML Clay or use the tray to update" 1 ;;
          *) finish failed "$REASON" 1 ;;
        esac ;;
      6)
        # Still starting, before or after the restart: ask again next poll.
        STARTING=true
        NEXT_SEND=$((SECONDS - START)) ;;
      *)
        finish failed "htmlclay update-now exited $CODE: $ANSWER" 1 ;;
    esac
  fi
  sleep "$POLL"
done

if [ "$ACCEPTED" = true ]; then
  # Still the same process on the same bundle: the restart never began (an AI
  # edit, a running program or saves in flight hold it), so nothing changed.
  if [ -n "$OLD_PID" ] && kill -0 "$OLD_PID" 2>/dev/null &&
     [ "$(lock_pid)" = "$OLD_PID" ] &&
     [ "$(read_key "$PLIST" CFBundleShortVersionString)" = "$FROM" ]; then
    finish failed "HTML Clay accepted the update to v$TARGET and was not serving it after ${DEADLINE}s; v$FROM is still running untouched" 1 true
  fi
  finish failed "HTML Clay accepted the update to v$TARGET and was not serving it after ${DEADLINE}s" 1
fi
if [ "$STARTING" = true ]; then
  finish failed "HTML Clay was still starting after ${DEADLINE}s and never took the update to v$TARGET" 1 true
fi
finish failed "no answer from HTML Clay within ${DEADLINE}s" 1
