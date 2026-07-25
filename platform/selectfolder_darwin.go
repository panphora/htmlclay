//go:build darwin

package platform

import (
	"os/exec"
	"strings"
)

// selectFolder shows the macOS "choose folder" panel via osascript. "activate me"
// brings it to the foreground the same way confirmDialog does, so a panel spawned
// from the background tray process is not lost behind the active window. A user
// cancel makes osascript exit non-zero with "User canceled. (-128)"; that is a
// normal outcome, reported as ok=false with no error.
func selectFolder(prompt string) (string, bool, error) {
	script := "POSIX path of (choose folder with prompt " + appleScriptString(prompt) + ")"
	out, err := exec.Command("osascript", "-e", "activate me", "-e", script).CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "-128") {
			return "", false, nil
		}
		return "", false, err
	}
	// Trim only line endings, so a folder name with legitimate trailing spaces is
	// preserved rather than silently pointing at a different folder.
	return strings.Trim(string(out), "\n"), true, nil
}
