package platform

// ConfirmChoice is the outcome of a native permission dialog.
type ConfirmChoice int

const (
	// ConfirmDeny is also the fail-closed default: any error, timeout, or
	// unsupported platform resolves to Deny so access is never granted by accident.
	ConfirmDeny ConfirmChoice = iota
	// ConfirmAllowOnce is the narrower of the two grants: the second button.
	ConfirmAllowOnce
	// ConfirmAllowAlways is the wider, durable grant: the third button. What it
	// widens to is the caller's business, not this package's. The read prompt uses
	// it to remember a folder as trusted; the document-programs prompt uses it to
	// allow a program for every document. Platforms with no clean third button
	// degrade it to ConfirmAllowOnce, which errs toward less permission rather
	// than more.
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

// ConfirmLabels names the two affirmative buttons of the three-button dialog.
// Every caller supplies its own, because one dialog shape now backs two
// unrelated grants, and a button that describes the other one is worse than no
// button at all: "Trust this folder" over a prompt that actually registers a
// program tells the user the wrong thing about what they are approving. There
// is deliberately no default.
type ConfirmLabels struct {
	// Allow labels the narrower grant and comes back as ConfirmAllowOnce.
	Allow string
	// Always labels the wider, durable grant and comes back as ConfirmAllowAlways.
	Always string
}

// Confirm shows a modal, foreground native dialog for a permission grant and
// returns the user's choice. It is always a real OS dialog, never page content,
// so a served page cannot spoof, style, obscure, or auto-confirm it. On any
// error or unsupported platform it fails closed to ConfirmDeny.
func Confirm(title, message string, labels ConfirmLabels) (ConfirmChoice, error) {
	return confirmDialog(title, message, labels)
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
