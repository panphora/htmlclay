//go:build !darwin && !linux && !windows

package platform

import "errors"

func manageProgram(p ProgramSummary) (ManageChoice, error) {
	return ManageCancel, errors.New("native program management dialog not implemented on this platform")
}
