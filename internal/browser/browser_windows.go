//go:build windows

package browser

import "os/exec"

func OpenURL(url string) error {
	return exec.Command("cmd", "/c", "start", "", url).Run()
}
