//go:build !windows

package main

import (
	"errors"
	"syscall"
)

// ipv6Unavailable reports a bind on ::1 that failed because this machine has no IPv6
// loopback at all, as opposed to one that failed because something else holds the port.
func ipv6Unavailable(err error) bool {
	return errors.Is(err, syscall.EADDRNOTAVAIL) || errors.Is(err, syscall.EAFNOSUPPORT)
}
