//go:build linux

package platform

import "os/exec"

// notify uses notify-send (libnotify), present on essentially every GTK/Qt
// desktop. If it is missing the error is returned and the caller logs instead.
func notify(title, message string) error {
	return exec.Command("notify-send", "--urgency=critical", title, message).Run()
}
