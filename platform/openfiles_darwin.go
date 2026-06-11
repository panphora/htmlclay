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
// installed from applicationWillFinishLaunching: (via an NSApplication
// notification) so it replaces AppKit's default open-documents handler; both the
// cold-launch event and later warm events are then delivered to our callback.
func OnOpenFile(cb func(string)) {
	openFileMu.Lock()
	openFileCb = cb
	openFileMu.Unlock()
	C.installOpenFileHandler()
}
