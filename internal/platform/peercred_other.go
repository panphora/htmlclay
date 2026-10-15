//go:build !windows && !darwin && !linux

package platform

import "net"

// peerUIDIsSelf cannot read the peer's user here, so it refuses every command.
func peerUIDIsSelf(net.Conn) bool { return false }
