package platform

import "os"

// OpenShared opens path read-only for a handle the caller intends to HOLD, without
// preventing anyone else from renaming or deleting the file underneath it.
//
// This exists because os.Open is the wrong tool for a long-lived handle on Windows.
// Windows only permits a rename or delete if every open handle to the file allowed
// it up front via FILE_SHARE_DELETE, and Go's os.Open asks for read and write
// sharing but not delete. So a handle kept open for bookkeeping silently becomes a
// lock: htmlclay's own atomic save, which writes a temp file and renames it over
// the target, fails with "Access is denied", and the user is told their save did
// not work. Unix has no such rule, so there the distinction does not exist and this
// is plain os.Open.
//
// Use this for any handle that outlives a single read. A brief open-read-close does
// not need it, though it can still collide; the atomic replace retries to cover that.
func OpenShared(path string) (*os.File, error) {
	return openShared(path)
}
