//go:build !windows

package server

import (
	"fmt"
	"os"
	"syscall"
)

// fileIdentity returns a device+inode fingerprint so an atomic replacement is
// visible even when size and modtime happen to match.
func fileIdentity(info os.FileInfo) string {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d:%d", uint64(st.Dev), uint64(st.Ino))
}
