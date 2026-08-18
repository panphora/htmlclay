//go:build !windows

package main

import (
	"os"
	"syscall"
)

// wireStopSignals ends a wire command. Ctrl-C arrives as SIGINT; a supervisor
// sends SIGTERM.
var wireStopSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

// wireChildStop is what a cancelled request sends its handler: an ask to stop
// rather than a kill, so a handler mid-write finishes the file. The caller's
// WaitDelay is what handles one that ignores it.
var wireChildStop os.Signal = syscall.SIGTERM
