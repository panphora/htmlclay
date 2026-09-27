//go:build linux

package platform

import (
	"context"
	"errors"
	"os/exec"
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

// confirmDialog shows a three-button permission prompt via zenity, falling back
// to kdialog. Availability is probed with exec.LookPath; if neither tool is
// present the prompt fails closed to ConfirmDismissed rather than silently
// allowing.
//
// zenity puts keyboard focus on OK unless told otherwise, and Return activates
// the focused button, so without --default-cancel a reflexive Return granted
// the narrower permission. --default-cancel moves focus to the cancel button,
// which is Later when the caller supplies one (Deny then becomes an extra
// button that prints its own label) and Deny when it does not.
//
// --no-markup is passed so a folder name containing Pango markup cannot restyle or
// rewrite the prompt text; the label is rendered literally.
func confirmDialog(title, message string, labels ConfirmLabels) (ConfirmChoice, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dialogTimeout)
	defer cancel()

	if bin, err := exec.LookPath("zenity"); err == nil {
		out, err := exec.CommandContext(ctx, bin, zenityConfirmArgs(title, message, labels)...).Output()
		return choiceFromZenity(string(out), exitCode(err), labels), nil
	}

	// kdialog's yes/no dialogs put Return on Yes whatever the labels say, so a
	// button dialog cannot keep Return off every decision. A radiolist can: its
	// OK returns the highlighted row even when nothing is checked, and the
	// highlight starts on the first row, so the first row is the keyboard
	// default. That row is Later when there is one and Deny when there is not.
	// A list also has room for the wider grant, which the old yes/no box lost.
	if bin, err := exec.LookPath("kdialog"); err == nil {
		out, err := exec.CommandContext(ctx, bin, kdialogConfirmArgs(title, message, labels)...).Output()
		return choiceFromKDialog(string(out), exitCode(err)), nil
	}

	return ConfirmDismissed, errors.New("no native dialog tool (zenity or kdialog) found")
}

// zenityConfirmArgs puts Always before Deny among the extra buttons, so a
// zenity that keeps only the last --extra-button loses the wider grant rather
// than the refusal.
func zenityConfirmArgs(title, message string, labels ConfirmLabels) []string {
	args := []string{
		"--question",
		"--no-markup",
		"--title", title,
		"--text", message,
		"--ok-label", labels.Allow,
		"--extra-button", labels.Always,
	}
	if labels.Later == "" {
		return append(args, "--cancel-label", confirmDenyLabel, "--default-cancel")
	}
	return append(args, "--extra-button", confirmDenyLabel, "--cancel-label", labels.Later, "--default-cancel")
}

func kdialogConfirmArgs(title, message string, labels ConfirmLabels) []string {
	args := []string{"--title", title, "--radiolist", message}
	if labels.Later != "" {
		args = append(args, "later", labels.Later, "off")
	}
	return append(args,
		"deny", confirmDenyLabel, "off",
		"once", labels.Allow, "off",
		"always", labels.Always, "off",
	)
}

// confirmTwoButtons shows a two-button prompt via zenity, falling back to
// kdialog (whose --yes-label/--no-label make it a full two-button dialog, so
// nothing degrades here). With neither tool present it fails closed.
// --default-cancel keeps Return on Deny, as the contract says; without it
// zenity focuses the affirmative button and Return accepted it.
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
			"--cancel-label", confirmDenyLabel,
			"--default-cancel",
		).Run()
		return err == nil, nil
	}

	// Return always lands on kdialog's Yes, so Yes is Deny here and the
	// affirmative is No: exit 1 is the only answer that allows. Escape goes to
	// Cancel, which exits 2.
	if bin, err := exec.LookPath("kdialog"); err == nil {
		err := exec.CommandContext(ctx, bin,
			"--title", title,
			"--yes-label", confirmDenyLabel,
			"--no-label", allowLabel,
			"--cancel-label", "Cancel",
			"--warningyesnocancel", message,
		).Run()
		return exitCode(err) == 1, nil
	}

	return false, errors.New("no native dialog tool (zenity or kdialog) found")
}

// missingDialogAdvice reports the one machine state that makes HTML Clay look
// broken for no visible reason: a desktop with neither zenity nor kdialog, where
// confirmDialog and confirmTwoButtons above can only fail closed.
//
// It covers the permission prompts alone. The folder picker has a third backend
// that needs no helper binary (selectfolder_linux.go), and there is no portal
// interface for a three-button question, so a machine with neither tool can
// still trust a folder from the tray and then work with no prompts at all.
func missingDialogAdvice() string {
	for _, bin := range []string{"zenity", "kdialog"} {
		if _, err := exec.LookPath(bin); err == nil {
			return ""
		}
	}
	return "HTML Clay cannot show permission dialogs on this desktop. Install zenity or kdialog."
}
