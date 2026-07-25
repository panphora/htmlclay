//go:build linux

package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// selectFolder shows a native directory picker via zenity, falling back to
// kdialog. A user cancel exits with status 1 and is reported as ok=false with no
// error; any other non-zero exit (no display, crash, timeout) is a genuine failure
// and is surfaced as an error rather than a silent cancel. Only a missing tool
// fails closed with an error.
func selectFolder(prompt string) (string, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), pickerTimeout)
	defer cancel()

	if bin, err := exec.LookPath("zenity"); err == nil {
		out, err := exec.CommandContext(ctx, bin, "--file-selection", "--directory", "--title", prompt).Output()
		if err != nil {
			if cancelled(err) {
				return "", false, nil
			}
			return "", false, fmt.Errorf("zenity folder picker failed: %w", err)
		}
		if dir := pickedDir(out); dir != "" {
			return dir, true, nil
		}
		return "", false, nil
	}

	if bin, err := exec.LookPath("kdialog"); err == nil {
		home, _ := os.UserHomeDir()
		out, err := exec.CommandContext(ctx, bin, "--title", prompt, "--getexistingdirectory", home).Output()
		if err != nil {
			if cancelled(err) {
				return "", false, nil
			}
			return "", false, fmt.Errorf("kdialog folder picker failed: %w", err)
		}
		if dir := pickedDir(out); dir != "" {
			return dir, true, nil
		}
		return "", false, nil
	}

	return "", false, errors.New("no native folder picker (zenity or kdialog) found")
}

// cancelled reports whether err is a clean user cancel (exit status 1), as opposed
// to a genuine launch failure. zenity and kdialog both exit 1 when the user
// dismisses the picker.
func cancelled(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == 1
}

// pickedDir trims only line endings from the picker output, so a folder name with
// legitimate trailing spaces is preserved rather than pointing at a different one.
func pickedDir(out []byte) string {
	return strings.Trim(string(out), "\n")
}
