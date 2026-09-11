//go:build windows

package platform

import (
	"context"
	"fmt"
	"os/exec"
)

func manageProgram(p ProgramSummary) (ManageChoice, error) {
	const script = "$ErrorActionPreference = 'Stop'; " +
		"Add-Type -AssemblyName System.Windows.Forms; " +
		"Add-Type -AssemblyName System.Drawing; " +
		"$f = New-Object System.Windows.Forms.Form; " +
		"$f.Text = $env:HTMLCLAY_DIALOG_TITLE; " +
		"$f.FormBorderStyle = 'FixedDialog'; $f.MaximizeBox = $false; $f.MinimizeBox = $false; " +
		"$f.StartPosition = 'CenterScreen'; $f.TopMost = $true; " +
		"$f.ClientSize = New-Object System.Drawing.Size(520, 300); " +
		"$summary = New-Object System.Windows.Forms.Label; $summary.Text = $env:HTMLCLAY_DIALOG_MESSAGE; " +
		"$summary.SetBounds(16, 16, 488, 116); $summary.AutoEllipsis = $true; $f.Controls.Add($summary); " +
		"$list = New-Object System.Windows.Forms.ListBox; $list.SetBounds(16, 140, 488, 82); " +
		"[void]$list.Items.Add($env:HTMLCLAY_DIALOG_TOGGLE); " +
		"[void]$list.Items.Add('Forget document permissions'); [void]$list.Items.Add('Remove program'); " +
		"$list.SelectedIndex = -1; $f.Controls.Add($list); " +
		"$cancel = New-Object System.Windows.Forms.Button; $cancel.Text = 'Cancel'; " +
		"$cancel.SetBounds(304, 246, 96, 34); $cancel.DialogResult = [System.Windows.Forms.DialogResult]::Cancel; " +
		"$apply = New-Object System.Windows.Forms.Button; $apply.Text = 'Apply'; " +
		"$apply.SetBounds(408, 246, 96, 34); $apply.DialogResult = [System.Windows.Forms.DialogResult]::OK; " +
		"$f.Controls.AddRange(@($cancel, $apply)); $f.AcceptButton = $apply; $f.CancelButton = $cancel; " +
		"$r = $f.ShowDialog(); if ($r -eq [System.Windows.Forms.DialogResult]::OK) { " +
		"switch ($list.SelectedIndex) { 0 { Write-Output 'toggle' } 1 { Write-Output 'forget' } 2 { Write-Output 'remove' } } }"

	ctx, cancel := context.WithTimeout(context.Background(), manageProgramTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-STA", "-Command", script)
	cmd.Env = append(cmd.Environ(),
		"HTMLCLAY_DIALOG_TITLE=Manage "+p.Name,
		"HTMLCLAY_DIALOG_MESSAGE="+manageProgramMessage(p),
		"HTMLCLAY_DIALOG_TOGGLE="+manageToggleLabel(p.AnyDocument),
	)
	out, err := cmd.Output()
	if err != nil {
		return ManageCancel, fmt.Errorf("program management dialog failed: %w", err)
	}
	choice, ok := manageChoiceFromResult(string(out), p)
	if !ok {
		return ManageCancel, fmt.Errorf("program management dialog returned an unexpected result")
	}
	return choice, nil
}
