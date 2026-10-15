//go:build !windows

package update

import (
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The helper binaries are real Go programs built from testdata/helper, so the
// supervisor launches, watches and kills real processes and the swap moves real
// executables. They are built once per test run.
var (
	helpersOnce sync.Once
	helpersDir  string
	helpersErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if helpersDir != "" {
		os.RemoveAll(helpersDir)
	}
	os.Exit(code)
}

// helper returns the path of the helper built with version and mode.
func helper(t *testing.T, version, mode string) string {
	t.Helper()
	helpersOnce.Do(func() {
		goBin, err := exec.LookPath("go")
		if err != nil {
			helpersErr = err
			return
		}
		helpersDir, helpersErr = os.MkdirTemp("", "htmlclay-update-helpers-")
		if helpersErr != nil {
			return
		}
		for _, v := range [][2]string{{"1.0.0", "idle"}, {"2.0.0", "serve"}, {"2.0.0", "crash"}, {"2.0.0", "hang"}} {
			out := filepath.Join(helpersDir, v[0]+"-"+v[1])
			cmd := exec.Command(goBin, "build", "-o", out,
				"-ldflags", "-X main.version="+v[0]+" -X main.mode="+v[1], "./testdata/helper")
			if b, err := cmd.CombinedOutput(); err != nil {
				helpersErr = errors.New(string(b))
				return
			}
		}
	})
	if helpersErr != nil {
		t.Fatalf("building the helper binaries: %v", helpersErr)
	}
	return filepath.Join(helpersDir, version+"-"+mode)
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		t.Fatal(err)
	}
}

func gzipFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(data)
	zw.Close()
	if err := os.WriteFile(dst, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sameContent(t *testing.T, a, b string) bool {
	t.Helper()
	da, err := os.ReadFile(a)
	if err != nil {
		t.Fatal(err)
	}
	db, err := os.ReadFile(b)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(da, db)
}

// fixture is an install of the 1.0.0 helper with newMode's 2.0.0 helper staged
// beside it from a .gz artifact, the way a Linux update arrives.
type fixture struct {
	installDir, install, staged, updates, markers string
}

func newFixture(t *testing.T, newMode string) fixture {
	t.Helper()
	root := t.TempDir()
	f := fixture{
		installDir: filepath.Join(root, "bin"),
		updates:    filepath.Join(root, "config", "updates"),
		markers:    filepath.Join(root, "markers"),
	}
	for _, d := range []string{f.installDir, f.updates, f.markers} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.install = filepath.Join(f.installDir, "htmlclay")
	copyFile(t, helper(t, "1.0.0", "idle"), f.install)
	artifact := filepath.Join(f.updates, "htmlclay-2.0.0-test.gz")
	gzipFile(t, helper(t, "2.0.0", newMode), artifact)
	staged, err := Stage(artifact, f.install)
	if err != nil {
		t.Fatal(err)
	}
	f.staged = staged
	t.Setenv("HTMLCLAY_TEST_UPDATES_DIR", f.updates)
	t.Setenv("HTMLCLAY_TEST_MARKERS", f.markers)
	return f
}

// exitedPID returns the pid of a process that ran briefly and has been reaped,
// standing in for the old app that exits after Apply.
func exitedPID(t *testing.T, life time.Duration) int {
	t.Helper()
	cmd := exec.Command("sleep", strconv.FormatFloat(life.Seconds(), 'f', 3, 64))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go cmd.Wait()
	return cmd.Process.Pid
}

func waitForPhase(t *testing.T, dir string, timeout time.Duration, phases ...string) *State {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if st, _ := ReadState(dir); st != nil {
			for _, p := range phases {
				if st.Phase == p {
					return st
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	st, _ := ReadState(dir)
	log, _ := os.ReadFile(filepath.Join(dir, supervisorLog))
	t.Fatalf("state never reached %v; last %+v\nsupervisor log:\n%s", phases, st, log)
	return nil
}

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
}

func TestStageGzipArtifact(t *testing.T) {
	f := newFixture(t, "serve")
	if f.staged != filepath.Join(f.installDir, ".htmlclay.new") {
		t.Fatalf("staged at %s, want beside the install", f.staged)
	}
	if !sameContent(t, f.staged, helper(t, "2.0.0", "serve")) {
		t.Fatal("the staged binary is not the decompressed artifact")
	}
	info, err := os.Stat(f.staged)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("staged mode %v, want 0755", info.Mode().Perm())
	}
}

func TestStageRejectsCorruptGzip(t *testing.T) {
	dir := t.TempDir()
	artifact := filepath.Join(dir, "htmlclay.gz")
	os.WriteFile(artifact, []byte("not gzip"), 0o644)
	if _, err := Stage(artifact, filepath.Join(dir, "htmlclay")); err == nil {
		t.Fatal("a corrupt artifact was staged")
	}
	if _, err := os.Stat(filepath.Join(dir, ".htmlclay.new")); err == nil {
		t.Fatal("a corrupt artifact left a staged file behind")
	}
}

func TestCheckWritable(t *testing.T) {
	dir := t.TempDir()
	if err := CheckWritable(filepath.Join(dir, "htmlclay")); err != nil {
		t.Fatalf("a writable folder was refused: %v", err)
	}
	if os.Geteuid() == 0 {
		t.Skip("root writes through any mode")
	}
	locked := filepath.Join(dir, "locked")
	os.Mkdir(locked, 0o555)
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	if err := CheckWritable(filepath.Join(locked, "htmlclay")); !errors.Is(err, ErrNotWritable) {
		t.Fatalf("a read-only folder gave %v, want ErrNotWritable", err)
	}
}

func TestMarkHealthOnlyForTheApplyingVersion(t *testing.T) {
	dir := t.TempDir()
	if err := MarkHealth(dir, "2.0.0", HealthStarted); err != nil {
		t.Fatalf("an ordinary launch with no state: %v", err)
	}
	st := &State{Schema: StateSchema, Phase: PhaseApplying, From: "1.0.0", To: "2.0.0", Txid: "abc", Time: time.Now()}
	writeJSONAtomic(filepath.Join(dir, stateFile), st)
	MarkHealth(dir, "1.0.0", HealthStarted)
	if h, _ := readHealth(dir); h != nil {
		t.Fatalf("the old version wrote health: %+v", h)
	}
	MarkHealth(dir, "2.0.0", HealthServing)
	h, _ := readHealth(dir)
	if h == nil || h.Txid != "abc" || h.Phase != HealthServing || h.PID != os.Getpid() || h.Schema != HealthSchema {
		t.Fatalf("health %+v", h)
	}
	os.Remove(filepath.Join(dir, healthFile))
	st.Phase = PhaseRolledBack
	writeJSONAtomic(filepath.Join(dir, stateFile), st)
	MarkHealth(dir, "2.0.0", HealthStarted)
	if h, _ := readHealth(dir); h != nil {
		t.Fatalf("health written after the update ended: %+v", h)
	}
}

// The whole path the way production runs it: Apply swaps and spawns the
// supervisor from the previous binary, detached, and the supervisor finishes
// once the new version reports serving.
func TestApplySwapsLaunchesAndFinishes(t *testing.T) {
	f := newFixture(t, "serve")
	st, err := Apply(f.updates, ApplyParams{From: "1.0.0", To: "2.0.0", InstallPath: f.install, StagedPath: f.staged, OldPID: exitedPID(t, 300*time.Millisecond)})
	if err != nil {
		t.Fatal(err)
	}
	if !sameContent(t, f.install, helper(t, "2.0.0", "serve")) {
		t.Fatal("the install path does not hold the new binary after the swap")
	}
	done := waitForPhase(t, f.updates, 20*time.Second, PhaseDone, PhaseRolledBack, PhaseRollbackFailed)
	if done.Phase != PhaseDone || done.Txid != st.Txid || done.Failed != nil {
		t.Fatalf("state %+v, want done", done)
	}
	if !sameContent(t, f.install, helper(t, "2.0.0", "serve")) {
		t.Fatal("the new binary is not at the install path")
	}
	if !sameContent(t, st.PreviousPath, helper(t, "1.0.0", "idle")) {
		t.Fatal("the previous binary was not kept")
	}
	if _, err := os.Stat(filepath.Join(f.markers, "launched-1.0.0")); err == nil {
		t.Fatal("the old version was relaunched after a successful update")
	}
	if _, err := os.Stat(filepath.Join(f.markers, "launched-2.0.0")); err != nil {
		t.Fatal("the new version was never launched")
	}
}

func TestFinishRollsBackWhenTheNewVersionExits(t *testing.T) {
	f := newFixture(t, "crash")
	st, err := Prepare(f.updates, ApplyParams{From: "1.0.0", To: "2.0.0", InstallPath: f.install, StagedPath: f.staged, OldPID: exitedPID(t, 100*time.Millisecond)})
	if err != nil {
		t.Fatal(err)
	}
	err = FinishUpdate(f.updates, SupervisorOptions{ExitGrace: 200 * time.Millisecond, ServingTimeout: 10 * time.Second})
	if err == nil {
		t.Fatal("a crashing update finished")
	}
	assertRolledBack(t, f, st, "exited before it reported serving")
}

// The new version deletes the handoff as it reads it, so a version that dies
// after reading it must not take the open tabs' sessions with it: the
// supervisor writes its own copy back, pointed at the version it relaunches.
func TestFinishRollbackHandsTheSessionsBackToTheOldVersion(t *testing.T) {
	f := newFixture(t, "crash")
	handoff := filepath.Join(filepath.Dir(f.updates), "update-handoff.json")
	if err := os.WriteFile(handoff, []byte(keptHandoff), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HTMLCLAY_TEST_HANDOFF", handoff)
	st, err := Prepare(f.updates, ApplyParams{From: "1.0.0", To: "2.0.0", InstallPath: f.install, StagedPath: f.staged, OldPID: exitedPID(t, 100*time.Millisecond), HandoffPath: handoff})
	if err != nil {
		t.Fatal(err)
	}
	if st.HandoffPath != handoff {
		t.Fatalf("state handoff path %q, want %q", st.HandoffPath, handoff)
	}
	before := time.Now().UTC().Add(-time.Second)
	if err := FinishUpdate(f.updates, SupervisorOptions{ExitGrace: 200 * time.Millisecond, ServingTimeout: 10 * time.Second}); err == nil {
		t.Fatal("a crashing update finished")
	}
	assertRolledBack(t, f, st, "exited before it reported serving")
	assertRetargeted(t, handoff, "1.0.0", before)
}

func TestFinishRollsBackWhenServingNeverComes(t *testing.T) {
	f := newFixture(t, "hang")
	st, err := Prepare(f.updates, ApplyParams{From: "1.0.0", To: "2.0.0", InstallPath: f.install, StagedPath: f.staged, OldPID: exitedPID(t, 100*time.Millisecond)})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = FinishUpdate(f.updates, SupervisorOptions{ServingTimeout: 1500 * time.Millisecond})
	if err == nil {
		t.Fatal("an update that never served finished")
	}
	if took := time.Since(start); took > 8*time.Second {
		t.Fatalf("rollback took %s", took)
	}
	h, _ := readHealth(f.updates)
	if h == nil || h.Phase != HealthStarted {
		t.Fatalf("the hanging version should have reported started, health %+v", h)
	}
	if processAlive(h.PID) {
		t.Fatalf("the hanging new version (pid %d) is still running after the rollback", h.PID)
	}
	assertRolledBack(t, f, st, "did not report serving")
}

func assertRolledBack(t *testing.T, f fixture, st *State, reason string) {
	t.Helper()
	got, err := ReadState(f.updates)
	if err != nil || got == nil {
		t.Fatalf("state %v %v", got, err)
	}
	if got.Phase != PhaseRolledBack || got.Failed == nil || got.Failed.Version != "2.0.0" || !strings.Contains(got.Failed.Reason, reason) {
		t.Fatalf("state %+v failed %+v, want rolledBack for 2.0.0 (%s)", got, got.Failed, reason)
	}
	if !sameContent(t, f.install, helper(t, "1.0.0", "idle")) {
		t.Fatal("the old binary is not back at the install path")
	}
	if _, err := os.Stat(st.PreviousPath); err == nil {
		t.Fatal("the backup is still there; it should have moved back to the install path")
	}
	waitForFile(t, filepath.Join(f.markers, "launched-1.0.0"), 5*time.Second)
}

func TestFinishRefusesWithoutAnApplyingState(t *testing.T) {
	dir := t.TempDir()
	if err := FinishUpdate(dir, SupervisorOptions{}); err == nil {
		t.Fatal("finished an update that was never applied")
	}
	st := &State{Schema: StateSchema, Phase: PhaseApplying, From: "1.0.0", To: "2.0.0", OldPID: os.Getpid(), Txid: "old", Time: time.Now().Add(-time.Hour)}
	writeJSONAtomic(filepath.Join(dir, stateFile), st)
	if err := FinishUpdate(dir, SupervisorOptions{}); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("a stale state gave %v", err)
	}
}

// A supervisor that cannot start undoes the swap without losing the update:
// the old binary is back at the install path, the new one is back at its
// staged path, and applying again works.
func TestApplyKeepsTheStagedBinaryWhenTheSupervisorCannotStart(t *testing.T) {
	f := newFixture(t, "serve")
	defer func(s func(string, ...string) (*exec.Cmd, error)) { startSupervisor = s }(startSupervisor)
	startSupervisor = func(string, ...string) (*exec.Cmd, error) { return nil, errors.New("no supervisor") }
	params := ApplyParams{From: "1.0.0", To: "2.0.0", InstallPath: f.install, StagedPath: f.staged, OldPID: exitedPID(t, 300*time.Millisecond)}
	if _, err := Apply(f.updates, params); err == nil {
		t.Fatal("Apply succeeded without a supervisor")
	}
	if !sameContent(t, f.install, helper(t, "1.0.0", "idle")) {
		t.Fatal("the old binary is not back at the install path")
	}
	if !sameContent(t, f.staged, helper(t, "2.0.0", "serve")) {
		t.Fatal("the staged update was destroyed by the undo")
	}
	if st, _ := ReadState(f.updates); st != nil {
		t.Fatalf("state %+v left behind", st)
	}

	startSupervisor = startDetached
	params.OldPID = exitedPID(t, 300*time.Millisecond)
	st, err := Apply(f.updates, params)
	if err != nil {
		t.Fatalf("applying the kept update again: %v", err)
	}
	if done := waitForPhase(t, f.updates, 20*time.Second, PhaseDone, PhaseRolledBack, PhaseRollbackFailed); done.Phase != PhaseDone || done.Txid != st.Txid {
		t.Fatalf("state %+v, want done", done)
	}
}
