package platform

import "os"

// RealPath returns the canonical filesystem path of the file the descriptor f
// actually points at, asked of the OS by handle rather than derived from the
// pathname used to open it. Because it reads through the open descriptor, a
// concurrent rename or symlink swap of the pathname cannot change the answer: it
// always names where the held inode really lives. Callers use it to enforce "never
// serve internal state" against the descriptor they hold, which closes the
// check-vs-open TOCTOU that any later pathname re-resolution leaves open. It fails
// closed (returns an error) on platforms without a native handle-to-path call.
func RealPath(f *os.File) (string, error) {
	return realPath(f)
}
