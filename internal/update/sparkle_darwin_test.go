//go:build darwin

package update

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const bundleExe = "Contents/MacOS/htmlclay"

// writeBundle builds a stand-in .app at path: an Info.plist carrying version
// and the helper built with version and mode as its binary.
func writeBundle(t *testing.T, path, version, mode string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(path, "Contents", "MacOS"), 0o755); err != nil {
		t.Fatal(err)
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>CFBundleExecutable</key>
    <string>htmlclay</string>
    <key>CFBundleVersion</key>
    <string>%s</string>
</dict>
</plist>
`, version)
	if err := os.WriteFile(filepath.Join(path, "Contents", "Info.plist"), []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
	copyFile(t, helper(t, version, mode), filepath.Join(path, bundleExe))
}

// sparkleFixture is HTML Clay 1.0.0 installed as a bundle, with 2.0.0 in
// newMode prepared beside it the way Sparkle stages an update.
type sparkleFixture struct {
	bundle, incoming, updates, markers, handoff string
}

func newSparkleFixture(t *testing.T, newMode string) sparkleFixture {
	t.Helper()
	root := t.TempDir()
	f := sparkleFixture{
		bundle:   filepath.Join(root, "Applications", "HTMLClay.app"),
		incoming: filepath.Join(root, "Applications", ".sparkle-staged", "HTMLClay.app"),
		updates:  filepath.Join(root, "config", "updates"),
		markers:  filepath.Join(root, "markers"),
		handoff:  filepath.Join(root, "config", "update-handoff.json"),
	}
	for _, d := range []string{f.updates, f.markers} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeBundle(t, f.bundle, "1.0.0", "idle")
	writeBundle(t, f.incoming, "2.0.0", newMode)
	if err := os.WriteFile(f.handoff, []byte(keptHandoff), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HTMLCLAY_TEST_UPDATES_DIR", f.updates)
	t.Setenv("HTMLCLAY_TEST_MARKERS", f.markers)
	t.Setenv("HTMLCLAY_TEST_HANDOFF", f.handoff)
	return f
}

func (f sparkleFixture) params(oldPID int) SparkleParams {
	return SparkleParams{From: "1.0.0", To: "2.0.0", Bundle: f.bundle, Executable: filepath.Join(f.bundle, bundleExe), OldPID: oldPID, HandoffPath: f.handoff}
}

// fakeSparkle does what Sparkle's installer does once oldPID has exited: swap
// the staged bundle in, delete the old one and launch the new version. The
// returned function waits for the launched process.
func (f sparkleFixture) fakeSparkle(t *testing.T, oldPID int) func() {
	t.Helper()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		waitGone(oldPID, 10*time.Second)
		time.Sleep(200 * time.Millisecond)
		if err := unix.RenamexNp(f.incoming, f.bundle, unix.RENAME_SWAP); err != nil {
			t.Errorf("fake Sparkle could not swap the bundle: %v", err)
			return
		}
		os.RemoveAll(f.incoming)
		cmd := exec.Command(filepath.Join(f.bundle, bundleExe))
		if err := cmd.Start(); err != nil {
			t.Errorf("fake Sparkle could not relaunch: %v", err)
			return
		}
		cmd.Wait()
	}()
	return wg.Wait
}

// relaunchDirect stands in for `open -n`: it runs the bundle's binary and
// records the call.
func relaunchDirect(calls *[]string) func(string) error {
	return func(bundle string) error {
		*calls = append(*calls, bundle)
		cmd := exec.Command(filepath.Join(bundle, bundleExe))
		if err := cmd.Start(); err != nil {
			return err
		}
		go cmd.Wait()
		return nil
	}
}

func bundleHolds(t *testing.T, bundle, version, mode string) {
	t.Helper()
	if v, err := BundleVersion(bundle); err != nil || v != version {
		t.Fatalf("bundle version %q (%v), want %s", v, err, version)
	}
	if !sameContent(t, filepath.Join(bundle, bundleExe), helper(t, version, mode)) {
		t.Fatalf("the bundle's binary is not %s-%s", version, mode)
	}
}

func supervisorLogOf(dir string) string {
	b, _ := os.ReadFile(filepath.Join(dir, supervisorLog))
	return string(b)
}

// The whole path the way production runs it: BeginSparkle copies the bundle
// and starts the supervisor from the copy, detached; fake Sparkle installs and
// relaunches; the supervisor sees 2.0.0 serving and finishes without touching
// the bundle.
func TestSparkleHealthyUpdateFinishes(t *testing.T) {
	f := newSparkleFixture(t, "serve")
	oldPID := exitedPID(t, 300*time.Millisecond)
	st, err := BeginSparkle(f.updates, f.params(oldPID))
	if err != nil {
		t.Fatal(err)
	}
	if st.Installer != InstallerSparkle || st.InstallPath != f.bundle || st.Executable != bundleExe || !strings.HasPrefix(st.PreviousPath, filepath.Join(f.updates, "previous")) {
		t.Fatalf("state %+v", st)
	}
	bundleHolds(t, st.PreviousPath, "1.0.0", "idle")
	wait := f.fakeSparkle(t, oldPID)

	done := waitForPhase(t, f.updates, 30*time.Second, PhaseDone, PhaseRolledBack, PhaseRollbackFailed)
	wait()
	if done.Phase != PhaseDone || done.Txid != st.Txid || done.Failed != nil {
		t.Fatalf("state %+v, want done\n%s", done, supervisorLogOf(f.updates))
	}
	bundleHolds(t, f.bundle, "2.0.0", "serve")
	if _, err := os.Stat(filepath.Dir(st.PreviousPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the copy of the previous bundle was left behind after a good update")
	}
	if _, err := os.Stat(filepath.Join(f.markers, "launched-1.0.0")); err == nil {
		t.Fatal("the old version was relaunched after a successful update")
	}
}

func TestSparkleCrashingUpdateRestoresThePreviousBundle(t *testing.T) {
	f := newSparkleFixture(t, "crash")
	oldPID := exitedPID(t, 100*time.Millisecond)
	st, err := prepareSparkle(f.updates, f.params(oldPID))
	if err != nil {
		t.Fatal(err)
	}
	wait := f.fakeSparkle(t, oldPID)
	var relaunched []string
	before := time.Now().UTC().Add(-time.Second)
	err = FinishUpdate(f.updates, SupervisorOptions{ServingTimeout: 2 * time.Second, InstallTimeout: 10 * time.Second, Relaunch: relaunchDirect(&relaunched)})
	wait()
	if err == nil {
		t.Fatal("a crashing update finished")
	}
	assertSparkleRolledBack(t, f, st, "did not report serving", relaunched)
	assertRetargeted(t, f.handoff, "1.0.0", before)
}

func TestSparkleHangingUpdateIsKilledAndRolledBack(t *testing.T) {
	f := newSparkleFixture(t, "hang")
	oldPID := exitedPID(t, 100*time.Millisecond)
	st, err := prepareSparkle(f.updates, f.params(oldPID))
	if err != nil {
		t.Fatal(err)
	}
	wait := f.fakeSparkle(t, oldPID)
	var relaunched []string
	err = FinishUpdate(f.updates, SupervisorOptions{ServingTimeout: 2 * time.Second, InstallTimeout: 10 * time.Second, Relaunch: relaunchDirect(&relaunched)})
	if err == nil {
		t.Fatal("an update that never served finished")
	}
	finished := make(chan struct{})
	go func() { wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the hanging new version is still running after the rollback")
	}
	assertSparkleRolledBack(t, f, st, "did not report serving", relaunched)
}

func assertSparkleRolledBack(t *testing.T, f sparkleFixture, st *State, reason string, relaunched []string) {
	t.Helper()
	got, err := ReadState(f.updates)
	if err != nil || got == nil {
		t.Fatalf("state %v %v", got, err)
	}
	if got.Phase != PhaseRolledBack || got.Failed == nil || got.Failed.Version != "2.0.0" || !strings.Contains(got.Failed.Reason, reason) {
		t.Fatalf("state %+v failed %+v, want rolledBack for 2.0.0 (%s)\n%s", got, got.Failed, reason, supervisorLogOf(f.updates))
	}
	bundleHolds(t, f.bundle, "1.0.0", "idle")
	if _, err := os.Stat(filepath.Dir(st.PreviousPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the failed bundle swapped out of the install path was left behind")
	}
	if len(relaunched) != 1 || relaunched[0] != f.bundle {
		t.Fatalf("relaunched %v, want the restored bundle once", relaunched)
	}
	waitForFile(t, filepath.Join(f.markers, "launched-1.0.0"), 5*time.Second)
	if pids := runningCopies(filepath.Join(f.bundle, bundleExe)); len(pids) > 0 {
		for _, pid := range pids {
			killPID(pid)
		}
	}
}

// Sparkle quit the app but never installed: the bundle still holds 1.0.0, so
// there is nothing to swap. The supervisor clears the state, hands the sessions
// back and starts 1.0.0 again, since nothing else will.
func TestSparkleInstallThatNeverHappensLeavesTheBundle(t *testing.T) {
	f := newSparkleFixture(t, "serve")
	oldPID := exitedPID(t, 100*time.Millisecond)
	st, err := prepareSparkle(f.updates, f.params(oldPID))
	if err != nil {
		t.Fatal(err)
	}
	var relaunched []string
	before := time.Now().UTC().Add(-time.Second)
	err = FinishUpdate(f.updates, SupervisorOptions{InstallTimeout: time.Second, Relaunch: relaunchDirect(&relaunched)})
	if err == nil || !strings.Contains(err.Error(), "did not install") {
		t.Fatalf("FinishUpdate = %v, want a missing install", err)
	}
	if got, _ := ReadState(f.updates); got != nil {
		t.Fatalf("state %+v left behind", got)
	}
	bundleHolds(t, f.bundle, "1.0.0", "idle")
	if _, err := os.Stat(filepath.Dir(st.PreviousPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the copy was left behind")
	}
	if !strings.Contains(supervisorLogOf(f.updates), "Sparkle did not install 2.0.0") {
		t.Fatalf("log:\n%s", supervisorLogOf(f.updates))
	}
	if len(relaunched) != 1 || relaunched[0] != f.bundle {
		t.Fatalf("relaunched %v, want the untouched bundle once", relaunched)
	}
	assertRetargeted(t, f.handoff, "1.0.0", before)
}

// The app gave up on Sparkle's quit and cleared the state, then the person
// clicked again: the first supervisor must leave the second update alone.
func TestSparkleSupervisorLeavesAReplacedUpdate(t *testing.T) {
	f := newSparkleFixture(t, "serve")
	oldPID := exitedPID(t, 300*time.Millisecond)
	first, err := prepareSparkle(f.updates, f.params(oldPID))
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- FinishUpdate(f.updates, SupervisorOptions{InstallTimeout: time.Second}) }()
	time.Sleep(100 * time.Millisecond)
	ClearState(f.updates, first.Txid)
	second, err := prepareSparkle(f.updates, f.params(oldPID))
	if err != nil {
		t.Fatal(err)
	}
	if err := <-result; err == nil || !strings.Contains(err.Error(), "no longer current") {
		t.Fatalf("first supervisor = %v", err)
	}
	got, _ := ReadState(f.updates)
	if got == nil || got.Txid != second.Txid || got.Phase != PhaseApplying {
		t.Fatalf("state %+v, want the second update untouched", got)
	}
	bundleHolds(t, second.PreviousPath, "1.0.0", "idle")
}

func TestClearStateOnlyRemovesItsOwnUpdate(t *testing.T) {
	dir := t.TempDir()
	writeJSONAtomic(filepath.Join(dir, stateFile), &State{Schema: StateSchema, Phase: PhaseApplying, Txid: "b"})
	if ClearState(dir, "a") {
		t.Fatal("cleared another update's state")
	}
	if !ClearState(dir, "b") {
		t.Fatal("did not clear its own state")
	}
	if st, _ := ReadState(dir); st != nil {
		t.Fatalf("state %+v", st)
	}
}

func TestMarkHealthReportsForASparkleUpdate(t *testing.T) {
	dir := t.TempDir()
	writeJSONAtomic(filepath.Join(dir, stateFile), &State{Schema: StateSchema, Phase: PhaseApplying, From: "1.0.0", To: "2.0.0", Txid: "t", Installer: InstallerSparkle, Time: time.Now()})
	if err := MarkHealth(dir, "2.0.0", HealthServing); err != nil {
		t.Fatal(err)
	}
	if h, _ := readHealth(dir); h == nil || h.Txid != "t" || h.Phase != HealthServing {
		t.Fatalf("health %+v", h)
	}
}

func TestRestoreBundleSwapsAndDeletesTheFailedOne(t *testing.T) {
	root := t.TempDir()
	bundle := filepath.Join(root, "HTMLClay.app")
	previous := filepath.Join(root, "copy", "HTMLClay.app")
	writeBundle(t, bundle, "2.0.0", "serve")
	writeBundle(t, previous, "1.0.0", "idle")
	if err := restoreBundle(previous, bundle); err != nil {
		t.Fatal(err)
	}
	bundleHolds(t, bundle, "1.0.0", "idle")
	if _, err := os.Stat(previous); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the failed bundle was not deleted")
	}
	// Sparkle may already have removed the bundle.
	writeBundle(t, previous, "1.0.0", "idle")
	os.RemoveAll(bundle)
	if err := restoreBundle(previous, bundle); err != nil {
		t.Fatal(err)
	}
	bundleHolds(t, bundle, "1.0.0", "idle")
}
