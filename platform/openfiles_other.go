//go:build !darwin

package platform

// OnOpenFile is a no-op on platforms where the OS delivers opened file paths via
// argv (Linux, Windows); those are handled through the normal argv + single-
// instance forwarding path.
func OnOpenFile(cb func(string)) {}
