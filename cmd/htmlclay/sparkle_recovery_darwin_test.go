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
