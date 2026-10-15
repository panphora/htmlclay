//go:build !windows

package update

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// stagedPathFor and previousPathFor sit in the install folder, hidden, so they
// share its filesystem and a rename never turns into a copy.
func stagedPathFor(installPath string) string {
	return filepath.Join(filepath.Dir(installPath), "."+filepath.Base(installPath)+".new")
}

func previousPathFor(installPath string) string {
	return filepath.Join(filepath.Dir(installPath), "."+filepath.Base(installPath)+".prev")
}

// swapIn keeps the old binary as a hard link at previous, then renames staged
// over install. The install path names a complete binary at every instant, and
// the running process is untouched because it holds the old inode, which the
// link keeps alive. renameat2(RENAME_EXCHANGE) would give the same guarantee
// on Linux only, and needs a fallback anyway on filesystems that refuse it;
// link plus rename is one path on Linux and macOS alike, so the tests on a Mac
// exercise the code Linux runs.
func swapIn(staged, install, previous string) error {
	if err := os.Remove(previous); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Link(install, previous); err != nil {
		// No hard links here (some FUSE and network mounts). Moving the old binary
		// aside leaves the install path empty for the moment between the renames.
		if err := os.Rename(install, previous); err != nil {
			return err
		}
		if err := os.Rename(staged, install); err != nil {
			os.Rename(previous, install)
			return err
		}
		return nil
	}
	if err := os.Rename(staged, install); err != nil {
		os.Remove(previous)
		return err
	}
	return nil
}

// startDetached starts path in its own session, so nothing that ends the
// caller's process group takes it along: launchd kills a LaunchAgent job's whole
// group on exit, and a terminal's hangup reaches its foreground group.
func startDetached(path string, args ...string) (*exec.Cmd, error) {
	cmd := exec.Command(path, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

// processAlive treats EPERM as alive: the process exists, it is just not ours to signal.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
