//go:build !darwin && !linux && !windows

package platform

import (
	"errors"
	"os"
)

// realPath fails closed on platforms without a handle-to-path syscall, so a caller
// that enforces "never serve internal state" via the descriptor's real path denies
// rather than serving a file it cannot verify.
func realPath(f *os.File) (string, error) {
	return "", errors.New("descriptor real-path lookup not implemented on this platform")
}
