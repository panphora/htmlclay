//go:build !windows

package main

import "syscall"

func launchArgv(path string) ([]string, *syscall.SysProcAttr) {
	return []string{path}, nil
}
