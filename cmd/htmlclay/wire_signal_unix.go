//go:build !windows

package main

import (
	"os"
	"syscall"
)

// wireStopSignals ends a wire command. Ctrl-C arrives as SIGINT; a supervisor
// sends SIGTERM.
var wireStopSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}
