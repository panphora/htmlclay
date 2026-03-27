//go:build linux

package browser

import "os/exec"

func OpenURL(url string) error {
	return exec.Command("xdg-open", url).Run()
}
