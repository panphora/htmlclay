package platform

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	manageProgramTimeout = 2 * time.Minute
	forgetDecisionsLabel = "Forget document permissions"
	removeProgramLabel   = "Remove program"
)

type ProgramSummary struct {
	Name        string
	Path        string
	AnyDocument bool
	Decisions   int
	Missing     bool
}

type ManageChoice int

const (
	ManageCancel ManageChoice = iota
	ManageToggleAnyDocument
	ManageForgetDecisions
	ManageRemove
)

// ManageProgram shows the management dialog for one registered program and
// returns the chosen action. It fails closed to ManageCancel on any error,
// timeout, unsupported platform, or invalid platform result.
func ManageProgram(p ProgramSummary) (ManageChoice, error) {
	choice, err := manageProgram(p)
	if err != nil {
		return ManageCancel, err
	}
	if choice < ManageCancel || choice > ManageRemove {
		return ManageCancel, errors.New("program management dialog returned an invalid choice")
	}
	return choice, nil
}

func manageToggleLabel(anyDocument bool) string {
	if anyDocument {
		return "Stop allowing any document"
	}
	return "Allow any document"
}

func manageProgramMessage(p ProgramSummary) string {
	availability := "available"
	if p.Missing {
		availability = "missing"
	}
	scope := "allowed documents only"
	if p.AnyDocument {
		scope = "any document"
	}
	return fmt.Sprintf("Name: %s\nProgram: %s\nStatus: %s\nAccess: %s\nDocument decisions: %d", p.Name, p.Path, availability, scope, p.Decisions)
}

func manageChoiceFromResult(result string, p ProgramSummary) (ManageChoice, bool) {
	switch strings.Trim(result, "\r\n") {
	case "", "cancel":
		return ManageCancel, true
	case "toggle", manageToggleLabel(p.AnyDocument):
		return ManageToggleAnyDocument, true
	case "forget", forgetDecisionsLabel:
		return ManageForgetDecisions, true
	case "remove", removeProgramLabel:
		return ManageRemove, true
	default:
		return ManageCancel, false
	}
}
