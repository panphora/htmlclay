package platform

// ConfirmChoice is the outcome of a native permission dialog.
type ConfirmChoice int

const (
	// ConfirmDeny is also the fail-closed default: any error, timeout, or
	// unsupported platform resolves to Deny so access is never granted by accident.
	ConfirmDeny ConfirmChoice = iota
	ConfirmAllowOnce
	ConfirmAllowAlways
)

func (c ConfirmChoice) String() string {
	switch c {
	case ConfirmAllowOnce:
		return "allow-once"
	case ConfirmAllowAlways:
		return "allow-always"
	default:
		return "deny"
	}
}

// Confirm shows a modal, foreground native dialog for a permission grant and
// returns the user's choice. It is always a real OS dialog, never page content,
// so a served page cannot spoof, style, obscure, or auto-confirm it. On any
// error or unsupported platform it fails closed to ConfirmDeny.
func Confirm(title, message string) (ConfirmChoice, error) {
	return confirmDialog(title, message)
}
