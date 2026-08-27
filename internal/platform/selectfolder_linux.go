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

// selectFolder shows a native directory picker: the XDG desktop portal first,
// then zenity, then kdialog. The portal needs no helper binary and is uniform
// across GNOME, KDE, Wayland and Flatpak, so it leads; the helper tools remain
// for a desktop with no portal service running. The fallback happens only when
// the portal is unavailable: once a portal dialog could have opened, its errors
// are surfaced rather than stacking a second picker on the first.
//
// In every backend a user cancel is reported as ok=false with no error; a
// failure after the dialog existed (no display, crash, timeout) is surfaced as
// an error rather than a silent cancel. With no portal and neither tool it
// fails closed with an error.
func selectFolder(prompt string) (string, bool, error) {
	dir, ok, err := portalSelectFolder(prompt)
	if err == nil {
		return dir, ok, nil
	}
	if !errors.Is(err, errPortalUnavailable) {
		return "", false, err
	}

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

	return "", false, errors.New("no native folder picker found (no desktop portal, and neither zenity nor kdialog is installed)")
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
