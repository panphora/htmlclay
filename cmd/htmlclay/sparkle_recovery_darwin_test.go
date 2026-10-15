//go:build darwin

package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/panphora/htmlclay/internal/sparkle"
	"github.com/panphora/htmlclay/internal/tray"
	"github.com/panphora/htmlclay/internal/update"
)

// recoveryTestApp is sparkleTestApp with an installed bundle whose binary is a
// script that records how it was started, so a test can see the supervisor
// start from the copy without running a real one.
func recoveryTestApp(t *testing.T) (*app, *fakeSparkle, chan string, string) {
	t.Helper()
	a, fw, notices := sparkleTestApp(t)
	bundle := filepath.Join(t.TempDir(), "HTMLClay.app")
	exe := filepath.Join(bundle, "Contents", "MacOS", "htmlclay")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho \"$0 $*\" > \"$HTMLCLAY_TEST_SUPERVISOR\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "supervisor")
	t.Setenv("HTMLCLAY_TEST_SUPERVISOR", marker)
	a.rt.sparkle.bundle, a.rt.sparkle.exe = bundle, exe
	return a, fw, notices, marker
}

func TestSparkleRestartStartsRecoveryFromACopyFirst(t *testing.T) {
	a, fw, notices, marker := recoveryTestApp(t)
	dir := update.UpdatesDir(a.rt.configDir)
	seen := make(chan *update.State, 1)
	fw.onInstall = func() {
		st, _ := update.ReadState(dir)
		seen <- st
	}
	a.clickSparkleUpdate("9.9.9")
	st := <-seen
	if st == nil || st.Phase != update.PhaseApplying || st.Installer != update.InstallerSparkle || st.To != "9.9.9" || st.From != version || st.OldPID != os.Getpid() {
		t.Fatalf("state when Sparkle was asked to install: %+v", st)
	}
	if st.InstallPath != a.rt.sparkle.bundle || st.HandoffPath != filepath.Join(a.rt.configDir, updateHandoffFile) {
		t.Fatalf("state paths %+v", st)
	}
	waitFor(t, "the supervisor", func() bool { _, err := os.Stat(marker); return err == nil })
	got, _ := os.ReadFile(marker)
	want := filepath.Join(st.PreviousPath, "Contents", "MacOS", "htmlclay") + " --finish-update"
	if strings.TrimSpace(string(got)) != want {
		t.Fatalf("supervisor started as %q, want %q", got, want)
	}
	a.mu.Lock()
	a.stopping = true
	a.mu.Unlock()
	a.rt.sparkle.fail("test over")
	if len(notices) != 0 {
		t.Fatalf("notice %s", <-notices)
	}
}

// When Sparkle never quits the app, the recovery state goes with the handoff,
// so the supervisor waiting on this process stands down.
func TestSparkleInstallFailureClearsTheRecoveryState(t *testing.T) {
	a, fw, notices, _ := recoveryTestApp(t)
	fw.onInstall = func() {
		a.onSparkleEvent(sparkle.Event{Kind: sparkle.InstallFailed, Text: "Sparkle has no prepared update"}, make(chan tray.UpdateInfo, 1))
	}
	a.clickSparkleUpdate("9.9.9")
	<-notices
	if st, _ := update.ReadState(update.UpdatesDir(a.rt.configDir)); st != nil {
		t.Fatalf("recovery state %+v left behind after a failed install", st)
	}
}

func TestSparkleSkipsAVersionThatWasRolledBack(t *testing.T) {
	a, _, _ := sparkleTestApp(t)
	if r := a.failedSparkleUpdate("9.9.9"); r != "" {
		t.Fatalf("no state, skip reason %q", r)
	}
	dir := update.UpdatesDir(a.rt.configDir)
	os.MkdirAll(dir, 0o700)
	data, _ := json.Marshal(update.State{Schema: update.StateSchema, Phase: update.PhaseRolledBack, From: "1.0.0", To: "9.9.9", Installer: update.InstallerSparkle,
		Failed: &update.Failure{Version: "9.9.9", Reason: "9.9.9 exited before it reported serving", Time: time.Now()}})
	if err := os.WriteFile(filepath.Join(dir, "state.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if r := a.failedSparkleUpdate("9.9.9"); !strings.Contains(r, "exited before it reported serving") {
		t.Fatalf("skip reason %q", r)
	}
	if r := a.failedSparkleUpdate("9.9.10"); r != "" {
		t.Fatalf("a newer version was skipped: %q", r)
	}
}

// Without a known bundle (a test app, or a gate that never ran) the install
// goes ahead with no recovery and no state.
func TestSparkleRestartWithoutABundleSkipsRecovery(t *testing.T) {
	a, fw, _ := sparkleTestApp(t)
	a.clickSparkleUpdate("9.9.9")
	waitFor(t, "the install request", func() bool { return fw.installCount() == 1 })
	if _, err := os.Stat(filepath.Join(update.UpdatesDir(a.rt.configDir), "state.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("recovery state written with no bundle")
	}
	a.mu.Lock()
	a.stopping = true
	a.mu.Unlock()
	a.rt.sparkle.fail("test over")
}

func waitForSupervisor(t *testing.T, marker string) string {
	t.Helper()
	waitFor(t, "the supervisor", func() bool { _, err := os.Stat(marker); return err == nil })
	got, _ := os.ReadFile(marker)
	return strings.TrimSpace(string(got))
}

// Sparkle installs a prepared update on any quit. A plain quit therefore starts
// a recovery for it too, marked as a quit install and with no handoff, since an
// ordinary quit hands no session on.
func TestPlainQuitWithAPreparedUpdateStartsRecovery(t *testing.T) {
	a, _, _, marker := recoveryTestApp(t)
	a.onSparkleEvent(sparkle.Event{Kind: sparkle.Ready, Text: "9.9.9"}, make(chan tray.UpdateInfo, 1))
	a.prepareSparkleQuit()
	st, _ := update.ReadState(update.UpdatesDir(a.rt.configDir))
	if st == nil || st.Phase != update.PhaseApplying || st.To != "9.9.9" || !st.Quit || st.HandoffPath != "" || st.OldPID != os.Getpid() {
		t.Fatalf("state at a plain quit: %+v", st)
	}
	if got := waitForSupervisor(t, marker); !strings.HasSuffix(got, " --finish-update") {
		t.Fatalf("supervisor started as %q", got)
	}
	if _, err := os.Stat(filepath.Join(a.rt.configDir, updateHandoffFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a plain quit wrote a session handoff")
	}
}

func TestPlainQuitWithNothingPreparedStartsNothing(t *testing.T) {
	a, _, _, marker := recoveryTestApp(t)
	a.prepareSparkleQuit()
	if st, _ := update.ReadState(update.UpdatesDir(a.rt.configDir)); st != nil {
		t.Fatalf("state %+v written with no prepared update", st)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a supervisor started with no prepared update")
	}
}

// Sparkle quitting the app long after the click: the click's handoff and
// recovery would both have expired by the time the new version starts, so the
// quit writes a new handoff and starts a new recovery for the same install.
func TestLateSparkleQuitRenewsTheHandoffAndRecovery(t *testing.T) {
	a, _, notices, marker := recoveryTestApp(t)
	defer func(d, f time.Duration) { sparkleInstallTimeout, sparkleRecoveryFresh = d, f }(sparkleInstallTimeout, sparkleRecoveryFresh)
	sparkleInstallTimeout, sparkleRecoveryFresh = 50*time.Millisecond, 0
	dir := update.UpdatesDir(a.rt.configDir)
	handoff := filepath.Join(a.rt.configDir, updateHandoffFile)

	a.onSparkleEvent(sparkle.Event{Kind: sparkle.Ready, Text: "9.9.9"}, make(chan tray.UpdateInfo, 1))
	a.clickSparkleUpdate("9.9.9")
	<-notices
	first, _ := update.ReadState(dir)
	if first == nil {
		t.Fatal("the click started no recovery")
	}
	if _, err := os.Stat(handoff); err != nil {
		t.Fatalf("the click's handoff is gone after the timeout: %v", err)
	}
	waitForSupervisor(t, marker)
	os.Remove(marker)
	backdateHandoff(t, handoff, 50*time.Second)

	a.prepareSparkleQuit()
	second, _ := update.ReadState(dir)
	if second == nil || second.Txid == first.Txid || second.Quit || second.HandoffPath != handoff || second.To != "9.9.9" {
		t.Fatalf("state after the late quit %+v, want a new recovery for the click's install", second)
	}
	data, err := os.ReadFile(handoff)
	if err != nil {
		t.Fatal(err)
	}
	var h updateHandoff
	if err := json.Unmarshal(data, &h); err != nil || time.Since(h.Created) > 10*time.Second || h.TargetVersion != "9.9.9" {
		t.Fatalf("handoff after the late quit %s (%v), want a fresh one", data, err)
	}
	waitForSupervisor(t, marker)
	a.mu.Lock()
	a.stopping = true
	a.mu.Unlock()
	a.rt.sparkle.fail("test over")
}
