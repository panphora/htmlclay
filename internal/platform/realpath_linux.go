//go:build linux

package platform

import (
	"os"
	"strconv"
)

// realPath reads /proc/self/fd/N, the kernel's symlink to the descriptor's current
// path. It resolves through every symlink used to open the file, so a file opened
// via a swapped-in symlink reports the real target, not the alias.
func realPath(f *os.File) (string, error) {
	return os.Readlink("/proc/self/fd/" + strconv.Itoa(int(f.Fd())))
}
