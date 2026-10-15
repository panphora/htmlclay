//go:build darwin

package update

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// This file is the recovery half of the macOS update. Sparkle installs and
// relaunches; before it does, the app copies its own signed bundle aside and
// starts a supervisor from that copy. The supervisor touches nothing while the
// update goes well. If the new version never reports serving, it kills it,
// swaps the copy back into place and relaunches the previous version.
// A prepared update that Sparkle installs on a plain quit gets a supervisor
// too (State.Quit): it starts the new version without its tray to check it,
// and relaunches nothing.

// SparkleParams describes one Sparkle install for BeginSparkle.
type SparkleParams struct {
	From string
	To   string
	// Bundle is the installed .app that Sparkle replaces.
	Bundle string
	// Executable is the running binary inside Bundle.
	Executable string
	// OldPID is the process Sparkle is about to quit, normally os.Getpid().
	OldPID int
	// HandoffPath is the session handoff already written for To, if any.
	HandoffPath string
	// Quit says Sparkle installs To on a plain quit and relaunches nothing.
	Quit bool
}

// BeginSparkle copies Bundle aside, records the update as applying and starts
// the recovery supervisor from the copy. Call it right before asking Sparkle to
// install. On error nothing is left behind.
func BeginSparkle(dir string, p SparkleParams) (*State, error) {
	st, err := prepareSparkle(dir, p)
	if err != nil {
		return nil, err
	}
	cmd, err := startDetached(filepath.Join(st.PreviousPath, st.Executable), finishUpdateArg)
	if err != nil {
		os.Remove(filepath.Join(dir, stateFile))
		os.RemoveAll(filepath.Dir(st.PreviousPath))
		return nil, fmt.Errorf("start the recovery supervisor: %w", err)
	}
	cmd.Process.Release()
	return st, nil
}

// prepareSparkle is BeginSparkle without starting the supervisor.
func prepareSparkle(dir string, p SparkleParams) (*State, error) {
	if p.From == "" || p.To == "" || p.Bundle == "" || p.Executable == "" || p.OldPID <= 0 {
		return nil, errors.New("update: incomplete Sparkle parameters")
	}
	rel, err := filepath.Rel(p.Bundle, p.Executable)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return nil, fmt.Errorf("update: %s is not inside %s", p.Executable, p.Bundle)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	txid := make([]byte, 16)
	if _, err := rand.Read(txid); err != nil {
		return nil, err
	}
	id := hex.EncodeToString(txid)
	// One copy per update, so a supervisor that gave up on an earlier click can
	// never delete the copy a later one depends on. Older copies go now; a
	// supervisor still running from one keeps its open inode.
	root := filepath.Join(dir, "previous")
	if err := os.RemoveAll(root); err != nil {
		return nil, err
	}
	previous := filepath.Join(root, id, filepath.Base(p.Bundle))
	if err := os.MkdirAll(filepath.Dir(previous), 0o700); err != nil {
		return nil, err
	}
	// ditto keeps the code signature, symlinks and extended attributes intact.
	if out, err := exec.Command("/usr/bin/ditto", p.Bundle, previous).CombinedOutput(); err != nil {
		os.RemoveAll(root)
		return nil, fmt.Errorf("copy %s: %v: %s", p.Bundle, err, bytes.TrimSpace(out))
	}
	st := &State{
		Schema:       StateSchema,
		Phase:        PhaseApplying,
		From:         p.From,
		To:           p.To,
		OldPID:       p.OldPID,
		InstallPath:  p.Bundle,
		PreviousPath: previous,
		Txid:         id,
		Time:         time.Now().UTC(),
		HandoffPath:  p.HandoffPath,
		Installer:    InstallerSparkle,
		Executable:   rel,
		Quit:         p.Quit,
	}
	if err := writeJSONAtomic(filepath.Join(dir, stateFile), st); err != nil {
		os.RemoveAll(root)
		return nil, err
	}
	return st, nil
}

// finishSparkleUpdate is FinishUpdate for a Sparkle install. It waits for the
// old process to quit, for Sparkle to put To in the bundle, and for To to report
// serving. Sparkle relaunches To itself, so this launches nothing unless the
// update failed.
func finishSparkleUpdate(dir string, st *State, opts SupervisorOptions, logf func(string, ...any)) error {
	if opts.Relaunch == nil {
		opts.Relaunch = openBundle
	}
	copyDir := filepath.Dir(st.PreviousPath)
	handoff := readHandoff(st.HandoffPath, logf)

	if !waitGone(st.OldPID, opts.OldExitTimeout) {
		// The app gives up on Sparkle's quit sooner than this and keeps running
		// the version it is; there is nothing to recover.
		logf("%s did not quit within %s, so Sparkle never installed %s; leaving it running", st.From, opts.OldExitTimeout, st.To)
		ClearState(dir, st.Txid)
		os.RemoveAll(copyDir)
		return fmt.Errorf("Sparkle did not start installing %s", st.To)
	}
	if cur, _ := ReadState(dir); cur == nil || cur.Txid != st.Txid {
		logf("update %s was cancelled or replaced; leaving it", st.Txid)
		os.RemoveAll(copyDir)
		return fmt.Errorf("update %s is no longer current", st.Txid)
	}

	if !waitBundleVersion(st.InstallPath, st.To, opts.InstallTimeout, opts.Poll) {
		logf("Sparkle did not install %s within %s; %s still holds %s", st.To, opts.InstallTimeout, st.InstallPath, st.From)
		ClearState(dir, st.Txid)
		os.RemoveAll(copyDir)
		if st.Quit {
			return fmt.Errorf("Sparkle did not install %s", st.To)
		}
		if len(runningCopies(filepath.Join(st.InstallPath, st.Executable))) == 0 {
			if err := retargetHandoff(st.HandoffPath, handoff, st.From); err != nil {
				logf("could not hand the sessions back to %s: %v", st.From, err)
			}
			if err := opts.Relaunch(st.InstallPath); err != nil {
				logf("could not relaunch %s: %v", st.InstallPath, err)
			} else {
				logf("relaunched %s from %s", st.From, st.InstallPath)
			}
		}
		return fmt.Errorf("Sparkle did not install %s", st.To)
	}
	logf("Sparkle installed %s at %s", st.To, st.InstallPath)

	var reason string
	if st.Quit {
		reason = verifyQuitInstall(dir, st, opts, logf)
	} else {
		reason = waitSparkleServing(dir, st, opts)
	}
	if reason == "" {
		st.Phase = PhaseDone
		if err := writeJSONAtomic(filepath.Join(dir, stateFile), st); err != nil {
			logf("update %s is serving but the state could not be saved: %v", st.Txid, err)
		}
		os.RemoveAll(copyDir)
		logf("update %s finished: %s is serving", st.Txid, st.To)
		return nil
	}

	logf("rolling back update %s: %s", st.Txid, reason)
	stopBundle(dir, st)
	st.Failed = &Failure{Version: st.To, Reason: reason, Time: time.Now().UTC()}
	relaunch := st.InstallPath
	if err := restoreBundle(st.PreviousPath, st.InstallPath); err != nil {
		logf("could not move %s back to %s: %v", st.PreviousPath, st.InstallPath, err)
		st.Phase = PhaseRollbackFailed
		relaunch = st.PreviousPath
	} else {
		st.Phase = PhaseRolledBack
		os.RemoveAll(copyDir)
	}
	if err := writeJSONAtomic(filepath.Join(dir, stateFile), st); err != nil {
		logf("could not save the rollback state: %v", err)
	}
	if st.Quit {
		logf("%s is back and not relaunched, since HTML Clay had been quit", st.From)
		return fmt.Errorf("update to %s rolled back: %s", st.To, reason)
	}
	if err := retargetHandoff(st.HandoffPath, handoff, st.From); err != nil {
		logf("could not hand the sessions back to %s: %v", st.From, err)
	}
	if err := opts.Relaunch(relaunch); err != nil {
		logf("could not relaunch %s: %v", relaunch, err)
	} else {
		logf("relaunched %s from %s", st.From, relaunch)
	}
	return fmt.Errorf("update to %s rolled back: %s", st.To, reason)
}

// verifyQuitInstall checks a version Sparkle installed on a plain quit, which
// nothing relaunches. A launch of the person's own that already reported
// serving counts. Otherwise it starts the bundle's binary with --no-tray, so
// no menu bar icon appears, waits for it to report serving, and stops it again,
// because the person had quit. It returns "" or why the version failed.
func verifyQuitInstall(dir string, st *State, opts SupervisorOptions, logf func(string, ...any)) string {
	if h, _ := readHealth(dir); h != nil && h.Schema == HealthSchema && h.Txid == st.Txid && h.Version == st.To && h.Phase == HealthServing {
		return ""
	}
	reason, cmd, exited := launchAndWatch(dir, st, opts, logf, filepath.Join(st.InstallPath, st.Executable), "--no-tray")
	if reason != "" {
		return reason
	}
	select {
	case <-exited:
	default:
		cmd.Process.Signal(unix.SIGTERM)
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			cmd.Process.Kill()
			<-exited
		}
	}
	logf("%s started and served; stopped it again since HTML Clay had been quit", st.To)
	return ""
}

// waitBundleVersion polls until bundle's CFBundleVersion is want, the sign that
// Sparkle's installer has swapped the new bundle in.
func waitBundleVersion(bundle, want string, timeout, poll time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if v, err := BundleVersion(bundle); err == nil && v == want {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(poll)
	}
}

// BundleVersion returns the CFBundleVersion in bundle's Info.plist. plutil
// reads both the XML and the binary format.
func BundleVersion(bundle string) (string, error) {
	out, err := exec.Command("/usr/bin/plutil", "-extract", "CFBundleVersion", "raw", "-o", "-", filepath.Join(bundle, "Contents", "Info.plist")).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// waitSparkleServing returns "" once To reports serving for this update, or why
// it did not.
func waitSparkleServing(dir string, st *State, opts SupervisorOptions) string {
	deadline := time.Now().Add(opts.ServingTimeout)
	for {
		if h, _ := readHealth(dir); h != nil && h.Schema == HealthSchema && h.Txid == st.Txid && h.Version == st.To {
			if h.Phase == HealthServing {
				return ""
			}
			if h.Phase == HealthStarted && h.PID > 0 && !processAlive(h.PID) {
				return fmt.Sprintf("%s exited before it reported serving", st.To)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Sprintf("%s did not report serving within %s", st.To, opts.ServingTimeout)
		}
		time.Sleep(opts.Poll)
	}
}

// stopBundle kills every process of the new version: the one that reported
// started, and any other running the bundle's binary, such as one that hung
// before it took the single-instance lock. The rollback then never swaps a
// bundle out from under a live process, and the old version can take the lock
// and the port.
func stopBundle(dir string, st *State) {
	pids := runningCopies(filepath.Join(st.InstallPath, st.Executable))
	if h, _ := readHealth(dir); h != nil && h.Txid == st.Txid && h.PID > 0 && h.PID != os.Getpid() {
		pids = append(pids, h.PID)
	}
	for _, pid := range pids {
		killPID(pid)
	}
	for _, pid := range pids {
		waitGone(pid, 5*time.Second)
	}
}

// runningCopies returns the pids, other than this one, whose executable is exe.
// ps prints each process's full executable path in the comm column on macOS.
func runningCopies(exe string) []int {
	out, err := exec.Command("/bin/ps", "-axo", "pid=,comm=").Output()
	if err != nil {
		return nil
	}
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.SplitN(strings.TrimSpace(line), " ", 2)
		if len(fields) != 2 || strings.TrimSpace(fields[1]) != exe {
			continue
		}
		if pid, err := strconv.Atoi(fields[0]); err == nil && pid != os.Getpid() {
			pids = append(pids, pid)
		}
	}
	return pids
}

// restoreBundle puts previous back at bundle with one atomic swap, then deletes
// what was swapped out. The copy lives in the config folder, which shares a
// volume with /Applications on a standard install; on another volume it is
// copied beside the bundle first so the swap stays a rename.
func restoreBundle(previous, bundle string) error {
	err := unix.RenamexNp(previous, bundle, unix.RENAME_SWAP)
	if errors.Is(err, unix.EXDEV) {
		staged := filepath.Join(filepath.Dir(bundle), "."+filepath.Base(bundle)+".restore")
		if err := os.RemoveAll(staged); err != nil {
			return err
		}
		if out, err := exec.Command("/usr/bin/ditto", previous, staged).CombinedOutput(); err != nil {
			os.RemoveAll(staged)
			return fmt.Errorf("copy %s: %v: %s", previous, err, bytes.TrimSpace(out))
		}
		previous = staged
		err = unix.RenamexNp(staged, bundle, unix.RENAME_SWAP)
	}
	if errors.Is(err, unix.ENOENT) {
		if _, statErr := os.Lstat(bundle); errors.Is(statErr, os.ErrNotExist) {
			return os.Rename(previous, bundle)
		}
	}
	if err != nil {
		return err
	}
	return os.RemoveAll(previous)
}

// openBundle starts bundle through LaunchServices, the way Sparkle and the
// Finder do. -n because this supervisor's own binary now sits inside it.
func openBundle(bundle string) error {
	return exec.Command("/usr/bin/open", "-n", bundle).Run()
}
