//go:build darwin

package platform

import (
	"os/exec"
	"strings"
)

// confirmDialog shows a three-button modal via osascript. "activate me" brings
// the osascript process (and its dialog) to the foreground; without it a dialog
// spawned from the background tray process appears behind the active window and
// gets missed. This was verified in the grant-flow spike: plain `display dialog`
// went unnoticed, `activate me` broke through. `giving up after 120` matches the
// broker's park ceiling so an ignored dialog self-dismisses to Deny.
//
// Only "Deny" is fixed. The two affirmative labels come from the caller, because
// the same three-button shape asks two different questions and each one has to
// name the grant it actually makes.
func confirmDialog(title, message string, labels ConfirmLabels) (ConfirmChoice, error) {
	dialog := "display dialog " + appleScriptString(message) +
		" with title " + appleScriptString(title) +
		` buttons {"Deny", ` + appleScriptString(labels.Allow) + ", " + appleScriptString(labels.Always) +
		`} default button "Deny" with icon caution giving up after 120`
	out, err := exec.Command("osascript", "-e", "activate me", "-e", dialog).CombinedOutput()
	if err != nil {
		return ConfirmDeny, err
	}
	switch osascriptButton(string(out)) {
	case "":
		// Deny is the only label this package fixes, so it needs no comparison:
		// an empty answer is Deny, a timeout ("gave up:true") carries no button
		// at all, and anything unrecognized fails closed the same way.
		return ConfirmDeny, nil
	case labels.Always:
		return ConfirmAllowAlways, nil
	case labels.Allow:
		return ConfirmAllowOnce, nil
	default:
		return ConfirmDeny, nil
	}
}

// osascriptButton returns the exact label the user clicked. `display dialog`
// answers with "button returned:<label>, gave up:false", and a dialog that gave
// up carries no label at all.
//
// Matching on substrings is not good enough now that callers supply their own
// labels: one label can be a prefix of the other ("Allow" inside "Allow for Any
// Document"), and either can appear in the message text above the buttons, which
// CombinedOutput does not separate from the answer.
func osascriptButton(out string) string {
	const key = "button returned:"
	i := strings.Index(out, key)
	if i < 0 {
		return ""
	}
	label := out[i+len(key):]
	if j := strings.Index(label, ", gave up:"); j >= 0 {
		label = label[:j]
	}
	return strings.TrimRight(label, "\r\n")
}

// confirmTwoButtons is confirmDialog's two-button variant, sharing its
// foregrounding and its 120s give-up (which self-dismisses to deny).
func confirmTwoButtons(title, message, allowLabel string) (bool, error) {
	dialog := "display dialog " + appleScriptString(message) +
		" with title " + appleScriptString(title) +
		" buttons {\"Deny\", " + appleScriptString(allowLabel) + `} default button "Deny" with icon caution giving up after 120`
	out, err := exec.Command("osascript", "-e", "activate me", "-e", dialog).CombinedOutput()
	if err != nil {
		return false, err
	}
	// "button returned:Deny" and "gave up:true" both read as no.
	return allowLabel != "" && osascriptButton(string(out)) == allowLabel, nil
}

// missingDialogAdvice always answers "nothing is missing": the prompt is an
// osascript dialog, and osascript is part of macOS.
func missingDialogAdvice() string { return "" }
