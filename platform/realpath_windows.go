//go:build windows

package platform

import (
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

// realPath uses GetFinalPathNameByHandle to get the descriptor's canonical path,
// resolving through symlinks and junctions. It strips the \\?\ extended-length
// prefix (and normalizes \\?\UNC\ back to \\) so the result compares against ordinary
// paths.
func realPath(f *os.File) (string, error) {
	h := windows.Handle(f.Fd())
	// FILE_NAME_NORMALIZED (0x0) and VOLUME_NAME_DOS (0x0) are the defaults for
	// GetFinalPathNameByHandle but are not exported by x/sys/windows; their combined
	// value is 0.
	const flags = 0
	buf := make([]uint16, windows.MAX_PATH)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), flags)
		if err != nil {
			return "", err
		}
		// When the buffer is too small the return value is the required size
		// (including the terminating NUL); grow and retry. Otherwise it is the
		// character count without the NUL.
		if int(n) > len(buf) {
			buf = make([]uint16, n)
			continue
		}
		p := windows.UTF16ToString(buf[:n])
		const uncPrefix = `\\?\UNC\`
		const extPrefix = `\\?\`
		switch {
		case strings.HasPrefix(p, uncPrefix):
			p = `\\` + p[len(uncPrefix):]
		case strings.HasPrefix(p, extPrefix):
			p = p[len(extPrefix):]
		}
		return p, nil
	}
}
