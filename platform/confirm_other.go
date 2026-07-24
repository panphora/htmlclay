//go:build !darwin

package platform

import "errors"

// confirmDialog is a fail-closed placeholder for platforms whose native modal is
// not implemented yet. Linux (zenity/kdialog) and Windows (TaskDialogIndirect)
// land in a later phase; until then a grant can never be approved here.
func confirmDialog(title, message string) (ConfirmChoice, error) {
	return ConfirmDeny, errors.New("native confirm dialog not implemented on this platform")
}
