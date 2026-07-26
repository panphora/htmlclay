package platform

// ConfirmChoice is the outcome of a native permission dialog.
type ConfirmChoice int

const (
	// ConfirmDeny is also the fail-closed default: any error, timeout, or
	// unsupported platform resolves to Deny so access is never granted by accident.
	ConfirmDeny ConfirmChoice = iota
	ConfirmAllowOnce
	// ConfirmTrustFolder is the durable choice: allow this read AND remember the
	// folder as trusted, so files opened from inside it stop asking. Platforms with
	// no clean third button degrade it to ConfirmAllowOnce, which errs toward less
	// permission rather than more.
	ConfirmTrustFolder
)

func (c ConfirmChoice) String() string {
	switch c {
	case ConfirmAllowOnce:
		return "allow-once"
	case ConfirmTrustFolder:
		return "trust-folder"
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
