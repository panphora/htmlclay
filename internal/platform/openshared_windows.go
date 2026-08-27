//go:build windows

package platform

import (
	"os"

	"golang.org/x/sys/windows"
)

// openShared opens path read-only with all three sharing modes, so holding the
// handle does not block another process (or another part of htmlclay) from
// writing, renaming, or deleting the file. FILE_SHARE_DELETE is the one that
// matters and the one os.Open leaves out.
func openShared(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(
		p,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}
