//go:build linux

package platform

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

func promptName(title, message, initial string) (string, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), promptNameTimeout)
	defer cancel()

	if bin, err := exec.LookPath("zenity"); err == nil {
		out, err := exec.CommandContext(ctx, bin,
			"--entry",
			"--title", title,
			"--text", message,
			"--entry-text", initial,
		).Output()
		if err != nil {
			if cancelled(err) {
				return "", false, nil
			}
			return "", false, fmt.Errorf("zenity name prompt failed: %w", err)
		}
		return strings.Trim(string(out), "\n"), true, nil
	}

	if bin, err := exec.LookPath("kdialog"); err == nil {
		out, err := exec.CommandContext(ctx, bin, "--title", title, "--inputbox", message, initial).Output()
		if err != nil {
			if cancelled(err) {
				return "", false, nil
			}
			return "", false, fmt.Errorf("kdialog name prompt failed: %w", err)
		}
		return strings.Trim(string(out), "\n"), true, nil
	}

	return "", false, errors.New("no native name prompt found (neither zenity nor kdialog is installed)")
}
