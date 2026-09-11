//go:build !windows

package helper

import (
	"os"
	"syscall"
)

func launchArgv(path string) ([]string, *syscall.SysProcAttr) {
	return []string{path}, nil
}

var childStop os.Signal = syscall.SIGTERM
