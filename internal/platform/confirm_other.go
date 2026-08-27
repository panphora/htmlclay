//go:build !darwin && !linux && !windows

package platform

import "errors"

// confirmDialog is a fail-closed placeholder for platforms whose native modal is
// not implemented yet. Linux (zenity/kdialog) and Windows (TaskDialogIndirect)
// land in a later phase; until then a grant can never be approved here.
func confirmDialog(title, message string) (ConfirmChoice, error) {
	return ConfirmDeny, errors.New("native confirm dialog not implemented on this platform")
}

func confirmTwoButtons(title, message, allowLabel string) (bool, error) {
	return false, errors.New("native confirm dialog not implemented on this platform")
}

// missingDialogAdvice says so plainly, because confirmDialog above can only
// deny on this platform and the tray row is the only place that shows.
func missingDialogAdvice() string {
	return "HTML Clay cannot show permission dialogs on this platform, so files outside a trusted folder will not open."
}
