//go:build !windows

package main

import (
	"os"
	"syscall"
)

func launchArgv(path string) ([]string, *syscall.SysProcAttr) {
	return []string{path}, nil
}

var helperChildStop os.Signal = syscall.SIGTERM
