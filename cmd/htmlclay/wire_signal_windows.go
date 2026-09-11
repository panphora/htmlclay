//go:build windows

package main

import "os"

// Windows has no SIGTERM to deliver, so a wire command stops on Ctrl-C alone.
var wireStopSignals = []os.Signal{os.Interrupt}
