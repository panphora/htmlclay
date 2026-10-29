//go:build linux

package browser

import (
	"net/url"
	"os/exec"
	"path/filepath"
)

func OpenURL(url string) error {
	return exec.Command("xdg-open", url).Run()
}

// Reveal asks the freedesktop file manager to show path, selected, and falls back
// to opening its folder when no file manager answers that call.
func Reveal(path string) error {
	item := (&url.URL{Scheme: "file", Path: path}).String()
	if err := exec.Command("dbus-send", "--session", "--dest=org.freedesktop.FileManager1", "--type=method_call", "/org/freedesktop/FileManager1", "org.freedesktop.FileManager1.ShowItems", "array:string:"+item, "string:").Run(); err == nil {
		return nil
	}
	return OpenURL(filepath.Dir(path))
}
