//go:build !windows

package aiedit

import (
	"errors"
	"os/exec"
	"syscall"
)

// prepareProcess puts the agent in its own process group so a cancel kills the
// whole tree, not just the program that was started.
func prepareProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return killChild(cmd)
	}
}

func killChild(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
