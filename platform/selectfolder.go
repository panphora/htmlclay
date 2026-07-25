package platform

// SelectFolder shows a native folder-picker dialog and returns the chosen path.
// ok is false when the user cancels, which is a normal outcome rather than an
// error. On an unsupported platform it returns an error. The returned path is
// whatever the OS picker yields; the caller canonicalizes and validates it before
// trusting it.
func SelectFolder(prompt string) (path string, ok bool, err error) {
	return selectFolder(prompt)
}
