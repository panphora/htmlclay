#!/bin/bash
# Start on Login: with the preference on, a launch must (re)write the freedesktop
# autostart entry pointing at this exact binary, and --unregister must remove it.
source "$(dirname "$0")/lib.sh"
DOCS="$HOME/docs"; trust_folder "$DOCS" '"startOnLogin":true,'
echo '<!doctype html><html><body>x</body></html>' > "$DOCS/x.htmlclay"

start_app --no-tray "$DOCS/x.htmlclay"; PID=$APP_PID
wait_for_file "$LAB/opened-urls" 30 || fail "never served"
A="$XDG_CONFIG_HOME/autostart/htmlclay.desktop"
test -f "$A" || fail "no autostart entry written"
grep -qF "Exec=\"$BIN\"" "$A" || fail "autostart Exec does not name the binary: $(grep '^Exec=' "$A")"
grep -q '^X-GNOME-Autostart-enabled=true' "$A" || fail "autostart entry not enabled"
keep "$A"
stop_app "$PID"
pass "autostart entry written for $BIN"

"$BIN" --unregister > "$LAB/unregister.log" 2>&1 || fail "--unregister exited $?"
test ! -e "$A" || fail "--unregister left the autostart entry"
pass "--unregister removed it"
