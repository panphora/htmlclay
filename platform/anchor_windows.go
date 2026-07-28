//go:build windows

package platform

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// fileIDInfo mirrors FILE_ID_INFO. x/sys declares the info class but not the
// struct, so it is spelled out here.
type fileIDInfo struct {
	volumeSerialNumber uint64
	fileID             [16]byte
}

// FILE_ID_INFO is 24 bytes: a ULONGLONG volume serial plus a 16-byte FILE_ID_128.
// A wrong size makes GetFileInformationByHandleEx fail and drops us silently to
// the 64-bit fallback, so pin it where the compiler will catch it.
var _ [24]byte = [unsafe.Sizeof(fileIDInfo{})]byte{}

// identity on Windows is a plain value and holds nothing open, because a held
// handle would veto the rename-over that every htmlclay save performs.
//
// wide records WHICH id this is. FILE_ID_INFO gives 128 bits and is what we want:
// on NTFS the low half is the file reference number, whose top 16 bits are an MFT
// sequence number that increments whenever a record is reused, so a recycled
// record cannot pass as the file we anchored. Where that info class is not
// available the older 64-bit file index is used instead, which is what Go's
// os.SameFile reads and is unique on NTFS but not on ReFS. Anchors of different
// kinds never compare equal: an ambiguous comparison should roll the generation,
// which costs a replay buffer, rather than pass, which serves a dead document.
type identity struct {
	volume uint64
	id     [16]byte
	wide   bool
}

func readIdentity(path string) (identity, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return identity{}, &os.PathError{Op: "anchor", Path: path, Err: err}
	}
	// FILE_READ_ATTRIBUTES is the least access that can answer the question, and
	// all three sharing modes keep even this momentary open from blocking anyone.
	// Leaving FILE_FLAG_BACKUP_SEMANTICS out is deliberate: without it CreateFile
	// refuses to open a directory. That covers less than the Unix half's explicit
	// IsRegular check, which also rejects devices and pipes, but anything of that
	// kind that does open answers neither info call, so it ends as no anchor rather
	// than as a wrong one.
	h, err := windows.CreateFile(
		p,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return identity{}, &os.PathError{Op: "anchor", Path: path, Err: err}
	}
	defer windows.CloseHandle(h)

	var wide fileIDInfo
	if err := windows.GetFileInformationByHandleEx(
		h,
		windows.FileIdInfo,
		(*byte)(unsafe.Pointer(&wide)),
		uint32(unsafe.Sizeof(wide)),
	); err == nil && usableID(wide.fileID[:]) {
		return identity{volume: wide.volumeSerialNumber, id: wide.fileID, wide: true}, nil
	}

	var basic windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &basic); err != nil {
		return identity{}, &os.PathError{Op: "anchor", Path: path, Err: err}
	}
	index := uint64(basic.FileIndexHigh)<<32 | uint64(basic.FileIndexLow)
	id := identity{volume: uint64(basic.VolumeSerialNumber)}
	for i := 0; i < 8; i++ {
		id.id[i] = byte(index >> (8 * i))
	}
	if !usableID(id.id[:8]) {
		return identity{}, &os.PathError{Op: "anchor", Path: path, Err: windows.ERROR_NOT_SUPPORTED}
	}
	return id, nil
}

// usableID rejects the two sentinels that mean "this filesystem cannot identify
// files", rather than naming one. A filesystem with no 128-bit id answers the
// FileIdInfo query successfully and fills in zeros, and all-ones is the invalid
// value in the 128-bit file id protocol. Taking either at face value would make
// every file on such a volume share one identity, so a replaced document would
// pass as the one we anchored, which is the exact failure this package exists to
// prevent. Returning no anchor at all is the safe answer.
func usableID(id []byte) bool {
	var zero, ones byte = 0x00, 0xff
	for _, b := range id {
		zero |= b
		ones &= b
	}
	return zero != 0x00 && ones != 0xff
}

func (id *identity) same(other identity) bool {
	return id.wide == other.wide && id.volume == other.volume && id.id == other.id
}

// close is a no-op. The handle was released before the anchor existed, which is
// the entire reason this half of the seam exists.
func (id *identity) close() {}
