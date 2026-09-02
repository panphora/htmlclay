#!/bin/bash
# Run every Linux check, keep going after a failure, and write a summary. Exits
# non-zero if any check failed. Artifacts land in linux-check-artifacts/.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
export HTMLCLAY_CHECK_OUT="${HTMLCLAY_CHECK_OUT:-$REPO/linux-check-artifacts}"
mkdir -p "$HTMLCLAY_CHECK_OUT"
SUMMARY="$HTMLCLAY_CHECK_OUT/summary.txt"; : > "$SUMMARY"
{
  echo "=== environment"; cat /etc/os-release | head -2; uname -m
  echo "XDG_SESSION_TYPE=${XDG_SESSION_TYPE:-} DISPLAY=${DISPLAY:-} WAYLAND_DISPLAY=${WAYLAND_DISPLAY:-} user=$(id -un)"
  for t in zenity kdialog xdg-open notify-send xdg-mime update-mime-database desktop-file-validate gio dbus-launch Xvfb xdotool node; do
    printf '%-24s %s\n' "$t" "$(command -v $t || echo MISSING)"
  done
} > "$HTMLCLAY_CHECK_OUT/environment.txt"
failed=0
for check in ${CHECKS:-install-uninstall serve-save tray-nobus login-item prompts}; do
  if bash "$HERE/$check.sh" > "$HTMLCLAY_CHECK_OUT/$check.out" 2>&1; then
    echo "PASS $check" | tee -a "$SUMMARY"
  else
    echo "FAIL $check" | tee -a "$SUMMARY"; failed=1
    sed 's/^/    /' "$HTMLCLAY_CHECK_OUT/$check.out" | tail -30
  fi
done
exit $failed
