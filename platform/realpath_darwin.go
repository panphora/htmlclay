//go:build darwin

package platform

import (
	"bytes"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// realPath uses fcntl(F_GETPATH), which fills a caller buffer with the descriptor's
// current absolute path. It resolves through every symlink used to open the file, so
// a file opened via a swapped-in symlink reports the real target, not the alias.
func realPath(f *os.File) (string, error) {
	buf := make([]byte, unix.PathMax)
	if _, _, errno := unix.Syscall(unix.SYS_FCNTL, f.Fd(), unix.F_GETPATH, uintptr(unsafe.Pointer(&buf[0]))); errno != 0 {
		return "", errno
	}
	if n := bytes.IndexByte(buf, 0); n >= 0 {
		return string(buf[:n]), nil
	}
	return string(buf), nil
}
