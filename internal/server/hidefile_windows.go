//go:build windows

package server

import "golang.org/x/sys/windows"

// hideFile sets the hidden attribute, since Explorer shows dotfiles.
func hideFile(path string) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return
	}
	_ = windows.SetFileAttributes(p, windows.FILE_ATTRIBUTE_HIDDEN)
}
