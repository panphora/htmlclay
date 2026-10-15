package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/panphora/htmlclay/internal/sparkle"
	"github.com/panphora/htmlclay/internal/tray"
)

func TestSparkleGate(t *testing.T) {
	writable := func(string) error { return nil }
	readOnly := func(string) error { return errors.New("the folder holding it is not writable") }
	const app = "/Applications/HTMLClay.app"
	cases := []struct {
		name     string
		channel  string
		version  string
		key      string
		off      bool
		bundle   string
		inBundle bool
		writable func(string) error
		want     string
	}{
		{"release build in Applications", "stable", "1.14.0", "pk", false, app, true, writable, ""},
		{"local build", "", "1.14.0", "pk", false, app, true, writable, "no update channel"},
		{"dev version", "stable", "1.14.0-dev", "pk", false, app, true, writable, "not a release version"},
		{"bare binary", "stable", "1.14.0", "pk", false, "", false, writable, "not running from an app bundle"},
		{"no public key", "stable", "1.14.0", "", false, app, true, writable, "no SUPublicEDKey"},
		{"turned off in config", "stable", "1.14.0", "pk", true, app, true, writable, `"updates": "off"`},
		{"translocated", "stable", "1.14.0", "pk", false, "/private/var/folders/x/T/AppTranslocation/ABC/d/HTMLClay.app", true, writable, "translocated"},
		{"standard user in Applications", "stable", "1.14.0", "pk", false, app, true, readOnly, "not writable"},
	}
	for _, tc := range cases {
		err := sparkleGate(tc.channel, tc.version, tc.key, tc.off, tc.bundle, tc.inBundle, tc.writable)
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: gate refused: %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: gate gave %v, want an error containing %q", tc.name, err, tc.want)
		}
	}
}

func TestAppBundleFor(t *testing.T) {
	cases := []struct {
		exe    string
		bundle string
		ok     bool
	}{
		{"/Applications/HTMLClay.app/Contents/MacOS/htmlclay", "/Applications/HTMLClay.app", true},
		{"/Users/x/Apps/HTML Clay.app/Contents/MacOS/htmlclay", "/Users/x/Apps/HTML Clay.app", true},
		{"/Users/x/go/bin/htmlclay", "", false},
		{"/Applications/HTMLClay.app/Contents/Resources/htmlclay", "", false},
		{"/tmp/Contents/MacOS/htmlclay", "", false},
	}
	for _, tc := range cases {
		bundle, ok := appBundleFor(tc.exe)
		if bundle != tc.bundle || ok != tc.ok {
			t.Errorf("appBundleFor(%q) = %q, %v; want %q, %v", tc.exe, bundle, ok, tc.bundle, tc.ok)
		}
	}
}

// fakeSparkle stands in for the framework. onInstall runs when the app asks
// for the immediate install.
type fakeSparkle struct {
	mu        sync.Mutex
	installs  int
	checks    int
	onInstall func()
}

func (f *fakeSparkle) PublicKey() string         { return "pk" }
func (f *fakeSparkle) Writable(string) error     { return nil }
func (f *fakeSparkle) Load(string) error         { return nil }
func (f *fakeSparkle) Start(func(sparkle.Event)) {}
func (f *fakeSparkle) installCount() int         { f.mu.Lock(); defer f.mu.Unlock(); return f.installs }
func (f *fakeSparkle) checkCount() int           { f.mu.Lock(); defer f.mu.Unlock(); return f.checks }
func (f *fakeSparkle) CheckNow()                 { f.mu.Lock(); f.checks++; f.mu.Unlock() }
func (f *fakeSparkle) InstallNow() {
	f.mu.Lock()
	f.installs++
	hook := f.onInstall
	f.mu.Unlock()
	if hook != nil {
		go hook()
	}
}

// sparkleTestApp is updateTestApp with a fake framework and a recorder for the
// notice a failed restart shows.
func sparkleTestApp(t *testing.T) (*app, *fakeSparkle, chan string) {
	t.Helper()
	a, _ := updateTestApp(t)
	fw := &fakeSparkle{}
	a.rt.sparkle = &sparkleState{fw: fw}
	notices := make(chan string, 4)
	a.rt.notify = func(_, msg string) error { notices <- msg; return nil }
	return a, fw, notices
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSparkleReadyOffersTheRestartRow(t *testing.T) {
	a, _, _ := sparkleTestApp(t)
	updateCh := make(chan tray.UpdateInfo, 1)
	a.onSparkleEvent(sparkle.Event{Kind: sparkle.Ready, Text: "not-a-version"}, updateCh)
	if len(updateCh) != 0 {
		t.Fatal("a version that is not X.Y.Z was offered")
	}
	a.onSparkleEvent(sparkle.Event{Kind: sparkle.Ready, Text: "9.9.9"}, updateCh)
	info := <-updateCh
	if info.Version != "9.9.9" || info.Apply == nil || info.URL != "" {
		t.Fatalf("tray info %+v, want a Restart to Update row for 9.9.9", info)
	}
}

func TestSparkleStartFailureFallsBackToTheDownloadLink(t *testing.T) {
	a, _, _ := sparkleTestApp(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"latest":"99.0.0","url":"https://htmlclay.com/#downloads"}`))
	}))
	defer srv.Close()
	defer func(u string) { releaseInfoURL = u }(releaseInfoURL)
	releaseInfoURL = srv.URL

	updateCh := make(chan tray.UpdateInfo, 1)
	a.onSparkleEvent(sparkle.Event{Kind: sparkle.StartFailed, Text: "bad key"}, updateCh)
	info := <-updateCh
	if info.Version != "99.0.0" || info.Apply != nil || info.URL == "" {
		t.Fatalf("tray info %+v, want the download-page link", info)
	}
}

// The successful path up to Sparkle's quit event: the handoff is written for
// the new version before the install is requested, the app stays marked as
// updating, and nothing is torn down while the quit is on its way.
func TestSparkleRestartWritesTheHandoffThenInstalls(t *testing.T) {
	a, fw, notices := sparkleTestApp(t)
	handoff := filepath.Join(a.rt.configDir, updateHandoffFile)
	fw.onInstall = func() {
		data, err := os.ReadFile(handoff)
		if err != nil {
			t.Errorf("Sparkle was asked to install before the handoff existed: %v", err)
			return
		}
		var h updateHandoff
		if err := json.Unmarshal(data, &h); err != nil || h.TargetVersion != "9.9.9" || h.Port != a.rt.port {
			t.Errorf("handoff %s, %v", data, err)
		}
	}
	a.clickSparkleUpdate("9.9.9")
	waitFor(t, "the install request", func() bool { return fw.installCount() == 1 })
	if !a.updating.Load() {
		t.Fatal("a requested install must keep a second click from starting another")
	}
	a.mu.Lock()
	a.stopping = true
	a.mu.Unlock()
	a.rt.sparkle.fail("too late")
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(handoff); err != nil {
		t.Fatalf("the handoff was removed after the quit had started: %v", err)
	}
	if len(notices) != 0 {
		t.Fatalf("a restart under way showed a failure notice: %s", <-notices)
	}
}

func TestSparkleInstallFailureRemovesTheHandoff(t *testing.T) {
	a, fw, notices := sparkleTestApp(t)
	updateCh := make(chan tray.UpdateInfo, 1)
	fw.onInstall = func() {
		a.onSparkleEvent(sparkle.Event{Kind: sparkle.InstallFailed, Text: "Sparkle has no prepared update"}, updateCh)
	}
	a.clickSparkleUpdate("9.9.9")
	msg := <-notices
	if !strings.Contains(msg, "still running") {
		t.Fatalf("notice %q", msg)
	}
	if _, err := os.Stat(filepath.Join(a.rt.configDir, updateHandoffFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a failed install left a handoff with live tokens behind")
	}
	if a.updating.Load() {
		t.Fatal("a failed install left the app marked as updating")
	}

	// The row stays, so the person can click again.
	fw.onInstall = nil
	a.clickSparkleUpdate("9.9.9")
	waitFor(t, "the second install request", func() bool { return fw.installCount() == 2 })
	a.mu.Lock()
	a.stopping = true
	a.mu.Unlock()
	a.rt.sparkle.fail("test over")
}

// Sparkle not quitting the app in time is not a failure: it still holds the
// install request and may quit the app later to carry it out. Writes reopen so
// the running version keeps saving, but the handoff stays and the app stays
// marked as updating until Sparkle reports a definite failure.
func TestSparkleInstallThatNeverQuitsKeepsTheHandoff(t *testing.T) {
	a, _, notices := sparkleTestApp(t)
	defer func(d time.Duration) { sparkleInstallTimeout = d }(sparkleInstallTimeout)
	sparkleInstallTimeout = 50 * time.Millisecond
	handoff := filepath.Join(a.rt.configDir, updateHandoffFile)

	a.clickSparkleUpdate("9.9.9")
	msg := <-notices
	if !strings.Contains(msg, "9.9.9") || !strings.Contains(msg, "next time") {
		t.Fatalf("notice %q", msg)
	}
	if _, err := os.Stat(handoff); err != nil {
		t.Fatalf("a slow Sparkle lost the handoff: %v", err)
	}
	if !a.updating.Load() {
		t.Fatal("a pending Sparkle install was given up on")
	}
	if !a.admitWrite() {
		t.Fatal("writes stayed closed while Sparkle was slow")
	}
	a.rt.writesInFlight.Add(-1)

	a.rt.sparkle.fail("Sparkle has no prepared update")
	msg = <-notices
	if !strings.Contains(msg, "still running") {
		t.Fatalf("notice %q", msg)
	}
	if _, err := os.Stat(handoff); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a definite failure left the handoff behind")
	}
	if a.updating.Load() {
		t.Fatal("a definite failure left the app marked as updating")
	}
}

func TestSparkleRestartWaitsForWrites(t *testing.T) {
	a, fw, _ := sparkleTestApp(t)
	defer func(q, l time.Duration) { updateQuietFor, updateQuietLimit = q, l }(updateQuietFor, updateQuietLimit)
	updateQuietFor, updateQuietLimit = 20*time.Millisecond, 150*time.Millisecond
	a.rt.writesInFlight.Store(1)

	err := a.restartForUpdate("9.9.9", func(h string) error { return a.installWithSparkle("9.9.9", h) })
	if !errors.Is(err, errUpdateBusy) {
		t.Fatalf("restartForUpdate = %v, want errUpdateBusy", err)
	}
	if fw.installCount() != 0 {
		t.Fatal("Sparkle installed while a save was still in flight")
	}
	if _, err := os.Stat(filepath.Join(a.rt.configDir, updateHandoffFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a cancelled restart left a handoff behind")
	}
}

// An error from an ordinary background check, with no install pending, is
// only logged.
func TestSparkleAbortWithNoPendingInstallIsHarmless(t *testing.T) {
	a, _, notices := sparkleTestApp(t)
	a.onSparkleEvent(sparkle.Event{Kind: sparkle.Aborted, Text: "the appcast could not be read"}, make(chan tray.UpdateInfo, 1))
	if len(notices) != 0 || a.updating.Load() {
		t.Fatal("a background check error was treated as a failed restart")
	}
}
