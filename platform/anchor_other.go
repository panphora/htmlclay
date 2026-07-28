//go:build !windows

package platform

import (
	"fmt"
	"os"
)

// identity on Unix is a held descriptor plus the FileInfo read through it. The
// descriptor is the point, not an implementation detail: (device, inode) only
// means anything while something keeps the inode alive, and while this handle is
// open the number cannot be handed to a newly created file. Holding it is free
// here, since a Unix handle blocks neither rename nor unlink.
type identity struct {
	file *os.File
	info os.FileInfo
}

func readIdentity(path string) (identity, error) {
	f, err := os.Open(path)
	if err != nil {
		return identity{}, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return identity{}, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return identity{}, fmt.Errorf("anchor %s: not a regular file", path)
	}
	return identity{file: f, info: info}, nil
}

func (id *identity) same(other identity) bool {
	return os.SameFile(id.info, other.info)
}

func (id *identity) close() {
	if id.file != nil {
		id.file.Close()
		id.file = nil
	}
}
