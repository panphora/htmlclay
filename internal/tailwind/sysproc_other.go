//go:build !windows

package tailwind

import "syscall"

func sysProcAttr() *syscall.SysProcAttr { return nil }
