package platform

import "os"

// OpenShared opens path read-only for a handle the caller intends to HOLD, without
// preventing anyone else from DELETING the file underneath it.
//
// This exists because os.Open is the wrong tool for a long-lived handle on Windows.
// Windows only permits a delete if every open handle to the file allowed it up front
// via FILE_SHARE_DELETE, and Go's os.Open asks for read and write sharing but not
// delete. So a handle kept open for bookkeeping silently becomes a lock on removing
// the file. Unix has no such rule, so there this is plain os.Open.
//
// KNOWN LIMIT, measured on the Windows CI runner rather than assumed: this buys
// delete, NOT rename-over. MoveFileEx with MOVEFILE_REPLACE_EXISTING is still
// refused while any handle is open, whatever sharing mode it asked for, so a
// held handle still breaks htmlclay's atomic save. openshared_windows_test.go pins
// both halves of that. The only fix for the save path is to stop holding a handle
// on a file we intend to replace; sharing mode cannot rescue it.
func OpenShared(path string) (*os.File, error) {
	return openShared(path)
}
