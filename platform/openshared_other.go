//go:build !windows

package platform

import "os"

// openShared is plain os.Open everywhere but Windows: a Unix open handle never
// blocks rename or unlink, so there is no sharing mode to ask for.
func openShared(path string) (*os.File, error) {
	return os.Open(path)
}
