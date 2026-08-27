//go:build darwin

package browser

import "os/exec"

func OpenURL(url string) error {
	return exec.Command("open", url).Run()
}
