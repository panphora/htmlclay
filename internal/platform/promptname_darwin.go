//go:build darwin

package platform

import (
	"context"
	"os/exec"
	"strings"
)

// The foreground activation is load-bearing for dialogs started by the tray,
// whose process is normally behind whichever application the user is in.
func promptName(title, message, initial string) (string, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), promptNameTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "osascript", "-e", "activate me", "-e", promptNameScript(title, message, initial)).CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "-128") {
			return "", false, nil
		}
		return "", false, err
	}
	return strings.Trim(string(out), "\n"), true, nil
}

func promptNameScript(title, message, initial string) string {
	return "text returned of (display dialog " + appleScriptString(message) +
		" with title " + appleScriptString(title) +
		" default answer " + appleScriptString(initial) +
		` buttons {"Cancel", "OK"} default button "OK" cancel button "Cancel")`
}
