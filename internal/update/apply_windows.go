//go:build windows

package update

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// stagedPathFor and previousPathFor sit beside the exe, so they share its volume
// and a move never turns into a copy.
func stagedPathFor(installPath string) string {
	return installPath + ".new"
}

func previousPathFor(installPath string) string {
	return installPath + ".old"
}

// swapIn renames the running exe to previous, then moves staged into its place.
// Windows refuses to overwrite or delete a running exe but allows renaming it
// within its volume, and the path never changes, so the HKCU file associations
// and the Run key stay valid. The previous update's .old is deleted first: its
// supervisor exited long ago, so nothing runs from it any more. This update's
// .old stays until the next one, because the supervisor runs from it.
func swapIn(staged, install, previous string) error {
	if err := os.Remove(previous); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(install, previous); err != nil {
		return err
	}
	if err := os.Rename(staged, install); err != nil {
		os.Rename(previous, install)
		return err
	}
	return nil
}

// startDetached starts path with no console window and in its own process group,
// and asks to leave the caller's job, so closing the job (a terminal, or a
// launcher that kills its job on exit) does not take the supervisor or the new
// version with it. A job that does not allow breakaway refuses that flag
// outright, so the launch is retried without it.
func startDetached(path string, args ...string) (*exec.Cmd, error) {
	base := uint32(windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP)
	cmd := exec.Command(path, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: base | windows.CREATE_BREAKAWAY_FROM_JOB}
	if err := cmd.Start(); err == nil {
		return cmd, nil
	}
	cmd = exec.Command(path, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: base}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

// processAlive treats access denied as alive: the process exists, it is just
// not ours to open.
func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	ev, err := windows.WaitForSingleObject(h, 0)
	return err == nil && ev == uint32(windows.WAIT_TIMEOUT)
}
