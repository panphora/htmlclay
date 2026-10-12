package platform

// PortOwner names the process listening on a loopback TCP port: its pid and a short
// program name. Only listeners that conflict with a bind on 127.0.0.1 or ::1 count:
// the loopback addresses themselves and the wildcards. A listener on a LAN address
// with the same port number is not the conflict. ok is false when nothing could be
// learned: a platform without a lookup, a missing tool, or a port in a Windows
// exclusion range, which has no owner at all.
func PortOwner(port int) (pid int, name string, ok bool) {
	return portOwner(port)
}

// QuitProcess asks pid to exit. With force false it is a polite request (SIGTERM, or
// WM_CLOSE through taskkill); with force true it is a kill. The caller re-identifies the
// process first and waits afterwards; this reports only whether the request was delivered.
func QuitProcess(pid int, force bool) error {
	return quitProcess(pid, force)
}
