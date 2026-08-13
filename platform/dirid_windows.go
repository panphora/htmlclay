//go:build windows

package platform

// DirIdentity has no cheap persistent equivalent on Windows: FileInfo carries
// no file index there, matching the watcher's fileIdentity fallback. The empty
// fingerprint means the stored path alone is the entry's identity, and the
// identity comparison at install time is skipped.
func DirIdentity(path string) string {
	return ""
}
