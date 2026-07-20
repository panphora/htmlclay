//go:build !windows

package versions

import (
	"errors"
	"os"
	"syscall"
)

// SyncDir fsyncs a directory so a rename or link into it is durable. Writing and
// fsyncing the temp file only makes its contents durable; without this the
// directory entry that gives those bytes their final name can still be lost, so
// an acknowledged save may not survive power loss.
//
// A filesystem that does not support fsync on a directory reports EINVAL or
// ENOTSUP. That is not a durability failure the caller can act on, so it is
// treated as success rather than failing the write it protects.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	closeErr := d.Close()
	if err != nil {
		if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) {
			return nil
		}
		return err
	}
	return closeErr
}
