//go:build darwin

package platform

/*
#cgo LDFLAGS: -framework Cocoa
#include "openfiles_darwin.h"
*/
import "C"

import "sync"

var (
	openFileMu sync.Mutex
	openFileCb func(string)
)

//export goOpenFile
func goOpenFile(path *C.char) {
	openFileMu.Lock()
	cb := openFileCb
	openFileMu.Unlock()
	if cb != nil {
		cb(C.GoString(path))
	}
}

// OnOpenFile registers a callback for macOS "open documents" Apple Events, which
// is how Finder delivers double-clicked files (argv is not used). The handler is
// installed on the shared NSAppleEventManager before the systray run loop starts,
// so events queued during a cold launch are delivered once the loop runs.
func OnOpenFile(cb func(string)) {
	openFileMu.Lock()
	openFileCb = cb
	openFileMu.Unlock()
	C.installOpenFileHandler()
}
