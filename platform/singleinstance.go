package platform

import (
	"io"
	"net"
	"time"
)

type SingleInstance interface {
	TryLock() (bool, error)
	SendFilePath(path string) error
	OnFileReceived(callback func(path string))
	Unlock() error
}

// handshakeBanner is sent by the primary instance to every connecting peer.
// A forwarding instance verifies it before writing a file path, so a stale
// addr/socket pointing at an unrelated local listener never receives the path.
const handshakeBanner = "HTMLCLAY-SINGLE-INSTANCE-1\n"

func writeBanner(c net.Conn) {
	c.SetWriteDeadline(time.Now().Add(2 * time.Second))
	c.Write([]byte(handshakeBanner))
}

func verifyBanner(c net.Conn) bool {
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, len(handshakeBanner))
	if _, err := io.ReadFull(c, buf); err != nil {
		return false
	}
	return string(buf) == handshakeBanner
}
