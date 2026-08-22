//go:build windows

package platform

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

// dialogTimeout and pickerTimeout bound how long native prompts may block. On expiry
// the helper process is killed and the outcome fails closed (confirm to Deny, picker
// to error), so a wedged or ignored dialog cannot hang the flow. dialogTimeout is set
// near the broker's 120s park ceiling (see confirm_darwin.go's "giving up after 120")
// so the dialog does not routinely outlive the request that raised it; it does NOT by
// itself guarantee the parked request is still alive when the user answers, the
// broker's waiter lifecycle owns that. The folder picker races nothing (it is driven
// from the tray with no request pending), so its deadline is generous and only stops
// a wedged helper from leaking forever.
const (
	dialogTimeout = 120 * time.Second
	pickerTimeout = 5 * time.Minute
)

// confirmDialog shows a three-button permission prompt via a PowerShell WinForms
// window. MessageBox cannot relabel its buttons, so until 1.4.0 this was a
// YesNoCancel box where only Yes meant anything and ConfirmTrustFolder was
// unreachable: the prompt text described three outcomes and Windows could deliver
// two. A custom Form can carry three labelled buttons, so the durable choice is
// now offered here as it is on macOS.
//
// It falls back to the old message box when the form fails for any reason. That
// path is exercised by nothing automated, because no test can click a native
// dialog, so a script that breaks on some Windows build degrades to the previous
// behaviour rather than to Deny on every read prompt.
//
// The button clicked comes back as a bare word on stdout: TRUST, ALLOW, or
// anything else (window closed, timeout, error) meaning deny. Escape and the
// window's close box both land on the deny button, so the dialog fails closed.
//
// The title and message are passed as environment variables and referenced with
// $env:VAR inside the script, never spliced into the script text. PowerShell's
// lexer treats several Unicode code points (U+2018/2019/201A/201B) as single-quote
// delimiters, so escaping untrusted text into a quoted literal is not safe; an env
// var is pure data and immune to every quoting trick.
func confirmDialog(title, message string) (ConfirmChoice, error) {
	// Each button carries a DialogResult, so WinForms closes the window and
	// ShowDialog returns the answer with no event handlers, no closures, and no
	// scope tricks. That matters more than usual here: nothing in CI can click this
	// window, so the script has to be the plainest thing that works.
	//
	// CancelButton makes Escape and the window's close box both return Cancel, which
	// is Deny. AcceptButton is deliberately unset, so Enter selects nothing.
	const script = "$ErrorActionPreference = 'Stop'; " +
		"Add-Type -AssemblyName System.Windows.Forms; " +
		"Add-Type -AssemblyName System.Drawing; " +
		"$f = New-Object System.Windows.Forms.Form; " +
		"$f.Text = $env:HTMLCLAY_DIALOG_TITLE; " +
		"$f.FormBorderStyle = 'FixedDialog'; $f.MaximizeBox = $false; $f.MinimizeBox = $false; " +
		"$f.StartPosition = 'CenterScreen'; $f.TopMost = $true; " +
		"$f.ClientSize = New-Object System.Drawing.Size(480, 212); " +
		"$l = New-Object System.Windows.Forms.Label; " +
		"$l.Text = $env:HTMLCLAY_DIALOG_MESSAGE; " +
		"$l.SetBounds(16, 16, 448, 132); " +
		"$f.Controls.Add($l); " +
		"$deny = New-Object System.Windows.Forms.Button; " +
		"$deny.Text = 'Deny'; $deny.SetBounds(16, 162, 140, 32); " +
		"$deny.DialogResult = [System.Windows.Forms.DialogResult]::Cancel; " +
		"$allow = New-Object System.Windows.Forms.Button; " +
		"$allow.Text = 'Allow Once'; $allow.SetBounds(166, 162, 140, 32); " +
		"$allow.DialogResult = [System.Windows.Forms.DialogResult]::OK; " +
		"$trust = New-Object System.Windows.Forms.Button; " +
		"$trust.Text = 'Trust This Folder'; $trust.SetBounds(316, 162, 148, 32); " +
		"$trust.DialogResult = [System.Windows.Forms.DialogResult]::Yes; " +
		"$f.Controls.AddRange(@($deny, $allow, $trust)); " +
		"$f.CancelButton = $deny; " +
		"Write-Output $f.ShowDialog()"

	ctx, cancel := context.WithTimeout(context.Background(), dialogTimeout)
	defer cancel()

	// -STA because WinForms needs a single-threaded apartment. powershell.exe
	// defaults to it, but pwsh does not, and being explicit costs nothing.
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-STA", "-Command", script)
	cmd.Env = append(os.Environ(),
		"HTMLCLAY_DIALOG_TITLE="+title,
		"HTMLCLAY_DIALOG_MESSAGE="+message,
	)
	out, err := cmd.Output()
	if err != nil {
		return confirmDialogMessageBox(title, message)
	}
	return choiceFromDialogResult(string(out)), nil
}

// choiceFromDialogResult maps what the form printed onto a choice. Anything
// unrecognized is Deny, so a truncated or unexpected answer fails closed.
func choiceFromDialogResult(out string) ConfirmChoice {
	switch strings.TrimSpace(out) {
	case "Yes":
		return ConfirmTrustFolder
	case "OK":
		return ConfirmAllowOnce
	}
	return ConfirmDeny
}

// confirmDialogMessageBox is the pre-1.4.0 prompt, kept as the fallback for a
// machine where the custom form will not run. It offers two outcomes: Yes maps to
// ConfirmAllowOnce, and No, Cancel, timeout and any error all fail closed.
func confirmDialogMessageBox(title, message string) (ConfirmChoice, error) {
	const script = "$ErrorActionPreference = 'Stop'; " +
		"Add-Type -AssemblyName System.Windows.Forms; " +
		"$r = [System.Windows.Forms.MessageBox]::Show(" +
		"$env:HTMLCLAY_DIALOG_MESSAGE, $env:HTMLCLAY_DIALOG_TITLE, " +
		"'YesNoCancel', 'Warning'); Write-Output $r"

	ctx, cancel := context.WithTimeout(context.Background(), dialogTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.Env = append(os.Environ(),
		"HTMLCLAY_DIALOG_TITLE="+title,
		"HTMLCLAY_DIALOG_MESSAGE="+message,
	)
	out, err := cmd.Output()
	if err != nil {
		return ConfirmDeny, nil
	}
	if strings.TrimSpace(string(out)) == "Yes" {
		return ConfirmAllowOnce, nil
	}
	return ConfirmDeny, nil
}

// confirmTwoButtons shows a YesNo WinForms message box. MessageBox cannot
// relabel its buttons, so the affirmative label is folded into the message text
// and Yes stands in for it — wording degrades, the two-choice shape does not.
// No, close, timeout, and any error all fail closed. Text passes through env
// vars for the same quoting-immunity reason as confirmDialog above.
func confirmTwoButtons(title, message, allowLabel string) (bool, error) {
	const script = "$ErrorActionPreference = 'Stop'; " +
		"Add-Type -AssemblyName System.Windows.Forms; " +
		"$r = [System.Windows.Forms.MessageBox]::Show(" +
		"$env:HTMLCLAY_DIALOG_MESSAGE, $env:HTMLCLAY_DIALOG_TITLE, " +
		"'YesNo', 'Warning'); Write-Output $r"

	ctx, cancel := context.WithTimeout(context.Background(), dialogTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.Env = append(os.Environ(),
		"HTMLCLAY_DIALOG_TITLE="+title,
		"HTMLCLAY_DIALOG_MESSAGE="+message+"\n\nYes = "+allowLabel+"\nNo = Deny",
	)
	out, err := cmd.Output()
	if err != nil {
		return false, nil
	}
	return strings.TrimSpace(string(out)) == "Yes", nil
}

// missingDialogAdvice always answers "nothing is missing": the prompt is a
// PowerShell WinForms window, and PowerShell ships with Windows.
func missingDialogAdvice() string { return "" }
