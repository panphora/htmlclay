//go:build windows

package main

import (
	"errors"

	"golang.org/x/sys/windows"
)

func ipv6Unavailable(err error) bool {
	return errors.Is(err, windows.WSAEADDRNOTAVAIL) || errors.Is(err, windows.WSAEAFNOSUPPORT)
}
