//go:build windows

package platform

import (
	"os/exec"
	"strconv"
)

// taskkill without /F posts WM_CLOSE to the process's windows, which is the polite
// request; /F terminates it.
func quitProcess(pid int, force bool) error {
	args := []string{"/PID", strconv.Itoa(pid)}
	if force {
		args = append(args, "/F")
	}
	cmd := exec.Command("taskkill", args...)
	noConsoleWindow(cmd)
	return cmd.Run()
}
