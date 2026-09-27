//go:build windows

package platform

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// dialogTimeout and pickerTimeout bound how long native prompts may block. On expiry
// the helper process is killed and the outcome fails closed (confirm to no answer,
// picker to error), so a wedged or ignored dialog cannot hang the flow. dialogTimeout is set
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
// YesNoCancel box where only Yes meant anything and ConfirmAllowAlways was
// unreachable: the prompt text described three outcomes and Windows could deliver
// two. A custom Form can carry three labelled buttons, so the durable choice is
// now offered here as it is on macOS.
//
// It falls back to the old message box when the form fails for any reason other
// than the deadline. That path is exercised by nothing automated, because no test
// can click a native dialog, so a script that breaks on some Windows build
// degrades to the previous behaviour rather than to no answer on every prompt. A
// form that simply timed out was ignored, and raising a second dialog for
// another two minutes would only be ignored again.
//
// The title, the message and every caller label are passed as environment
// variables and referenced with $env:VAR inside the script, never spliced into the
// script text. PowerShell's lexer treats several Unicode code points
// (U+2018/2019/201A/201B) as single-quote delimiters, so escaping untrusted text
// into a quoted literal is not safe; an env var is pure data and immune to every
// quoting trick.
func confirmDialog(title, message string, labels ConfirmLabels) (ConfirmChoice, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dialogTimeout)
	defer cancel()

	// -STA because WinForms needs a single-threaded apartment. powershell.exe
	// defaults to it, but pwsh does not, and being explicit costs nothing.
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-STA", "-Command", confirmFormScript(labels.Later != ""))
	cmd.Env = append(os.Environ(),
		"HTMLCLAY_DIALOG_TITLE="+title,
		"HTMLCLAY_DIALOG_MESSAGE="+message,
		"HTMLCLAY_DIALOG_HEIGHT="+strconv.Itoa(confirmFormHeight(message)),
		"HTMLCLAY_DIALOG_ALLOW="+labels.Allow,
		"HTMLCLAY_DIALOG_ALWAYS="+labels.Always,
		"HTMLCLAY_DIALOG_LATER="+labels.Later,
	)
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return ConfirmDismissed, nil
	}
	if err != nil {
		return confirmDialogMessageBox(title, message, labels)
	}
	return choiceFromDialogResult(string(out)), nil
}

// confirmFormScript is the form. Each button carries a DialogResult, so
// WinForms closes the window and ShowDialog returns the answer with no event
// handlers, no closures, and no scope tricks. That matters more than usual
// here: nothing in CI can click this window, so the script has to be the
// plainest thing that works.
//
// The buttons sit in a right-to-left FlowLayoutPanel and size themselves to
// their text rather than to fixed bounds. The labels are the caller's, and a
// fixed 140px button silently clips a longer one, which turns a security prompt
// into a button whose meaning the user has to guess.
//
// WinForms makes whichever button has focus the default, so Return clicks it,
// and focus starts on the lowest TabIndex. Before 1.10.0 that was the wider
// grant, the first button added. The first TabIndex, CancelButton and
// AcceptButton now all belong to Later when there is one, so Return, Escape and
// the close box decide nothing, and to Deny when there is not.
func confirmFormScript(later bool) string {
	script := "$ErrorActionPreference = 'Stop'; " +
		"Add-Type -AssemblyName System.Windows.Forms; " +
		"Add-Type -AssemblyName System.Drawing; " +
		"$f = New-Object System.Windows.Forms.Form; " +
		"$f.Text = $env:HTMLCLAY_DIALOG_TITLE; " +
		"$f.FormBorderStyle = 'FixedDialog'; $f.MaximizeBox = $false; $f.MinimizeBox = $false; " +
		"$f.StartPosition = 'CenterScreen'; $f.TopMost = $true; " +
		"$f.ClientSize = New-Object System.Drawing.Size(640, [int]$env:HTMLCLAY_DIALOG_HEIGHT); " +
		"$p = New-Object System.Windows.Forms.Panel; $p.Dock = 'Fill'; " +
		"$p.Padding = New-Object System.Windows.Forms.Padding(16, 16, 16, 8); " +
		"$l = New-Object System.Windows.Forms.TextBox; " +
		"$l.Multiline = $true; $l.ReadOnly = $true; $l.ScrollBars = 'Vertical'; " +
		"$l.BorderStyle = 'None'; $l.BackColor = $f.BackColor; $l.TabStop = $false; " +
		"$l.Dock = 'Fill'; $l.Lines = $env:HTMLCLAY_DIALOG_MESSAGE -split \"`n\"; " +
		"$p.Controls.Add($l); " +
		"$row = New-Object System.Windows.Forms.FlowLayoutPanel; " +
		"$row.FlowDirection = 'RightToLeft'; $row.Dock = 'Bottom'; $row.Height = 52; " +
		"$row.Padding = New-Object System.Windows.Forms.Padding(8, 8, 8, 8); " +
		"$deny = New-Object System.Windows.Forms.Button; " +
		"$deny.Text = 'Deny'; " +
		"$deny.DialogResult = [System.Windows.Forms.DialogResult]::No; " +
		"$allow = New-Object System.Windows.Forms.Button; " +
		"$allow.Text = $env:HTMLCLAY_DIALOG_ALLOW; " +
		"$allow.DialogResult = [System.Windows.Forms.DialogResult]::OK; " +
		"$always = New-Object System.Windows.Forms.Button; " +
		"$always.Text = $env:HTMLCLAY_DIALOG_ALWAYS; " +
		"$always.DialogResult = [System.Windows.Forms.DialogResult]::Yes; " +
		"$order = @($deny, $allow, $always); "
	if later {
		script += "$later = New-Object System.Windows.Forms.Button; " +
			"$later.Text = $env:HTMLCLAY_DIALOG_LATER; " +
			"$later.DialogResult = [System.Windows.Forms.DialogResult]::Cancel; " +
			"$order = @($later, $deny, $allow, $always); "
	}
	return script +
		"foreach ($b in $order) { $b.AutoSize = $true; " +
		"$b.AutoSizeMode = 'GrowAndShrink'; " +
		"$b.MinimumSize = New-Object System.Drawing.Size(96, 32); " +
		"$b.Margin = New-Object System.Windows.Forms.Padding(6, 4, 6, 4) }; " +
		"[array]::Reverse($order); $row.Controls.AddRange($order); [array]::Reverse($order); " +
		"for ($i = 0; $i -lt $order.Count; $i++) { $order[$i].TabIndex = $i }; " +
		"$f.Controls.AddRange(@($p, $row)); " +
		"$f.CancelButton = $order[0]; $f.AcceptButton = $order[0]; " +
		"Write-Output $f.ShowDialog()"
}

// confirmFormHeight sizes the form to its message, which now names a program
// path per helper and can run to a dozen lines. The label wraps at 608px, about
// 80 characters. The button row and padding take 76px, each line about 20px,
// and a form taller than 640px would not fit a small laptop screen.
func confirmFormHeight(message string) int {
	lines := 0
	for _, line := range strings.Split(message, "\n") {
		lines += 1 + len(line)/80
	}
	return min(max(76+20*lines, 220), 640)
}

// confirmDialogMessageBox is the pre-1.4.0 prompt, kept as the fallback for a
// machine where the custom form will not run. It offers two decisions, Yes for
// ConfirmAllowOnce and No for ConfirmDeny, and Cancel, which is Later when the
// caller has one. The keyboard default is Cancel with a Later and No without
// one, so it is never Yes.
//
// MessageBox cannot relabel its buttons, so the labels are appended to the
// message text instead. Without them the user is asked to say Yes to a grant
// this dialog never names.
func confirmDialogMessageBox(title, message string, labels ConfirmLabels) (ConfirmChoice, error) {
	defaultButton := "Button2"
	if labels.Allow != "" {
		message += "\n\nYes: " + labels.Allow + "\nNo: Deny"
	}
	if labels.Later != "" {
		defaultButton = "Button3"
		message += "\nCancel: " + labels.Later
	}
	const script = "$ErrorActionPreference = 'Stop'; " +
		"Add-Type -AssemblyName System.Windows.Forms; " +
		"$r = [System.Windows.Forms.MessageBox]::Show(" +
		"$env:HTMLCLAY_DIALOG_MESSAGE, $env:HTMLCLAY_DIALOG_TITLE, " +
		"'YesNoCancel', 'Warning', $env:HTMLCLAY_DIALOG_DEFAULT); Write-Output $r"

	ctx, cancel := context.WithTimeout(context.Background(), dialogTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.Env = append(os.Environ(),
		"HTMLCLAY_DIALOG_TITLE="+title,
		"HTMLCLAY_DIALOG_MESSAGE="+message,
		"HTMLCLAY_DIALOG_DEFAULT="+defaultButton,
	)
	out, err := cmd.Output()
	if err != nil {
		return ConfirmDismissed, nil
	}
	switch strings.TrimSpace(string(out)) {
	case "Yes":
		return ConfirmAllowOnce, nil
	case "No":
		return ConfirmDeny, nil
	}
	return ConfirmDismissed, nil
}

// confirmTwoButtons shows a YesNo WinForms message box. MessageBox cannot
// relabel its buttons, so the affirmative label is folded into the message text
// and Yes stands in for it — wording degrades, the two-choice shape does not.
// Button2 makes No the keyboard default, as the contract says Deny is.
// No, close, timeout, and any error all fail closed. Text passes through env
// vars for the same quoting-immunity reason as confirmDialog above.
func confirmTwoButtons(title, message, allowLabel string) (bool, error) {
	const script = "$ErrorActionPreference = 'Stop'; " +
		"Add-Type -AssemblyName System.Windows.Forms; " +
		"$r = [System.Windows.Forms.MessageBox]::Show(" +
		"$env:HTMLCLAY_DIALOG_MESSAGE, $env:HTMLCLAY_DIALOG_TITLE, " +
		"'YesNo', 'Warning', 'Button2'); Write-Output $r"

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
