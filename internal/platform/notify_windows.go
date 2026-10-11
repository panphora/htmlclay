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
	const mbSetForeground = 0x10000
	procMessageBoxW.Call(0,
		uintptr(unsafe.Pointer(m)),
		uintptr(unsafe.Pointer(t)),
		icon|mbSetForeground)
	return nil
}
