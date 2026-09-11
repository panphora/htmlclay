//go:build darwin

package platform

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// AppleScript display dialog supports only three buttons total. A native list
// keeps the three required actions and a distinct Cancel outcome in one modal.
//
// Nothing is preselected. Two of the three actions are destructive or widen a
// permission, and a list that arrives with one of them already highlighted
// turns a reflexive Return into a decision the user never made.
func manageProgram(p ProgramSummary) (ManageChoice, error) {
	ctx, cancel := context.WithTimeout(context.Background(), manageProgramTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "osascript", "-e", "activate me", "-e", manageProgramScript(p)).CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "-128") {
			return ManageCancel, nil
		}
		return ManageCancel, fmt.Errorf("program management dialog failed: %w", err)
	}
	choice, ok := manageChoiceFromResult(string(out), p)
	if !ok {
		return ManageCancel, fmt.Errorf("program management dialog returned an unexpected result %q", strings.Trim(string(out), "\r\n"))
	}
	return choice, nil
}

func manageProgramScript(p ProgramSummary) string {
	toggle := manageToggleLabel(p.AnyDocument)
	return "set picked to choose from list {" +
		appleScriptString(toggle) + ", " + appleScriptString(forgetDecisionsLabel) + ", " + appleScriptString(removeProgramLabel) + "}" +
		" with title " + appleScriptString("Manage "+p.Name) +
		" with prompt " + appleScriptString(manageProgramMessage(p)) +
		` OK button name "Apply" cancel button name "Cancel"` +
		"\nif picked is false or picked is {} then return \"cancel\"" +
		"\nreturn item 1 of picked"
}
