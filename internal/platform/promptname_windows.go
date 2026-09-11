//go:build windows

package platform

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

func promptName(title, message, initial string) (string, bool, error) {
	const script = "$ErrorActionPreference = 'Stop'; " +
		"Add-Type -AssemblyName System.Windows.Forms; " +
		"Add-Type -AssemblyName System.Drawing; " +
		"$f = New-Object System.Windows.Forms.Form; " +
		"$f.Text = $env:HTMLCLAY_DIALOG_TITLE; " +
		"$f.FormBorderStyle = 'FixedDialog'; $f.MaximizeBox = $false; $f.MinimizeBox = $false; " +
		"$f.StartPosition = 'CenterScreen'; $f.TopMost = $true; " +
		"$f.ClientSize = New-Object System.Drawing.Size(460, 150); " +
		"$l = New-Object System.Windows.Forms.Label; $l.Text = $env:HTMLCLAY_DIALOG_MESSAGE; " +
		"$l.SetBounds(16, 16, 428, 42); $f.Controls.Add($l); " +
		"$box = New-Object System.Windows.Forms.TextBox; $box.Text = $env:HTMLCLAY_DIALOG_INITIAL; " +
		"$box.SetBounds(16, 62, 428, 24); $f.Controls.Add($box); " +
		"$cancel = New-Object System.Windows.Forms.Button; $cancel.Text = 'Cancel'; " +
		"$cancel.SetBounds(244, 102, 96, 32); $cancel.DialogResult = [System.Windows.Forms.DialogResult]::Cancel; " +
		"$ok = New-Object System.Windows.Forms.Button; $ok.Text = 'OK'; " +
		"$ok.SetBounds(348, 102, 96, 32); $ok.DialogResult = [System.Windows.Forms.DialogResult]::OK; " +
		"$f.Controls.AddRange(@($cancel, $ok)); $f.AcceptButton = $ok; $f.CancelButton = $cancel; " +
		"$f.ActiveControl = $box; $box.SelectAll(); " +
		"if ($f.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) { Write-Output ('OK:' + $box.Text) }"

	ctx, cancel := context.WithTimeout(context.Background(), promptNameTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-STA", "-Command", script)
	cmd.Env = append(cmd.Environ(),
		"HTMLCLAY_DIALOG_TITLE="+title,
		"HTMLCLAY_DIALOG_MESSAGE="+message,
		"HTMLCLAY_DIALOG_INITIAL="+initial,
	)
	out, err := cmd.Output()
	if err != nil {
		return "", false, fmt.Errorf("name prompt failed: %w", err)
	}
	return promptNameResult(string(out))
}

func promptNameResult(out string) (string, bool, error) {
	line := strings.Trim(string(out), "\r\n")
	if line == "" {
		return "", false, nil
	}
	if !strings.HasPrefix(line, "OK:") {
		return "", false, fmt.Errorf("name prompt returned an unexpected result")
	}
	return strings.TrimPrefix(line, "OK:"), true, nil
}
