//go:build !windows

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// canaryFakeApp is the htmlclay binary inside a fake HTMLClay.app. FAKE_MODE
// picks how the "running app" answers update-now; every call is logged, and
// any process it leaves behind is recorded so the test can stop it.
const canaryFakeApp = `#!/bin/bash
W=%q
echo "$*" >> "$W/calls"
n=$(wc -l < "$W/calls" | tr -d ' ')
spawn() {
  ( sleep "$1"
    sleep 60 & pid=$!
    echo "$pid" >> "$W/pids"
    [ "$2" = health ] && printf '{"schema":1,"txid":"t","version":"%%s","pid":%%d,"phase":"serving"}' "$3" "$pid" > "$W/cfg/updates/health.json"
    [ "$4" = bundle ] && plutil -replace CFBundleShortVersionString -string "$3" "$W/app/HTMLClay.app/Contents/Info.plist"
    echo "$pid" > "$W/cfg/lock"
  ) >/dev/null 2>&1 &
}
case "$FAKE_MODE" in
  self) echo accepted; [ "$n" = 1 ] && spawn 1 health "$3" bundle; exit 0 ;;
  already) echo "already $3"; exit 0 ;;
  down) exit 2 ;;
  down-rollback)
    printf '{"schema":1,"phase":"rolledBack","to":"%%s","failed":{"version":"%%s","reason":"never served"}}' "$3" "$3" > "$W/cfg/updates/state.json"
    exit 2 ;;
  refused) echo 'refused self-update is off: config sets "updates": "off"'; exit 3 ;;
  not-newer) echo "refused v$3 is not newer than the running v9.9.9"; exit 3 ;;
  old) echo "refused the running HTML Clay predates update-now"; exit 4 ;;
  cancelled) echo "failed the restart to v$3 was cancelled on screen"; exit 5 ;;
  held) echo "failed Sparkle holds v9.9.9 prepared, which installs at the next quit"; exit 5 ;;
  rollback)
    echo accepted
    printf '{"schema":1,"phase":"rolledBack","to":"%%s","failed":{"version":"%%s","reason":"never served"}}' "$3" "$3" > "$W/cfg/updates/state.json"
    exit 0 ;;
  stuck) echo accepted; exit 0 ;;
  no-bundle) echo accepted; [ "$n" = 1 ] && spawn 0 health "$3" keep; exit 0 ;;
  starting-first)
    if [ "$n" -le 2 ]; then echo starting; exit 6; fi
    echo accepted; [ "$n" = 3 ] && spawn 1 health "$3" bundle; exit 0 ;;
  starting-after)
    case "$n" in
      1) echo accepted; spawn 3 health "$3" bundle; exit 0 ;;
      2) echo starting; exit 6 ;;
      *) echo accepted; exit 0 ;;
    esac ;;
  starting) echo starting; exit 6 ;;
  new-pid)
    if [ "$n" = 1 ]; then echo accepted; spawn 0 none "$3" bundle; exit 0; fi
    echo "already $3"; exit 0 ;;
esac
`

const canaryFakeInstall = `#!/bin/bash
echo "install $1" >> %q
exit ${FAKE_INSTALL_CODE:-0}
`

const canaryPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleShortVersionString</key><string>%s</string>
%s</dict></plist>
`

type canaryCase struct {
	name       string
	env        []string
	version    string
	noKey      bool
	rolledBack bool
	oldHealth  bool
	code       int
	outcome    string
	reason     string
	retry      bool
	installs   int
	calls      int
	callsAtMin bool
}

func TestUpdateCanary(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("update-canary.sh runs on macOS only and reads plists with plutil")
	}
	script, err := filepath.Abs("../../scripts/update-canary.sh")
	if err != nil {
		t.Fatal(err)
	}
	cases := []canaryCase{
		{name: "self-update", env: []string{"FAKE_MODE=self", "HTMLCLAY_CANARY_DEADLINE=20"}, code: 0, outcome: "self-updated", calls: 1},
		{name: "already running", env: []string{"FAKE_MODE=already"}, code: 0, outcome: "self-updated", reason: "already running v1.14.0", calls: 1},
		{name: "build without the key", env: []string{"FAKE_MODE=self"}, noKey: true, code: 10, outcome: "installed", reason: "predates update-now", installs: 1},
		{name: "not running", env: []string{"FAKE_MODE=down"}, code: 10, outcome: "installed", reason: "not running", installs: 1, calls: 1},
		{name: "running build too old to answer", env: []string{"FAKE_MODE=old"}, code: 10, outcome: "installed", reason: "predates update-now", installs: 1, calls: 1},
		{name: "fallback install fails", env: []string{"FAKE_MODE=down", "FAKE_INSTALL_CODE=1"}, code: 1, outcome: "failed", reason: "install-local.sh failed", installs: 1, calls: 1},
		{name: "refused is a failure, not a fallback", env: []string{"FAKE_MODE=refused"}, code: 1, outcome: "failed", reason: `"updates": "off"`, calls: 1},
		{name: "not newer is a failure, not a downgrade", env: []string{"FAKE_MODE=not-newer"}, code: 1, outcome: "failed", reason: "is not newer than the running v9.9.9", calls: 1},
		{name: "cancelled on screen", env: []string{"FAKE_MODE=cancelled"}, code: 1, outcome: "failed", reason: "cancelled on screen; restart HTML Clay or use the tray to update", calls: 1},
		{name: "another version held", env: []string{"FAKE_MODE=held"}, code: 1, outcome: "failed", reason: "Sparkle holds v9.9.9", calls: 1},
		{name: "rolled back after the accept", env: []string{"FAKE_MODE=rollback"}, code: 1, outcome: "failed", reason: "rolled back: never served", calls: 1},
		{name: "rolled back, build without the key", env: []string{"FAKE_MODE=self"}, noKey: true, rolledBack: true, code: 1, outcome: "failed", reason: "rolled back: never served"},
		{name: "rolled back, then not running", env: []string{"FAKE_MODE=down-rollback"}, code: 1, outcome: "failed", reason: "rolled back: never served", calls: 1},
		{name: "accepted, never serving", env: []string{"FAKE_MODE=stuck", "HTMLCLAY_CANARY_DEADLINE=6", "HTMLCLAY_CANARY_RESEND=2"}, code: 1, outcome: "failed", reason: "still running untouched", retry: true, calls: 2, callsAtMin: true},
		{name: "bundle still old", env: []string{"FAKE_MODE=no-bundle", "HTMLCLAY_CANARY_DEADLINE=4"}, code: 1, outcome: "failed", reason: "was not serving it after", calls: 1, callsAtMin: true},
		{name: "health names the old pid", env: []string{"FAKE_MODE=stuck", "HTMLCLAY_CANARY_DEADLINE=4"}, oldHealth: true, code: 1, outcome: "failed", reason: "still running untouched", retry: true, calls: 1, callsAtMin: true},
		{name: "starting, then accepted", env: []string{"FAKE_MODE=starting-first", "HTMLCLAY_CANARY_DEADLINE=20"}, code: 0, outcome: "self-updated", calls: 3, callsAtMin: true},
		{name: "accepted, then starting", env: []string{"FAKE_MODE=starting-after", "HTMLCLAY_CANARY_DEADLINE=20", "HTMLCLAY_CANARY_RESEND=1"}, code: 0, outcome: "self-updated", calls: 2, callsAtMin: true},
		{name: "still starting at the deadline", env: []string{"FAKE_MODE=starting", "HTMLCLAY_CANARY_DEADLINE=6"}, code: 1, outcome: "failed", reason: "still starting", retry: true, calls: 2, callsAtMin: true},
		{name: "already, from a new pid without health", env: []string{"FAKE_MODE=new-pid", "HTMLCLAY_CANARY_DEADLINE=10", "HTMLCLAY_CANARY_RESEND=1"}, code: 0, outcome: "self-updated", calls: 2, callsAtMin: true},
		{name: "bad version", env: []string{"FAKE_MODE=self"}, version: "1.14", code: 1, outcome: "failed", reason: "not a version"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runCanaryCase(t, script, tc)
		})
	}
}

func runCanaryCase(t *testing.T, script string, tc canaryCase) {
	w, err := os.MkdirTemp("", "canary-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if data, err := os.ReadFile(filepath.Join(w, "pids")); err == nil {
			for _, f := range strings.Fields(string(data)) {
				if pid, err := strconv.Atoi(f); err == nil {
					syscall.Kill(pid, syscall.SIGKILL)
				}
			}
		}
		os.RemoveAll(w)
	})
	app := filepath.Join(w, "app", "HTMLClay.app")
	cfg := filepath.Join(w, "cfg")
	for _, d := range []string{filepath.Join(app, "Contents", "MacOS"), filepath.Join(cfg, "updates")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	from, key := "1.13.3", "<key>HCUpdateNowCommand</key><integer>1</integer>\n"
	if tc.noKey {
		key = ""
	}
	if tc.oldHealth {
		from = "1.14.0"
		health := fmt.Sprintf(`{"schema":1,"txid":"t","version":"1.14.0","pid":%d,"phase":"serving"}`, os.Getpid())
		writeCanaryFile(t, filepath.Join(cfg, "updates", "health.json"), health, 0o644)
	}
	if tc.rolledBack {
		writeCanaryFile(t, filepath.Join(cfg, "updates", "state.json"),
			`{"schema":1,"phase":"rolledBack","to":"1.14.0","failed":{"version":"1.14.0","reason":"never served"}}`, 0o644)
	}
	writeCanaryFile(t, filepath.Join(app, "Contents", "Info.plist"), fmt.Sprintf(canaryPlist, from, key), 0o644)
	writeCanaryFile(t, filepath.Join(cfg, "lock"), fmt.Sprintf("%d\n", os.Getpid()), 0o644)
	writeCanaryFile(t, filepath.Join(app, "Contents", "MacOS", "htmlclay"), fmt.Sprintf(canaryFakeApp, w), 0o755)
	install := filepath.Join(w, "install.sh")
	writeCanaryFile(t, install, fmt.Sprintf(canaryFakeInstall, filepath.Join(w, "installs")), 0o755)

	target := tc.version
	if target == "" {
		target = "1.14.0"
	}
	cmd := exec.Command("bash", script, target)
	cmd.Env = append(os.Environ(),
		"HTMLCLAY_CANARY_APP="+app,
		"HTMLCLAY_CANARY_CONFIG_DIR="+cfg,
		"HTMLCLAY_CANARY_INSTALL="+install,
		"HTMLCLAY_CANARY_POLL=1",
		"HTMLCLAY_CANARY_DEADLINE=10",
	)
	cmd.Env = append(cmd.Env, tc.env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatal(err)
		}
		code = exit.ExitCode()
	}

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	last := lines[len(lines)-1]
	defer func() {
		if t.Failed() {
			t.Logf("last line: %s\nstderr:\n%s", last, stderr.String())
		}
	}()
	if code != tc.code {
		t.Errorf("exit %d, want %d", code, tc.code)
	}
	var keys []string
	dec := json.NewDecoder(strings.NewReader(last))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("the last line is not a JSON object: %v", err)
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, tok.(string))
		var v any
		if err := dec.Decode(&v); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(keys, ","); got != "app,outcome,from,to,reason,seconds,retry" {
		t.Errorf("keys %s", got)
	}
	var o struct {
		App, Outcome, Reason string
		Seconds              *int
		Retry                *bool
	}
	if err := json.Unmarshal([]byte(last), &o); err != nil {
		t.Fatal(err)
	}
	if o.App != "htmlclay" || o.Outcome != tc.outcome || o.Seconds == nil || o.Retry == nil {
		t.Errorf("app %q outcome %q seconds %v retry %v; want htmlclay, %q", o.App, o.Outcome, o.Seconds, o.Retry, tc.outcome)
	}
	if !strings.Contains(o.Reason, tc.reason) {
		t.Errorf("reason %q does not contain %q", o.Reason, tc.reason)
	}
	if o.Retry != nil && *o.Retry != tc.retry {
		t.Errorf("retry %v, want %v", *o.Retry, tc.retry)
	}
	if got := countCanaryLines(filepath.Join(w, "installs")); got != tc.installs {
		t.Errorf("%d fallback installs, want %d", got, tc.installs)
	}
	calls := countCanaryLines(filepath.Join(w, "calls"))
	if tc.callsAtMin && calls < tc.calls || !tc.callsAtMin && calls != tc.calls {
		t.Errorf("%d update-now calls, want %d (at least: %v)", calls, tc.calls, tc.callsAtMin)
	}
}

func writeCanaryFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func countCanaryLines(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return strings.Count(string(data), "\n")
}

func TestInstallLocalDownloadIsBounded(t *testing.T) {
	data, err := os.ReadFile("../../scripts/install-local.sh")
	if err != nil {
		t.Fatal(err)
	}
	src := strings.ReplaceAll(string(data), "\\\n", " ")
	var curls []string
	for _, line := range strings.Split(src, "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] == "curl" {
			curls = append(curls, line)
		}
	}
	if len(curls) == 0 {
		t.Fatal("install-local.sh runs no curl")
	}
	for _, c := range curls {
		if !strings.Contains(c, "--connect-timeout 20") || !strings.Contains(c, "--max-time 600") {
			t.Errorf("curl without --connect-timeout 20 --max-time 600: %s", c)
		}
	}
}
