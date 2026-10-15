//go:build darwin

package sparkle

/*
#cgo LDFLAGS: -framework Cocoa
#include <stdlib.h>
#include "sparkle_darwin.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

var (
	handlerMu sync.Mutex
	handler   func(Event)
	skip      func(version string) string
)

//export goSparkleEvent
func goSparkleEvent(kind C.int, text *C.char) {
	ev := Event{Kind: EventKind(kind), Text: C.GoString(text)}
	handlerMu.Lock()
	h := handler
	handlerMu.Unlock()
	if h != nil {
		// Off the main thread: a handler that blocked here would freeze the
		// menu bar and Sparkle with it.
		go h(ev)
	}
}

//export goSparkleSkipReason
func goSparkleSkipReason(version *C.char) *C.char {
	handlerMu.Lock()
	s := skip
	handlerMu.Unlock()
	if s == nil {
		return nil
	}
	if reason := s(C.GoString(version)); reason != "" {
		return C.CString(reason)
	}
	return nil
}

// SetSkip registers a check Sparkle runs on the main thread before it
// downloads an update: a non-empty answer is the reason to skip that version,
// and Sparkle aborts the cycle with it. It must return quickly.
func SetSkip(f func(version string) string) {
	handlerMu.Lock()
	skip = f
	handlerMu.Unlock()
}

// Load opens the Sparkle framework at frameworkPath in this process.
func Load(frameworkPath string) error {
	cpath := C.CString(frameworkPath)
	defer C.free(unsafe.Pointer(cpath))
	if msg := C.hcSparkleLoad(cpath); msg != nil {
		defer C.free(unsafe.Pointer(msg))
		return errors.New(C.GoString(msg))
	}
	return nil
}

// Start creates the updater on the main thread once the Cocoa run loop is
// running, and starts its scheduled background checks. events receives every
// Event. Call Load first.
func Start(events func(Event)) {
	handlerMu.Lock()
	handler = events
	handlerMu.Unlock()
	C.hcSparkleStart()
}

// InstallNow asks Sparkle to install the prepared update. Sparkle then sends
// this app a quit Apple event, installs once it has exited, and relaunches the
// new version. When there is nothing prepared, an InstallFailed event follows.
func InstallNow() {
	C.hcSparkleInstallNow()
}

// InfoString returns a string value from the running bundle's Info.plist, or
// "" when the key is missing or not a string.
func InfoString(key string) string {
	ckey := C.CString(key)
	defer C.free(unsafe.Pointer(ckey))
	v := C.hcSparkleInfoString(ckey)
	if v == nil {
		return ""
	}
	defer C.free(unsafe.Pointer(v))
	return C.GoString(v)
}

// BundleWritable reports whether Sparkle can replace bundle without asking for
// an administrator: it must be able to create a file beside the bundle and
// write the bundle folder itself. Creating the file is the real test for the
// parent, because a mode check misses ACLs and read-only volumes such as a
// mounted DMG.
func BundleWritable(bundle string) error {
	f, err := os.CreateTemp(filepath.Dir(bundle), ".htmlclay-write-check-*")
	if err != nil {
		return fmt.Errorf("the folder holding %s is not writable: %w", bundle, err)
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	if err := unix.Access(bundle, unix.W_OK); err != nil {
		return fmt.Errorf("%s is not writable: %w", bundle, err)
	}
	return nil
}
