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
func confirmDialog(title, message string) (ConfirmChoice, error) {
	dialog := "display dialog " + appleScriptString(message) +
		" with title " + appleScriptString(title) +
		` buttons {"Deny", "Allow Once", "Always Allow"} default button "Deny" with icon caution giving up after 120`
	out, err := exec.Command("osascript", "-e", "activate me", "-e", dialog).CombinedOutput()
	if err != nil {
		return ConfirmDeny, err
	}
	s := string(out)
	switch {
	case strings.Contains(s, "Always Allow"):
		return ConfirmAllowAlways, nil
	case strings.Contains(s, "Allow Once"):
		return ConfirmAllowOnce, nil
	default:
		// "button returned:Deny" and "gave up:true" both land here.
		return ConfirmDeny, nil
	}
}
