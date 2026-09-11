//go:build !windows

package helper

import (
	"os/exec"
	"syscall"
)

func launchArgv(path string) ([]string, *syscall.SysProcAttr) {
	return []string{path}, nil
}

// prepareStop returns the ask-first stop: SIGTERM, which the program can trap to
// finish its write. os/exec kills it after WaitDelay if it is still running.
func prepareStop(cmd *exec.Cmd) func() error {
	return func() error { return cmd.Process.Signal(syscall.SIGTERM) }
}
