package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/panphora/htmlclay/internal/config"
	"github.com/panphora/htmlclay/internal/htmlutil"
	"github.com/panphora/htmlclay/internal/platform"
	"github.com/panphora/htmlclay/internal/tray"
)

const helperDeclarationReadLimit = 512 << 10

type helperOpenPlan struct {
	names   []string
	allowed map[string]config.HelperProgram
	denied  []string
}

type helperApprovalDialogs struct {
	confirm func(title, message string, allowBroad bool) (platform.ConfirmChoice, error)
	choose  func(prompt string) (string, bool, error)
	prepare func(path string) (bool, error)
}

type helperCandidate struct {
	name    string
	program config.HelperProgram
	path    string
}

type helperDecisionUndo struct {
	document string
	name     string
	previous config.HelperDecision
	had      bool
}

// helperConfirmLabels names the two affirmative buttons of the document-programs
// prompt. Both differ from the read prompt's, and both had to: choosing the
// narrower one records a decision that stands until the tray changes it, so
// "Allow Once" would be a lie, and the wider one grants a PROGRAM everywhere
// rather than trusting a folder.
var helperConfirmLabels = platform.ConfirmLabels{
	Allow:  "Allow for This Document",
	Always: "Allow for Any Document",
}

func (a *app) systemHelperApprovalDialogs() helperApprovalDialogs {
	confirm := a.rt.confirmHelpers
	if confirm == nil {
		confirm = func(title, message string, _ bool) (platform.ConfirmChoice, error) {
			return platform.Confirm(title, message, helperConfirmLabels)
		}
	}
	return helperApprovalDialogs{
		confirm: confirm,
		choose:  platform.SelectFile,
		prepare: func(path string) (bool, error) {
			return prepareHelperProgram(path, platform.ConfirmWithButtons)
		},
	}
}

func (a *app) helpersForOpen(document string, prompt bool) helperOpenPlan {
	return a.helpersForOpenWith(document, prompt, a.systemHelperApprovalDialogs())
}

func (a *app) helpersForOpenWith(document string, prompt bool, dialogs helperApprovalDialogs) helperOpenPlan {
	a.helperMu.Lock()
	defer a.helperMu.Unlock()

	names, err := readDocumentHelperNames(document)
	if err != nil {
		a.reportHelperSetupError(document, err)
		return helperOpenPlan{}
	}
	plan := helperOpenPlan{names: names}
	plan.allowed, plan.denied = a.resolvedHelpers(document, names)
	if !prompt || len(names) == 0 {
		return plan
	}

	undecided := make([]helperCandidate, 0, len(names))
	for _, name := range names {
		resolution, registered := a.rt.cfg.ResolveHelper(document, name)
		if resolution.Decided {
			continue
		}
		candidate := helperCandidate{name: name}
		if registered {
			candidate.program = resolution.Program
		}
		undecided = append(undecided, candidate)
	}
	if len(undecided) == 0 {
		return plan
	}

	choice, err := dialogs.confirm(
		"Allow document programs?",
		helperApprovalMessage(document, undecided),
		true,
	)
	if err != nil {
		a.reportHelperSetupError(document, fmt.Errorf("could not show the helper permission dialog: %w", err))
		return plan
	}
	if choice != platform.ConfirmDeny && choice != platform.ConfirmAllowOnce && choice != platform.ConfirmAllowAlways {
		a.reportHelperSetupError(document, errors.New("the helper permission dialog returned an invalid choice"))
		return plan
	}
	if choice != platform.ConfirmDeny {
		for i := range undecided {
			if undecided[i].program.ID != "" {
				continue
			}
			path, ok, err := dialogs.choose("Choose the program for " + undecided[i].name)
			if err != nil {
				a.reportHelperSetupError(document, fmt.Errorf("could not choose %s: %w", undecided[i].name, err))
				return plan
			}
			if !ok {
				return plan
			}
			ok, err = dialogs.prepare(path)
			if err != nil {
				a.reportHelperSetupError(document, fmt.Errorf("could not prepare %s: %w", undecided[i].name, err))
				return plan
			}
			if !ok {
				return plan
			}
			undecided[i].path = path
		}
	}

	if err := a.saveHelperDecisionSet(document, undecided, choice); err != nil {
		a.reportHelperSetupError(document, err)
		return plan
	}
	plan.allowed, plan.denied = a.resolvedHelpers(document, names)
	return plan
}

func readDocumentHelperNames(document string) ([]string, error) {
	f, err := os.Open(document)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, helperDeclarationReadLimit))
	if err != nil {
		return nil, err
	}
	return htmlutil.ReadHelperNames(data), nil
}

func helperApprovalMessage(document string, candidates []helperCandidate) string {
	names := make([]string, len(candidates))
	for i, candidate := range candidates {
		names[i] = candidate.name
		if candidate.program.ID == "" {
			names[i] += " (Choose...)"
		}
	}
	label := "program"
	if len(names) != 1 {
		label = "programs"
	}
	return fmt.Sprintf(
		"%s wants to use %d %s:\n\n%s\n\nThe selected programs run as you and may read or change any file your account can access.",
		filepath.Base(document), len(names), label, strings.Join(names, "\n"),
	)
}

// A name that is in neither result was never decided: nobody was asked, the
// picker was cancelled, or the program behind an old allow is gone.
func (a *app) resolvedHelpers(document string, names []string) (map[string]config.HelperProgram, []string) {
	allowed := make(map[string]config.HelperProgram)
	var denied []string
	for _, name := range names {
		resolution, _ := a.rt.cfg.ResolveHelper(document, name)
		switch {
		case !resolution.Decided:
		case resolution.Allowed:
			allowed[name] = resolution.Program
		default:
			denied = append(denied, name)
		}
	}
	return allowed, denied
}

func (a *app) saveHelperDecisionSet(document string, candidates []helperCandidate, choice platform.ConfirmChoice) error {
	added := make([]config.HelperProgram, 0, len(candidates))
	flagUndo := make(map[string]bool)
	decisionUndo := make([]helperDecisionUndo, 0, len(candidates))
	rollback := func() {
		for i := len(decisionUndo) - 1; i >= 0; i-- {
			undo := decisionUndo[i]
			a.rt.cfg.ForgetHelperDecision(undo.document, undo.name)
			if undo.had {
				a.rt.cfg.RestoreHelperDecisions([]config.HelperDecision{undo.previous})
			}
		}
		for id, previous := range flagUndo {
			a.rt.cfg.SetHelperAnyDocument(id, previous)
		}
		for i := len(added) - 1; i >= 0; i-- {
			a.rt.cfg.RemoveHelperProgram(added[i].ID)
		}
	}

	if choice != platform.ConfirmDeny {
		for i := range candidates {
			if candidates[i].program.ID != "" {
				continue
			}
			program, err := a.rt.cfg.AddHelperProgram(candidates[i].name, candidates[i].path)
			if err != nil {
				rollback()
				return fmt.Errorf("could not register %s: %w", candidates[i].name, err)
			}
			candidates[i].program = program
			added = append(added, program)
		}
	}

	if choice == platform.ConfirmAllowAlways {
		for _, candidate := range candidates {
			if _, seen := flagUndo[candidate.program.ID]; seen {
				continue
			}
			previous, ok := a.rt.cfg.SetHelperAnyDocument(candidate.program.ID, true)
			if !ok {
				rollback()
				return fmt.Errorf("could not allow %s for any document", candidate.name)
			}
			flagUndo[candidate.program.ID] = previous
		}
	}

	now := time.Now().UnixNano()
	for i, candidate := range candidates {
		decision := config.HelperDecision{
			Document:  document,
			Name:      candidate.name,
			Allowed:   choice != platform.ConfirmDeny,
			DecidedAt: now + int64(i),
		}
		if decision.Allowed {
			decision.Program = candidate.program.ID
		}
		previous, had, err := a.rt.cfg.DecideHelper(decision)
		if err != nil {
			rollback()
			return fmt.Errorf("could not record the decision for %s: %w", candidate.name, err)
		}
		decisionUndo = append(decisionUndo, helperDecisionUndo{document: document, name: candidate.name, previous: previous, had: had})
	}
	if err := a.rt.cfg.Save(); err != nil {
		rollback()
		return fmt.Errorf("could not save helper decisions: %w", err)
	}
	return nil
}

func (a *app) applyHelperPlan(s *site, document string, plan helperOpenPlan) {
	if len(plan.names) == 0 {
		s.srv.DetachHelpers(document)
		return
	}
	if err := s.srv.AttachHelpers(document, plan.allowed, plan.denied); err != nil {
		a.rt.logger.Printf("Could not attach helpers for %s: %v", document, err)
	}
}

func (a *app) manageHelperProgramAndRefresh(id string) []tray.Row {
	a.helperMu.Lock()
	rows := a.manageHelperProgram(id)
	a.helperMu.Unlock()
	a.refreshHelperDispatchers()
	return rows
}

func (a *app) refreshHelperDispatchers() {
	type openDocument struct {
		site *site
		path string
	}
	a.mu.Lock()
	var documents []openDocument
	for _, s := range a.sites {
		for _, registration := range s.sessions.Registrations() {
			documents = append(documents, openDocument{site: s, path: registration.Path})
		}
	}
	a.mu.Unlock()
	for _, document := range documents {
		a.applyHelperPlan(document.site, document.path, a.helpersForOpen(document.path, false))
	}
}

func (a *app) reportHelperSetupError(document string, err error) {
	a.rt.logger.Printf("Could not configure helpers for %s: %v", document, err)
	message := fmt.Sprintf("%s could not configure its programs: %v", filepath.Base(document), err)
	if notifyErr := a.notifyUser("HTML Clay could not configure programs", message); notifyErr != nil {
		a.rt.logger.Printf("Could not show helper notification: %v", notifyErr)
	}
}
