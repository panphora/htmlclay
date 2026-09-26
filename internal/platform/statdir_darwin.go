//go:build darwin

package platform

import (
	"encoding/binary"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// statDir reads the inode and the volume UUID through one open handle, so both
// describe the same directory. It follows symlinks deliberately: a stored
// trusted-folder path whose directory was later swapped for a symlink resolves
// to the link's target, whose fingerprint differs from the recorded one, which
// is exactly the swap the comparison exists to catch. O_DIRECTORY and
// O_NONBLOCK keep a FIFO left at the path from blocking the open. A directory
// that cannot be opened (traverse-only) is read by path instead.
func statDir(path string) (dirStat, bool) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		s, ok := statPath(path)
		if ok {
			s.vol = volumeUUIDAtPath(path)
		}
		return s, ok
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return dirStat{}, false
	}
	return dirStat{dev: uint64(st.Dev), ino: uint64(st.Ino), vol: readVolumeUUID(unix.SYS_FGETATTRLIST, uintptr(fd))}, true
}

func volumeUUIDAtPath(path string) string {
	p, err := unix.BytePtrFromString(path)
	if err != nil {
		return ""
	}
	uuid := readVolumeUUID(unix.SYS_GETATTRLIST, uintptr(unsafe.Pointer(p)))
	runtime.KeepAlive(p)
	return uuid
}

// readVolumeUUID returns the UUID of the volume holding target (a descriptor
// for fgetattrlist, a path for getattrlist), or "" when the filesystem has none
// (FAT, most network mounts). It is stored in the volume itself, so unlike
// st_dev it survives a reboot.
func readVolumeUUID(trap, target uintptr) string {
	attrs := unix.Attrlist{
		Bitmapcount: unix.ATTR_BIT_MAP_COUNT,
		Commonattr:  unix.ATTR_CMN_RETURNED_ATTRS,
		Volattr:     unix.ATTR_VOL_INFO | unix.ATTR_VOL_UUID,
	}
	// Four-byte length, twenty-byte returned-attribute set, sixteen-byte UUID.
	var buf [40]byte
	_, _, errno := unix.Syscall6(trap, target,
		uintptr(unsafe.Pointer(&attrs)), uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)), unix.FSOPT_PACK_INVAL_ATTRS, 0)
	if errno != 0 || binary.NativeEndian.Uint32(buf[0:4]) != uint32(len(buf)) ||
		binary.NativeEndian.Uint32(buf[8:12])&unix.ATTR_VOL_UUID == 0 {
		return ""
	}
	var uuid [16]byte
	copy(uuid[:], buf[24:40])
	if uuid == [16]byte{} {
		return ""
	}
	return fmt.Sprintf("%X", uuid[:])
}
