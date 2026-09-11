//go:build linux

package platform

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
)

// Both backends use radiolists because the existing confirmation dialog loses
// its third action on kdialog. The management toggle must remain reachable.
func manageProgram(p ProgramSummary) (ManageChoice, error) {
	ctx, cancel := context.WithTimeout(context.Background(), manageProgramTimeout)
	defer cancel()

	toggle := manageToggleLabel(p.AnyDocument)
	if bin, err := exec.LookPath("zenity"); err == nil {
		out, err := exec.CommandContext(ctx, bin,
			"--list",
			"--radiolist",
			"--title", "Manage "+p.Name,
			"--text", manageProgramMessage(p),
			"--column", "",
			"--column", "Action",
			"--print-column", "2",
			"FALSE", toggle,
			"FALSE", forgetDecisionsLabel,
			"FALSE", removeProgramLabel,
		).Output()
		if err != nil {
			if cancelled(err) {
				return ManageCancel, nil
			}
			return ManageCancel, fmt.Errorf("zenity program management dialog failed: %w", err)
		}
		choice, ok := manageChoiceFromResult(string(out), p)
		if !ok {
			return ManageCancel, fmt.Errorf("zenity program management dialog returned an unexpected result")
		}
		return choice, nil
	}

	if bin, err := exec.LookPath("kdialog"); err == nil {
		out, err := exec.CommandContext(ctx, bin,
			"--title", "Manage "+p.Name,
			"--radiolist", manageProgramMessage(p),
			"toggle", toggle, "off",
			"forget", forgetDecisionsLabel, "off",
			"remove", removeProgramLabel, "off",
		).Output()
		if err != nil {
			if cancelled(err) {
				return ManageCancel, nil
			}
			return ManageCancel, fmt.Errorf("kdialog program management dialog failed: %w", err)
		}
		choice, ok := manageChoiceFromResult(string(out), p)
		if !ok {
			return ManageCancel, fmt.Errorf("kdialog program management dialog returned an unexpected result")
		}
		return choice, nil
	}

	return ManageCancel, errors.New("no native program management dialog found (neither zenity nor kdialog is installed)")
}
