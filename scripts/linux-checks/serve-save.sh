#!/bin/bash
# Headless mode (--no-tray) on a file in a trusted folder: the app must hand the
# browser a 127.0.0.1 URL, the wire must agree on the origin, the Host gate must
# refuse a foreign Host, and the Malleable HTML File conformance page must pass a
# real save round trip in a real browser from that origin.
source "$(dirname "$0")/lib.sh"
command -v node >/dev/null || fail "node is required (the conformance runner is a Playwright script)"

DOCS="$HOME/docs"; trust_folder "$DOCS"
PAGE="_mhf-host-test.htmlclay"
cp "$REPO/testdata/conformance/host-test.html" "$DOCS/$PAGE"

start_app --no-tray "$DOCS/$PAGE"; PID=$APP_PID
wait_for_file "$LAB/opened-urls" 30 || fail "the app never asked the browser to open a URL"
URL="$(head -1 "$LAB/opened-urls")"
ORIGIN="$(sed -E 's#^(https?://[^/]+).*#\1#' <<<"$URL")"; URLPATH="${URL#"$ORIGIN"}"
[[ "$ORIGIN" =~ ^http://127\.0\.0\.1:[0-9]+$ ]] || fail "unexpected URL $URL"
[ "$URLPATH" = "/docs/$PAGE" ] || fail "URL path is '$URLPATH', expected '/docs/$PAGE'"
pass "opened $URL"

where="$("$BIN" wire where "$DOCS/$PAGE")"
grep -qF "\"origin\":\"$ORIGIN\"" <<<"$where" || fail "wire where disagrees: $where"
pass "wire where reports the same origin"

port="${ORIGIN##*:}"
code="$(curl -s -o /dev/null -w '%{http_code}' -H "Host: attacker.example:$port" "$ORIGIN$URLPATH")"
[ "$code" = "403" ] || fail "foreign Host header got $code, expected 403"
code="$(curl -s -o /dev/null -w '%{http_code}' "$ORIGIN$URLPATH")"
[ "$code" = "200" ] || fail "own origin got $code, expected 200"
pass "Host gate: 403 foreign, 200 own"

( cd "$REPO" && node testdata/conformance/host-test.mjs --url "$ORIGIN" --page "${URLPATH#/}" --token-from-page ) > "$LAB/conformance.log" 2>&1 \
  || { keep "$LAB/conformance.log"; fail "conformance run failed: $(tail -20 "$LAB/conformance.log")"; }
keep "$LAB/conformance.log"
pass "conformance page passed from the app's own origin"

stop_app "$PID"; rc=$APP_RC
[ "$rc" = "0" ] || fail "exit code $rc on SIGTERM"
grep -q "panic" "$LAB/htmlclay.log" && fail "panic in log"
pass "clean exit on SIGTERM"
