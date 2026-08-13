//go:build linux

package platform

import (
	"context"
	"errors"
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

// confirmDialog shows a three-button permission prompt via zenity, falling back
// to kdialog. Availability is probed with exec.LookPath; if neither tool is
// present the grant fails closed to ConfirmDeny rather than silently allowing.
//
// zenity prints the extra-button label to stdout when it is clicked, so the
// stdout check runs before the exit-code check: "Trust this folder" maps to
// ConfirmTrustFolder, a zero exit maps to ConfirmAllowOnce, and any non-zero
// exit (Deny, window close, timeout, or a launch failure) maps to ConfirmDeny.
//
// --no-markup is passed so a folder name containing Pango markup cannot restyle or
// rewrite the prompt text; the label is rendered literally.
func confirmDialog(title, message string) (ConfirmChoice, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dialogTimeout)
	defer cancel()

	if bin, err := exec.LookPath("zenity"); err == nil {
		out, err := exec.CommandContext(ctx, bin,
			"--question",
			"--no-markup",
			"--title", title,
			"--text", message,
			"--ok-label", "Allow Once",
			"--extra-button", "Trust this folder",
			"--cancel-label", "Deny",
		).Output()
		if strings.Contains(strings.TrimSpace(string(out)), "Trust this folder") {
			return ConfirmTrustFolder, nil
		}
		if err != nil {
			return ConfirmDeny, nil
		}
		return ConfirmAllowOnce, nil
	}

	// kdialog has no clean third button, so ConfirmTrustFolder degrades to
	// ConfirmAllowOnce here; the tray's own Add Trusted Folder flow is still the
	// full persistent-grant path, so this is acceptable. Yes maps to
	// ConfirmAllowOnce, No/close maps to ConfirmDeny.
	if bin, err := exec.LookPath("kdialog"); err == nil {
		if err := exec.CommandContext(ctx, bin, "--title", title, "--warningyesno", message).Run(); err != nil {
			return ConfirmDeny, nil
		}
		return ConfirmAllowOnce, nil
	}

	return ConfirmDeny, errors.New("no native dialog tool (zenity or kdialog) found")
}

// confirmTwoButtons shows a two-button prompt via zenity, falling back to
// kdialog (whose --yes-label/--no-label make it a full two-button dialog, so
// nothing degrades here). With neither tool present it fails closed.
func confirmTwoButtons(title, message, allowLabel string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dialogTimeout)
	defer cancel()

	if bin, err := exec.LookPath("zenity"); err == nil {
		err := exec.CommandContext(ctx, bin,
			"--question",
			"--no-markup",
			"--title", title,
			"--text", message,
			"--ok-label", allowLabel,
			"--cancel-label", "Deny",
		).Run()
		return err == nil, nil
	}

	if bin, err := exec.LookPath("kdialog"); err == nil {
		err := exec.CommandContext(ctx, bin,
			"--title", title,
			"--yes-label", allowLabel,
			"--no-label", "Deny",
			"--warningyesno", message,
		).Run()
		return err == nil, nil
	}

	return false, errors.New("no native dialog tool (zenity or kdialog) found")
}
