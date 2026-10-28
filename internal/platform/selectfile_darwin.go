//go:build darwin

package platform

import (
	"context"
	"os/exec"
	"strings"
)

// "activate me" is required because this runs from the background tray
// process. Without it the picker can appear behind the active window.
func selectFile(prompt, dir string) (string, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), selectFileTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "osascript", "-e", "activate me", "-e", selectFileScript(prompt, dir)).CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "-128") {
			return "", false, nil
		}
		return "", false, err
	}
	// Trimming only the line ending preserves a legitimate trailing space in a
	// filename instead of silently selecting a different path.
	return strings.Trim(string(out), "\n"), true, nil
}

func selectFileScript(prompt, dir string) string {
	script := "choose file with prompt " + appleScriptString(prompt)
	if dir != "" {
		script += " default location (POSIX file " + appleScriptString(dir) + ")"
	}
	return "POSIX path of (" + script + ")"
}
