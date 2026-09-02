#!/bin/bash
# Tray mode where the desktop is missing, which is what a plain server or an SSH
# login is. The app must still serve, and quitting must not panic. Three variants:
# a user bus with no StatusNotifierWatcher on it (systemd --user without a desktop),
# no bus and no way to launch one, and a real dbus-launch on PATH, where the systray
# library's autolaunch must not leave a daemon behind.
source "$(dirname "$0")/lib.sh"
unset DBUS_SESSION_BUS_ADDRESS DISPLAY WAYLAND_DISPLAY
DOCS="$HOME/docs"; trust_folder "$DOCS"
echo '<!doctype html><html><body>x</body></html>' > "$DOCS/x.htmlclay"
REAL_RUNTIME_DIR="${XDG_RUNTIME_DIR:-}"
EMPTY_RUNTIME_DIR="$LAB/xdg-runtime"; mkdir -p "$EMPTY_RUNTIME_DIR"; chmod 700 "$EMPTY_RUNTIME_DIR"

tray_round() {
  local tag="$1"
  start_app "$DOCS/x.htmlclay"; PID=$APP_PID
  wait_for_file "$LAB/opened-urls" 30 || fail "[$tag] never served the file"
  sleep 2
  stop_app "$PID"; rc=$APP_RC
  cp "$LAB/htmlclay.log" "$LAB/htmlclay-$tag.log"; keep "$LAB/htmlclay-$tag.log"
  grep -q "panic" "$LAB/htmlclay.log" && fail "[$tag] panicked at quit: $(grep -A4 panic "$LAB/htmlclay.log" | head -6)"
  [ "$rc" = "0" ] || fail "[$tag] exit code $rc"
}

if [ -n "$REAL_RUNTIME_DIR" ] && [ -S "$REAL_RUNTIME_DIR/bus" ]; then
  export XDG_RUNTIME_DIR="$REAL_RUNTIME_DIR"
  tray_round "user-bus-no-watcher"
  grep -q "StatusNotifierWatcher" "$LAB/htmlclay.log" || fail "[user-bus-no-watcher] expected the systray registration to fail on a desktop-less bus"
  pass "user bus without a tray host: served, quit cleanly (exit 0)"
else
  pass "no systemd user bus here; that variant skipped"
fi

export XDG_RUNTIME_DIR="$EMPTY_RUNTIME_DIR"
printf '#!/bin/sh\nexit 1\n' > "$STUBS/dbus-launch"; chmod +x "$STUBS/dbus-launch"
tray_round "no-bus"
pass "no session bus and no dbus-launch: served, quit cleanly (exit 0)"

rm -f "$STUBS/dbus-launch"
if command -v dbus-launch >/dev/null; then
  before="$(pgrep -x dbus-daemon | wc -l)"
  tray_round "autolaunch"
  sleep 1
  after="$(pgrep -x dbus-daemon | wc -l)"
  [ "$after" -le "$before" ] || fail "[autolaunch] a stray dbus-daemon survived the app ($before -> $after)"
  pass "dbus-launch on PATH: served, quit cleanly, no stray dbus-daemon"
else
  pass "dbus-launch not installed here; autolaunch variant skipped"
fi
