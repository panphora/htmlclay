//go:build windows

package aiedit

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

func prepareProcess(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
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
	return cmd.Process.Kill()
}
