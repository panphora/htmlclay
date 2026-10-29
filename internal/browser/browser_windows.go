//go:build windows

package browser

import (
	"errors"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

func OpenURL(url string) error {
	cmd := exec.Command("cmd", "/c", "start", "", url)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	return cmd.Run()
}

// Reveal opens Explorer with path selected. Explorer exits with status 1 even
// when it succeeded, so an exit status is not a failure here.
func Reveal(path string) error {
	cmd := exec.Command("explorer", "/select,"+path)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	err := cmd.Run()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return err
	}
	return nil
}
