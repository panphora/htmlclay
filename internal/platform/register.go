package platform

// RegisterFileTypes makes the operating system open .htmlclay documents with
// the binary at exePath, and offers HTML Clay in the Open With list for plain
// .html and .htm without touching whichever browser owns them. It is idempotent
// and safe to call on every launch, which is how a binary the user moved keeps a
// working association.
//
// It is best effort by contract: the error is for the log, never for the exit
// code. The launch most likely to be running this is a double-click on a file,
// and refusing to open that file because a registry write failed would trade a
// cosmetic problem for the whole product.
func RegisterFileTypes(exePath string) error {
	return registerFileTypes(exePath)
}

// UnregisterFileTypes removes everything RegisterFileTypes wrote. It exists
// because there is otherwise no way back out short of editing the registry by
// hand: HTML Clay ships as a binary, not an installer, so uninstalling is
// deleting the file, and a deleted file leaves its associations behind.
func UnregisterFileTypes() error {
	return unregisterFileTypes()
}
