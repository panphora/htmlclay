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

// confirmDialog shows a WinForms message box via PowerShell. A true three-button
// prompt is fiddly, so ConfirmTrustFolder degrades to ConfirmAllowOnce on Windows
// v1; the tray's own Add Trusted Folder flow is still the full persistent-grant
// path, so this is acceptable. Yes maps to ConfirmAllowOnce; No, Cancel, timeout,
// and any error all fail closed to ConfirmDeny.
//
// The title and message are passed as environment variables and referenced with
// $env:VAR inside the script, never spliced into the script text. PowerShell's
// lexer treats several Unicode code points (U+2018/2019/201A/201B) as single-quote
// delimiters, so escaping untrusted text into a quoted literal is not safe; an env
// var is pure data and immune to every quoting trick.
func confirmDialog(title, message string) (ConfirmChoice, error) {
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
