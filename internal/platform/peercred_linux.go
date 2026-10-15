package platform

import (
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// peerUIDIsSelf reports whether the peer of a Unix socket connection runs as
// this process's user, from the kernel's record of who connected.
func peerUIDIsSelf(c net.Conn) bool {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return false
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return false
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil || credErr != nil {
		return false
	}
	return int(cred.Uid) == os.Getuid()
}
