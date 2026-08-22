//go:build !windows

package platform

// Only Windows keeps file associations inside the running program. macOS reads
// them from the .app bundle's Info.plist and Linux from the freedesktop MIME
// database that install.sh writes and uninstall.sh removes, so on both there is
// nothing for a launched binary to register and nothing for it to undo.
func registerFileTypes(exePath string) error { return nil }

func unregisterFileTypes() error { return nil }
