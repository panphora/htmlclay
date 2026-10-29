//go:build darwin

package browser

import "os/exec"

func OpenURL(url string) error {
	return exec.Command("open", url).Run()
}

// Reveal shows path in the Finder with the file itself selected.
func Reveal(path string) error {
	return exec.Command("open", "-R", path).Run()
}
