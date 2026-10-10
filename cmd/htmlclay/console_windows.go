//go:build windows

package main

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procGetConsoleProcessList = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleProcessList")
	procFreeConsoleSelf       = windows.NewLazySystemDLL("kernel32.dll").NewProc("FreeConsole")
)

// Explorer gives a console program a console of its own, which shows as a
// terminal window beside the tray. When no other process shares it, nobody
// started us from a terminal, so let it go.
func releaseOwnConsole() {
	var pids [2]uint32
	n, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	if n == 1 {
		procFreeConsoleSelf.Call()
	}
}
