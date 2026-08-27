//go:build windows

package platform

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// selectFolder shows a FolderBrowserDialog via PowerShell. On OK it prints the
// selected path and yields ok=true; a cancel prints nothing and exits zero, so an
// empty output is a clean cancel (ok=false, no error); a non-zero exit is a genuine
// launch failure and is surfaced as an error rather than a silent cancel.
//
// The prompt is passed as an environment variable and referenced with $env:VAR
// inside the script, never spliced into the script text (see confirmDialog for the
// PowerShell quoting hazard this avoids).
func selectFolder(prompt string) (string, bool, error) {
	const script = "$ErrorActionPreference = 'Stop'; " +
		"Add-Type -AssemblyName System.Windows.Forms; " +
		"$d = New-Object System.Windows.Forms.FolderBrowserDialog; " +
		"$d.Description = $env:HTMLCLAY_DIALOG_PROMPT; " +
		"if ($d.ShowDialog() -eq 'OK') { Write-Output $d.SelectedPath }"

	ctx, cancel := context.WithTimeout(context.Background(), pickerTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.Env = append(cmd.Environ(), "HTMLCLAY_DIALOG_PROMPT="+prompt)
	out, err := cmd.Output()
	if err != nil {
		return "", false, fmt.Errorf("folder picker failed: %w", err)
	}
	// Trim only line endings, so a folder name with legitimate trailing spaces is
	// preserved rather than silently pointing at a different folder.
	dir := strings.Trim(string(out), "\r\n")
	if dir == "" {
		return "", false, nil
	}
	return dir, true, nil
}
