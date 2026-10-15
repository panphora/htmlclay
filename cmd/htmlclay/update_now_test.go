package main

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/panphora/htmlclay/internal/platform"
	"github.com/panphora/htmlclay/internal/sparkle"
	"github.com/panphora/htmlclay/internal/tray"
	"github.com/panphora/htmlclay/internal/update"
)

// commandSI is a SingleInstance whose primary answers every command with
// answer, or fails with err.
type commandSI struct {
	answer string
	err    error
	sent   []string
}

func (c *commandSI) TryLock() (bool, error)        { return false, nil }
func (c *commandSI) SendFilePath(string) error     { return errors.New("not a file") }
func (c *commandSI) OnFileReceived(func(string))   {}
func (c *commandSI) OnCommand(func(string) string) {}
func (c *commandSI) Unlock() error                 { return nil }
func (c *commandSI) SendCommand(cmd string) (string, error) {
	c.sent = append(c.sent, cmd)
	return c.answer, c.err
}

func TestUpdateNowClientExitCodes(t *testing.T) {
	cases := []struct {
		name   string
		answer string
		err    error
		code   int
		out    string
	}{
		{"accepted", "accepted", nil, updateNowExitOK, "accepted\n"},
		{"already there", "already 9.9.9", nil, updateNowExitOK, "already 9.9.9\n"},
		{"already another version", "already 9.9.8", nil, updateNowExitRefused, "already 9.9.8\n"},
		{"refused", "refused self-update is off: x", nil, updateNowExitRefused, "refused self-update is off: x\n"},
		{"failed", "failed the restart was cancelled", nil, updateNowExitFailed, "failed the restart was cancelled\n"},
		{"starting", platform.CommandStarting, nil, updateNowExitStarting, "starting\n"},
		{"old app", "", platform.ErrNoAnswer, updateNowExitTooOld, "refused the running HTML Clay predates update-now\n"},
		{"not running", "", errors.New("dial unix: connection refused"), updateNowExitNotRunning, ""},
	}
	for _, tc := range cases {
		si := &commandSI{answer: tc.answer, err: tc.err}
		var out, errOut bytes.Buffer
		code := runUpdateNow(si, []string{"--version", "9.9.9"}, &out, &errOut)
		if code != tc.code || out.String() != tc.out {
			t.Errorf("%s: exit %d, stdout %q; want %d, %q", tc.name, code, out.String(), tc.code, tc.out)
		}
		if len(si.sent) != 1 || si.sent[0] != "update-now 9.9.9" {
			t.Errorf("%s: sent %q", tc.name, si.sent)
		}
	}
}

func TestUpdateNowClientRejectsBadArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"--version", "1.2"}, {"--version", "v1.2.3"}, {"--version", "1.2.3", "extra"}, {"1.2.3"}} {
		si := &commandSI{answer: "accepted"}
		var out, errOut bytes.Buffer
		if code := runUpdateNow(si, args, &out, &errOut); code != updateNowExitUsage || len(si.sent) != 0 {
			t.Errorf("args %q: exit %d, sent %q; want usage and nothing sent", args, code, si.sent)
		}
	}
}

func TestUpdateNowDispatchOnlyTakesItsOwnSubcommand(t *testing.T) {
	for _, argv := range [][]string{{"htmlclay"}, {"htmlclay", "/tmp/update-now"}, {"htmlclay", "wire"}} {
		if _, handled := updateNowDispatch(argv); handled {
			t.Errorf("updateNowDispatch handled %q", argv)
		}
	}
}

// updateNowTestApp is sparkleTestApp on macOS terms, whatever the host is.
func updateNowTestApp(t *testing.T) (*app, *fakeSparkle, chan string) {
	t.Helper()
	orig := updateNowOS
	t.Cleanup(func() { updateNowOS = orig })
	updateNowOS = "darwin"
	return sparkleTestApp(t)
}

func endPendingInstall(a *app) {
	a.mu.Lock()
	a.stopping = true
	a.mu.Unlock()
	a.rt.sparkle.fail("test over")
}

func TestUpdateNowRefusesWithAReason(t *testing.T) {
	a, fw, _ := updateNowTestApp(t)
	cases := []struct {
		target string
		want   string
	}{
		{"1.2", `refused "1.2" is not X.Y.Z`},
		{version, "already " + version},
		{"0.0.1", "refused v0.0.1 is not newer than the running v" + version},
	}
	for _, tc := range cases {
		if got := a.updateNow(tc.target); got != tc.want {
			t.Errorf("updateNow(%q) = %q, want %q", tc.target, got, tc.want)
		}
	}

	a.updateOff = `config sets "updates": "off"`
	if got := a.updateNow("9.9.9"); got != `refused self-update is off: config sets "updates": "off"` {
		t.Errorf("with self-update off: %q", got)
	}
	a.updateOff = ""

	updateNowOS = "windows"
	if got := a.updateNow("9.9.9"); !strings.HasPrefix(got, "refused update-now runs on macOS only") {
		t.Errorf("on Windows: %q", got)
	}
	updateNowOS = "darwin"

	if fw.checkCount() != 0 || fw.installCount() != 0 {
		t.Fatal("a refused request reached Sparkle")
	}
}

func TestUpdateNowRefusesAfterSparkleFailedToStart(t *testing.T) {
	a, fw, _ := updateNowTestApp(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"latest":"1.0.0","url":"https://htmlclay.com/#downloads"}`))
	}))
	defer srv.Close()
	defer func(u string) { releaseInfoURL = u }(releaseInfoURL)
	releaseInfoURL = srv.URL

	a.onSparkleEvent(sparkle.Event{Kind: sparkle.StartFailed, Text: "bad key"}, make(chan tray.UpdateInfo, 1))
	if got := a.updateNow("9.9.9"); got != "refused Sparkle could not start: bad key" {
		t.Fatalf("updateNow = %q", got)
	}
	if fw.checkCount() != 0 {
		t.Fatal("a request reached a Sparkle that never started")
	}
}

func TestUpdateNowRefusesAVersionThatFailedBefore(t *testing.T) {
	a, fw, _ := updateNowTestApp(t)
	dir := update.UpdatesDir(a.rt.configDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	st := `{"schema":1,"phase":"rolledBack","from":"1.0.0","to":"9.9.9","failed":{"version":"9.9.9","reason":"never served"}}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(st), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := a.updateNow("9.9.9"); !strings.HasPrefix(got, "refused v9.9.9 did not start after an earlier update") {
		t.Fatalf("updateNow = %q", got)
	}
	if fw.checkCount() != 0 {
		t.Fatal("a version that rolled back was checked for again")
	}
}

func TestUpdateNowClicksAnUpdateSparkleAlreadyPrepared(t *testing.T) {
	a, fw, notices := updateNowTestApp(t)
	a.rt.sparkle.prepared = "9.9.9"
	if got := a.updateNow("9.9.9"); got != "accepted" {
		t.Fatalf("updateNow = %q", got)
	}
	waitFor(t, "the install request", func() bool { return fw.installCount() == 1 })
	if _, err := os.Stat(filepath.Join(a.rt.configDir, updateHandoffFile)); err != nil {
		t.Fatalf("update-now skipped the tray click's handoff: %v", err)
	}
	if got := a.updateNow("9.9.9"); got != "accepted" {
		t.Fatalf("resent updateNow = %q", got)
	}
	time.Sleep(50 * time.Millisecond)
	if fw.installCount() != 1 || fw.checkCount() != 0 {
		t.Fatalf("a resent request installed %d times and checked %d times", fw.installCount(), fw.checkCount())
	}
	if len(notices) != 0 {
		t.Fatalf("a resent request started a second restart: %s", <-notices)
	}
	endPendingInstall(a)
}

func TestUpdateNowChecksAndInstallsOnlyTheVersionAskedFor(t *testing.T) {
	a, fw, _ := updateNowTestApp(t)
	updateCh := make(chan tray.UpdateInfo, 4)
	if got := a.updateNow("9.9.9"); got != "accepted" {
		t.Fatalf("updateNow = %q", got)
	}
	if fw.checkCount() != 1 || fw.installCount() != 0 {
		t.Fatalf("checks %d, installs %d; want one check and no install", fw.checkCount(), fw.installCount())
	}
	if got := a.updateNow("9.9.9"); got != "accepted" || fw.checkCount() != 2 {
		t.Fatalf("a resent request gave %q and %d checks; want a second check", got, fw.checkCount())
	}

	a.onSparkleEvent(sparkle.Event{Kind: sparkle.Ready, Text: "9.9.8"}, updateCh)
	a.rt.sparkle.mu.Lock()
	clicking, requested := a.rt.sparkle.clicking, a.rt.sparkle.requested
	a.rt.sparkle.prepared = ""
	a.rt.sparkle.mu.Unlock()
	if clicking || requested != "9.9.9" {
		t.Fatal("update-now clicked for a version it did not ask for")
	}

	a.onSparkleEvent(sparkle.Event{Kind: sparkle.Ready, Text: "9.9.9"}, updateCh)
	waitFor(t, "the install request", func() bool { return fw.installCount() == 1 })
	if len(updateCh) != 2 {
		t.Fatalf("%d tray rows offered, want one per Ready", len(updateCh))
	}
	endPendingInstall(a)
}

func TestUpdateNowRequestExpires(t *testing.T) {
	a, fw, _ := updateNowTestApp(t)
	defer func(d time.Duration) { updateNowWindow = d }(updateNowWindow)
	updateNowWindow = 10 * time.Millisecond
	a.updateNow("9.9.9")
	time.Sleep(30 * time.Millisecond)
	a.onSparkleEvent(sparkle.Event{Kind: sparkle.Ready, Text: "9.9.9"}, make(chan tray.UpdateInfo, 1))
	a.rt.sparkle.mu.Lock()
	clicking := a.rt.sparkle.clicking
	a.rt.sparkle.mu.Unlock()
	if clicking || fw.installCount() != 0 {
		t.Fatal("a stale update-now request clicked when a scheduled check prepared the version")
	}
}

func TestUpdateNowRequestLivesAMinuteFromTheLastResend(t *testing.T) {
	a, fw, _ := updateNowTestApp(t)
	updateCh := make(chan tray.UpdateInfo, 4)
	age := func(d time.Duration) {
		a.rt.sparkle.mu.Lock()
		a.rt.sparkle.requestedAt = time.Now().Add(-d)
		a.rt.sparkle.mu.Unlock()
	}

	a.updateNow("9.9.9")
	age(61 * time.Second)
	a.onSparkleEvent(sparkle.Event{Kind: sparkle.Ready, Text: "9.9.9"}, updateCh)
	a.rt.sparkle.mu.Lock()
	clicking, requested := a.rt.sparkle.clicking, a.rt.sparkle.requested
	a.rt.sparkle.prepared = ""
	a.rt.sparkle.mu.Unlock()
	if clicking || requested != "" || fw.installCount() != 0 {
		t.Fatalf("a request last sent 61 s ago clicked %v and is still recorded as %q", clicking, requested)
	}

	a.updateNow("9.9.9")
	age(50 * time.Second)
	a.updateNow("9.9.9")
	age(50 * time.Second)
	a.onSparkleEvent(sparkle.Event{Kind: sparkle.Ready, Text: "9.9.9"}, updateCh)
	waitFor(t, "the install request", func() bool { return fw.installCount() == 1 })
	endPendingInstall(a)
}

func TestUpdateNowFailsWhileAnotherVersionIsPrepared(t *testing.T) {
	a, fw, _ := updateNowTestApp(t)
	a.rt.sparkle.prepared = "9.9.8"
	if got := a.updateNow("9.9.9"); got != "failed Sparkle holds v9.9.8 prepared, which installs at the next quit" {
		t.Fatalf("updateNow = %q", got)
	}
	if fw.checkCount() != 0 || fw.installCount() != 0 {
		t.Fatal("a refused request reached Sparkle")
	}
}

func TestUpdateNowAfterACancelledRestartFails(t *testing.T) {
	a, fw, _ := updateNowTestApp(t)
	a.rt.sparkle.prepared = "9.9.9"
	a.rt.sparkle.declined = "9.9.9"
	if got := a.updateNow("9.9.9"); got != "failed the restart to v9.9.9 was cancelled on screen" {
		t.Fatalf("updateNow = %q", got)
	}
	time.Sleep(50 * time.Millisecond)
	if fw.installCount() != 0 {
		t.Fatal("a restart the person cancelled was started again")
	}
}

func TestUpdateNowCommandParsing(t *testing.T) {
	a, _, _ := updateNowTestApp(t)
	for _, cmd := range []string{"", "update-now", "update-now 9.9.9 extra", "install 9.9.9"} {
		if got := a.handleInstanceCommand(cmd); got != "refused unknown command" {
			t.Errorf("handleInstanceCommand(%q) = %q", cmd, got)
		}
	}
	if got := a.handleInstanceCommand("update-now " + version); got != "already "+version {
		t.Errorf("handleInstanceCommand = %q", got)
	}
}
