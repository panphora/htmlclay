//go:build !darwin && !linux && !windows

package platform

import "errors"

// selectFolder is a placeholder until the Linux (zenity/kdialog) and Windows
// (IFileDialog) folder pickers land in a later phase.
func selectFolder(prompt string) (string, bool, error) {
	return "", false, errors.New("native folder picker not implemented on this platform")
}
