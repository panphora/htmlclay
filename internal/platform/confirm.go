package platform

import (
	"errors"
	"os/exec"
	"strings"
)

// ConfirmChoice is the outcome of a native permission dialog.
type ConfirmChoice int

const (
	// ConfirmDismissed is a dialog that closed without an answer: Return or
	// Escape where they are kept off Deny, the close box, the timeout, a missing
	// dialog tool, and every error. It grants nothing, and a caller that stores
	// refusals stores nothing for it, so the next prompt asks again. It is the
	// zero value, so a choice nobody set can never read as a refusal to remember.
	ConfirmDismissed ConfirmChoice = iota
	// ConfirmDeny is a refusal the user made. When the caller supplies a Later
	// label it comes back only for the Deny button itself; without one, Deny is
	// also the keyboard default, so a caller that stores refusals must set it.
	ConfirmDeny
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
	case ConfirmDeny:
		return "deny"
	case ConfirmAllowOnce:
		return "allow-once"
	case ConfirmAllowAlways:
		return "allow-always"
	default:
		return "dismissed"
	}
}

// confirmDenyLabel is the one button label this package fixes.
const confirmDenyLabel = "Deny"

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
	// Later labels a button that decides nothing and comes back as
	// ConfirmDismissed. Setting it asks for a dialog where only a click on Deny
	// refuses: Return and Escape land on Later where the platform can draw it,
	// and on no button at all where it cannot (macOS allows three buttons, so
	// there Return and Escape do nothing and Later is not shown). Left empty,
	// Deny is the keyboard default, which suits only a refusal that is not
	// stored. Either way no keyboard default ever grants.
	Later string
}

// Confirm shows a modal, foreground native dialog for a permission grant and
// returns the user's choice. It is always a real OS dialog, never page content,
// so a served page cannot spoof, style, obscure, or auto-confirm it. No keyboard
// default grants anything. On any error, timeout, or unsupported platform it
// fails closed to ConfirmDismissed.
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

// The three functions below map what each platform's dialog reported onto a
// choice. They live here rather than behind build tags so every platform's
// mapping is tested on every machine: no test can click a native dialog, and
// this mapping is where a wrong answer would store a refusal nobody made or
// grant a program nobody chose.

// choiceFromOSAScript maps the button display dialog reports. A dialog that
// gave up reports no button, which is checked first because a caller label left
// empty would otherwise match it.
func choiceFromOSAScript(button string, labels ConfirmLabels) ConfirmChoice {
	switch {
	case button == "":
		return ConfirmDismissed
	case button == confirmDenyLabel:
		return ConfirmDeny
	case button == labels.Always:
		return ConfirmAllowAlways
	case button == labels.Allow:
		return ConfirmAllowOnce
	}
	return ConfirmDismissed
}

// choiceFromZenity maps one zenity --question run. OK exits 0 printing
// nothing; an extra button exits 1 printing its own label; the cancel button,
// Escape and the close box exit 1 printing nothing. With a Later label the
// cancel button is Later and Deny is an extra button, so a silent exit 1 is no
// answer. Without one the cancel button is Deny. Any other exit, including the
// kill at the deadline, decided nothing.
func choiceFromZenity(out string, exit int, labels ConfirmLabels) ConfirmChoice {
	answer := strings.TrimSpace(out)
	switch {
	case exit == 0 && answer == "":
		return ConfirmAllowOnce
	case exit != 1:
		return ConfirmDismissed
	case answer == confirmDenyLabel:
		return ConfirmDeny
	case answer != "" && answer == labels.Always:
		return ConfirmAllowAlways
	case answer == "" && labels.Later == "":
		return ConfirmDeny
	}
	return ConfirmDismissed
}

// choiceFromKDialog maps the tag a kdialog --radiolist prints on OK. Cancel,
// Escape and the close box exit 1, and the Later row decided nothing either.
func choiceFromKDialog(out string, exit int) ConfirmChoice {
	if exit != 0 {
		return ConfirmDismissed
	}
	switch strings.TrimSpace(out) {
	case "deny":
		return ConfirmDeny
	case "once":
		return ConfirmAllowOnce
	case "always":
		return ConfirmAllowAlways
	}
	return ConfirmDismissed
}

// choiceFromDialogResult maps the DialogResult name the Windows form prints:
// Yes for the wider grant, OK for the narrower one, No for Deny. Later, the
// close box and anything unrecognized decided nothing.
func choiceFromDialogResult(out string) ConfirmChoice {
	switch strings.TrimSpace(out) {
	case "Yes":
		return ConfirmAllowAlways
	case "OK":
		return ConfirmAllowOnce
	case "No":
		return ConfirmDeny
	}
	return ConfirmDismissed
}

// exitCode is a finished dialog tool's exit status, 0 for a clean exit, and -1
// for a tool that was killed, timed out, or never started.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}
