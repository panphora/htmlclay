//go:build windows

package browser

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

func OpenURL(url string) error {
	cmd := exec.Command("cmd", "/c", "start", "", url)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	return cmd.Run()
}
