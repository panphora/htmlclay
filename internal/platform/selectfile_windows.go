//go:build windows

package platform

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

func selectFile(prompt string) (string, bool, error) {
	// The filter is a convenience only. What may actually be registered is
	// decided once, in prepareHelperProgram, beside the Unix executable-bit
	// check, so the two platforms answer "can this file be run" in one place.
	const script = "$ErrorActionPreference = 'Stop'; " +
		"Add-Type -AssemblyName System.Windows.Forms; " +
		"$d = New-Object System.Windows.Forms.OpenFileDialog; " +
		"$d.Title = $env:HTMLCLAY_DIALOG_PROMPT; " +
		"$d.CheckFileExists = $true; $d.Multiselect = $false; " +
		"$d.Filter = 'Helper programs (*.exe;*.bat;*.cmd)|*.exe;*.bat;*.cmd|All files (*.*)|*.*'; " +
		"if ($d.ShowDialog() -eq 'OK') { Write-Output $d.FileName }"

	ctx, cancel := context.WithTimeout(context.Background(), selectFileTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-STA", "-Command", script)
	cmd.Env = append(cmd.Environ(), "HTMLCLAY_DIALOG_PROMPT="+prompt)
	out, err := cmd.Output()
	if err != nil {
		return "", false, fmt.Errorf("file picker failed: %w", err)
	}
	path := strings.Trim(string(out), "\r\n")
	if path == "" {
		return "", false, nil
	}
	return path, true, nil
}
