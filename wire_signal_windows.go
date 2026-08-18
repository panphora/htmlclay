//go:build windows

package main

import "os"

// Windows has no SIGTERM to deliver: os.Process.Signal accepts only os.Kill for
// another process and errors on anything else, so a cancelled request is killed
// rather than asked to stop. A handler that must clean up after itself has to do
// it from the request it was given, not from a signal it will never receive.
var wireStopSignals = []os.Signal{os.Interrupt}

var wireChildStop os.Signal = os.Kill
