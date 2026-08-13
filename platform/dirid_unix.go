//go:build !windows

package platform

import (
	"fmt"
	"os"
	"syscall"
)

// DirIdentity returns a device+inode fingerprint for the directory at path, or
// "" when one cannot be derived. Stat follows symlinks deliberately: a stored
// workspace path whose directory was later swapped for a symlink stats through
// to the link's target, whose fingerprint differs from the recorded one, which
// is exactly the swap the comparison exists to catch.
func DirIdentity(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d:%d", uint64(st.Dev), uint64(st.Ino))
}
