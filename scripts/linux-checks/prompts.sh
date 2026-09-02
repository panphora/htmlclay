#!/bin/bash
# The permission prompt. A page inside a trusted folder requests an asset outside
# every trusted folder; the server parks the request and asks the user through
# zenity (kdialog as fallback). Under a virtual X display with the real zenity the
# dialog must appear and its default button must answer the request. With a stub
# zenity each of the three answers must map to the right grant. With neither tool
# on PATH the request fails closed and the user is told through a banner.
source "$(dirname "$0")/lib.sh"
for t in Xvfb zenity xdotool curl jq; do command -v $t >/dev/null || fail "$t is required"; done

export DISPLAY=:97
Xvfb :97 -screen 0 1280x800x24 -nolisten tcp > "$LAB/xvfb.log" 2>&1 &
XPID=$!; trap 'kill $XPID 2>/dev/null; finish' EXIT
sleep 1

DOCS="$HOME/docs"; OTHER="$HOME/other"; mkdir -p "$DOCS" "$OTHER"
echo '<!doctype html><html><body><img src="../other/asset.txt"></body></html>' > "$DOCS/page.htmlclay"
echo 'secret-asset' > "$OTHER/asset.txt"
CONFIG="$XDG_CONFIG_HOME/htmlclay/config.json"
trusted() { jq -e --arg p "$1" '[.workspaceFolders[]? | select(.path == $p)] | length > 0' "$CONFIG" >/dev/null 2>&1; }

# Serve the page from a trusted folder and return the origin the app announced.
open_site() {
  trust_folder "$DOCS"
  start_app --no-tray "$DOCS/page.htmlclay"; PID=$APP_PID
  wait_for_file "$LAB/opened-urls" 30 || fail "[$1] the app never served the page"
  URL="$(head -1 "$LAB/opened-urls")"
  ORIGIN="$(sed -E 's#^(https?://[^/]+).*#\1#' <<<"$URL")"
}

# Ask for the out-of-scope asset; the request is held open until the prompt is answered.
ask_asset() {
  curl -s --max-time 60 -o "$LAB/asset.body" -w '%{http_code}' "$ORIGIN/other/asset.txt" > "$LAB/asset.code" 2>/dev/null
}

# A. real zenity: the dialog shows; Return takes its default button.
open_site zenity
ask_asset &
CURL=$!
wid="$(timeout 30 xdotool search --sync --class zenity 2>/dev/null | head -1 || true)"
[ -n "$wid" ] || { kill $CURL 2>/dev/null; fail "[zenity] no dialog window appeared within 30s"; }
xdotool windowactivate --sync "$wid" 2>/dev/null || true
sleep 2
import -window root "$LAB/zenity-prompt.png" 2>/dev/null || true; keep "$LAB/zenity-prompt.png"
xdotool windowactivate --sync "$wid" 2>/dev/null || true
xdotool key --clearmodifiers Return
wait $CURL || true
code="$(cat "$LAB/asset.code")"
case "$code" in
  200) grep -q secret-asset "$LAB/asset.body" || fail "[zenity] 200 but wrong body"
       trusted "$OTHER" && fail "[zenity] Allow Once must not trust the folder"
       pass "real zenity dialog appeared; Return = Allow Once, the held request completed with 200" ;;
  403) pass "real zenity dialog appeared; Return = Deny on this zenity, the held request got the fixed 403 (default button is Deny, worth knowing)" ;;
  *)   fail "[zenity] held request ended with '$code'" ;;
esac
stop_app "$PID"
cp "$LAB/htmlclay.log" "$LAB/htmlclay-zenity.log"; keep "$LAB/htmlclay-zenity.log"

# B. stub zenity answering "Trust this folder": served, and the folder is now trusted.
printf '#!/bin/sh\necho "Trust this folder"\nexit 1\n' > "$STUBS/zenity"; chmod +x "$STUBS/zenity"
open_site trust
ask_asset
[ "$(cat "$LAB/asset.code")" = "200" ] || fail "[trust] held request got $(cat "$LAB/asset.code"), expected 200"
grep -q secret-asset "$LAB/asset.body" || fail "[trust] 200 but wrong body"
sleep 1
trusted "$OTHER" || fail "[trust] folder not recorded in config: $(cat "$CONFIG" 2>/dev/null)"
stop_app "$PID"
pass "Trust this folder: request served and the folder recorded in config"

# C. stub zenity answering Deny: the fixed 403, and no second prompt for the same tree.
printf '#!/bin/sh\nexit 1\n' > "$STUBS/zenity"
open_site deny
ask_asset
[ "$(cat "$LAB/asset.code")" = "403" ] || fail "[deny] held request got $(cat "$LAB/asset.code"), expected 403"
: > "$LAB/zenity-calls"; printf '#!/bin/sh\necho called >> "%s"\nexit 1\n' "$LAB/zenity-calls" > "$STUBS/zenity"
ask_asset
[ "$(cat "$LAB/asset.code")" = "403" ] || fail "[deny] second request got $(cat "$LAB/asset.code"), expected 403"
[ ! -s "$LAB/zenity-calls" ] || fail "[deny] a denied tree prompted again"
stop_app "$PID"
pass "Deny: fixed 403, and the denied tree is not asked about twice"

# D. neither tool: fail closed, log it, and tell the user via notify-send.
rm -f "$STUBS/zenity"
APP_PATH="$STUBS:/nonexistent" open_site nodialog
ask_asset
[ "$(cat "$LAB/asset.code")" = "403" ] || fail "[no dialog tool] got $(cat "$LAB/asset.code"), expected 403"
grep -qi "zenity or kdialog" "$LAB/htmlclay.log" || fail "[no dialog tool] log does not name the missing dialog tool"
grep -qi "zenity" "$LAB/notifications" 2>/dev/null || fail "[no dialog tool] no notification banner was attempted"
stop_app "$PID"
cp "$LAB/htmlclay.log" "$LAB/htmlclay-nodialog.log"; keep "$LAB/htmlclay-nodialog.log"
pass "no zenity/kdialog: refused with 403, logged, and a banner was attempted"
