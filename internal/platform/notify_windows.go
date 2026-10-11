//go:build windows

package platform

import (
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32                  = windows.NewLazySystemDLL("user32.dll")
	procMessageBoxW         = user32.NewProc("MessageBoxW")
	procFindWindowExW       = user32.NewProc("FindWindowExW")
	procAttachThreadInput   = user32.NewProc("AttachThreadInput")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procSetWindowPos        = user32.NewProc("SetWindowPos")
)

// notify shows a modal error dialog via user32!MessageBoxW. A toast would need
// a WinRT/AppUserModelID dance; a message box is built in and impossible to
// miss, which is what a "your file did not open" error needs.
func notify(title, message string) error {
	const mbIconError = 0x10
	return messageBox(title, message, mbIconError)
}

// inform is the same box with the information icon. Windows has no notice that
// fades on its own short of a toast, which needs a registered AppUserModelID.
func inform(title, message string) error {
	const mbIconInformation = 0x40
	return messageBox(title, message, mbIconInformation)
}

func messageBox(title, message string, icon uintptr) error {
	t, err := windows.UTF16PtrFromString(title)
	if err != nil {
		return err
	}
	m, err := windows.UTF16PtrFromString(message)
	if err != nil {
		return err
	}
	// The box belongs to the thread that calls MessageBoxW, so pin it: raiseBox
	// looks the box up by that thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	done := make(chan struct{})
	defer close(done)
	go raiseBox(t, windows.GetCurrentThreadId(), done)

	const mbSetForeground = 0x10000
	const mbTopmost = 0x40000
	procMessageBoxW.Call(0,
		uintptr(unsafe.Pointer(m)),
		uintptr(unsafe.Pointer(t)),
		icon|mbSetForeground|mbTopmost)
	return nil
}

// raiseBox brings the message box that thread owner is opening to the front.
// HTML Clay is rarely the foreground process when it reports something, and
// Windows then ignores MB_SETFOREGROUND and MB_TOPMOST alike: the box opens
// behind the browser and only its taskbar button flashes.
func raiseBox(title *uint16, owner uint32, done <-chan struct{}) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if box := findDialog(title, owner, done); box != 0 {
		bringToFront(box)
	}
}

// findDialog waits up to two seconds for the dialog window (class #32770) with
// this title that thread owner is opening, and returns 0 if it never appears or
// done closes first.
func findDialog(title *uint16, owner uint32, done <-chan struct{}) uintptr {
	class, _ := windows.UTF16PtrFromString("#32770")
	for range 100 {
		select {
		case <-done:
			return 0
		case <-time.After(20 * time.Millisecond):
		}
		var box uintptr
		for {
			box, _, _ = procFindWindowExW.Call(0, box, uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(title)))
			if box == 0 {
				break
			}
			if tid, _ := windows.GetWindowThreadProcessId(windows.HWND(box), nil); tid == owner {
				return box
			}
		}
	}
	return 0
}

// bringToFront makes h the foreground window. SetForegroundWindow from a
// background process is refused unless the caller shares the foreground
// thread's input, so attach to it for the call. Even attached, a process with
// no console of its own stays behind the browser until the box is made
// topmost first.
func bringToFront(h uintptr) {
	fg, _ := windows.GetWindowThreadProcessId(windows.GetForegroundWindow(), nil)
	me := windows.GetCurrentThreadId()
	attached := false
	if fg != 0 && fg != me {
		r, _, _ := procAttachThreadInput.Call(uintptr(me), uintptr(fg), 1)
		attached = r != 0
	}
	const (
		swpNoSize     = 0x1
		swpNoMove     = 0x2
		swpShowWindow = 0x40
	)
	hwndTopmost := ^uintptr(0)
	procSetWindowPos.Call(h, hwndTopmost, 0, 0, 0, 0, swpNoSize|swpNoMove|swpShowWindow)
	procSetForegroundWindow.Call(h)
	if attached {
		procAttachThreadInput.Call(uintptr(me), uintptr(fg), 0)
	}
}
