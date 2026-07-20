package server

import "os"

// fileIdentity has no cheap equivalent on Windows: Win32FileAttributeData carries
// no file index, so the watcher falls back to size and modtime there.
func fileIdentity(info os.FileInfo) string {
	return ""
}
