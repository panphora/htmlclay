//go:build windows

package platform

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32          = windows.NewLazySystemDLL("user32.dll")
	procMessageBoxW = user32.NewProc("MessageBoxW")
)

// notify shows a modal error dialog via user32!MessageBoxW. A toast would need
// a WinRT/AppUserModelID dance; a message box is built in and impossible to
// miss, which is what a "your file did not open" error needs.
func notify(title, message string) error {
	t, err := windows.UTF16PtrFromString(title)
	if err != nil {
		return err
	}
	m, err := windows.UTF16PtrFromString(message)
	if err != nil {
		return err
	}
	const mbIconError = 0x10
	const mbSetForeground = 0x10000
	procMessageBoxW.Call(0,
		uintptr(unsafe.Pointer(m)),
		uintptr(unsafe.Pointer(t)),
		uintptr(mbIconError|mbSetForeground))
	return nil
}
