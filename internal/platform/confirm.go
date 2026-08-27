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

// ConfirmWithButtons shows a modal, foreground native dialog with exactly two
// buttons — allowLabel and "Deny", Deny the default — and reports whether the
// user chose allowLabel. Same contract as Confirm: always a real OS dialog a
// page cannot spoof or auto-confirm, failing closed to false on any error,
// timeout, or unsupported platform. Platforms whose dialog cannot relabel its
// buttons (Windows) fold the label into the message and map their affirmative
// button to it, which degrades wording, never safety.
func ConfirmWithButtons(title, message, allowLabel string) (bool, error) {
	return confirmTwoButtons(title, message, allowLabel)
}

// MissingDialogAdvice returns a sentence naming what to install when this
// machine has no way to raise a permission dialog at all, and "" when it has
// one. It is a startup check, not a per-prompt one: the answer is a property of
// what is installed, and asking once is what lets the app say so up front
// instead of failing a prompt the user is waiting on.
//
// Failing closed is correct and is what Confirm already does, but a permission
// dialog that can never appear turns every out-of-folder open into a file that
// does not open with nothing on screen explaining it. Only Linux can be in that
// state: macOS and Windows raise their prompts through components that are part
// of the operating system.
func MissingDialogAdvice() string {
	return missingDialogAdvice()
}
