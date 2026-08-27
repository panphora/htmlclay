//go:build windows

package platform

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// DirIdentity returns a volume+file-id fingerprint for the directory at path,
// or "" when one cannot be derived. It is the directory counterpart of the file
// anchor's readIdentity, and it deliberately reuses that function's decisions:
// FILE_READ_ATTRIBUTES is the least access that can answer the question, all
// three sharing modes keep the momentary open from blocking anyone, and the
// handle is closed before this returns, so the fingerprint never pins the
// directory against the rename or delete the identity check exists to notice.
// os.OpenRoot would hold exactly that pin, which is why it is not used here.
//
// The one inversion from readIdentity is FILE_FLAG_BACKUP_SEMANTICS. There its
// absence is a cheap directory filter, because CreateFile without it refuses to
// open a directory; here directories are the point, so the flag is required.
// Do not unify the two calls.
//
// CreateFile follows symlinks and junctions, matching dirid_unix's deliberate
// os.Stat: a stored trusted-folder path whose directory was later swapped for a
// link resolves to the target, whose fingerprint differs from the recorded one,
// which is exactly the swap the comparison exists to catch.
//
// The string is volume:id, the same shape as the Unix side's dev:ino. The id is
// fixed-width hex: 32 characters for the 128-bit FILE_ID_INFO, 16 for the
// 64-bit fallback index, so the two kinds can never compare equal, mirroring
// readIdentity's rule that anchors of different kinds never match. usableID
// rejects the all-zero and all-one sentinels for the same reason it does there:
// on a filesystem that cannot identify files, every directory would share one
// fingerprint, and "" (path-only identity) is the safe answer instead.
func DirIdentity(path string) string {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return ""
	}
	h, err := windows.CreateFile(
		p,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)

	var wide fileIDInfo
	if err := windows.GetFileInformationByHandleEx(
		h,
		windows.FileIdInfo,
		(*byte)(unsafe.Pointer(&wide)),
		uint32(unsafe.Sizeof(wide)),
	); err == nil && usableID(wide.fileID[:]) {
		return fmt.Sprintf("%d:%x", wide.volumeSerialNumber, wide.fileID)
	}

	var basic windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &basic); err != nil {
		return ""
	}
	index := uint64(basic.FileIndexHigh)<<32 | uint64(basic.FileIndexLow)
	var id [8]byte
	for i := range id {
		id[i] = byte(index >> (8 * i))
	}
	if !usableID(id[:]) {
		return ""
	}
	return fmt.Sprintf("%d:%x", uint64(basic.VolumeSerialNumber), id)
}
