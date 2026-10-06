package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
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

// helperCandidate is one undecided name. program is set when exactly one
// program is registered under the name; registered lists every registration,
// so a name with several is shown with all of them and chosen, never guessed.
type helperCandidate struct {
	name       string
	program    config.HelperProgram
	registered []config.HelperProgram
	path       string
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
//
// Deny is the keyboard default. It stores nothing: it denies the helpers for
// this open only, and the next direct open asks again, so a stray Return or
// Space costs one open and never grants anything.
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

// helpersForOpen resolves a document's permissions for something the user did:
// opening a file, a tray action, untrusting a folder. A failure is reported on
// screen because someone is waiting on the action that caused it.
func (a *app) helpersForOpen(document string, prompt bool) helperOpenPlan {
	return a.helpersForOpenWith(document, prompt, true, a.systemHelperApprovalDialogs())
}

// helpersForNavigation resolves the same permissions for an HTTP request, which
// nobody asked for directly. It never prompts, so it never takes helperMu and
// cannot hold a page load behind a dialog someone left open elsewhere, and it
// reports a failure to the log alone rather than raising a banner over whatever
// the user is actually doing.
func (a *app) helpersForNavigation(document string) helperOpenPlan {
	return a.helpersForOpenWith(document, false, false, a.systemHelperApprovalDialogs())
}

func (a *app) helpersForOpenWith(document string, prompt, notify bool, dialogs helperApprovalDialogs) helperOpenPlan {
	if prompt {
		a.helperMu.Lock()
		defer a.helperMu.Unlock()
	}

	names, err := readDocumentHelperNames(document)
	if err != nil {
		if notify {
			a.reportHelperSetupError(document, err)
		} else {
			a.rt.logger.Printf("Could not configure helpers for %s: %v", document, err)
		}
		return helperOpenPlan{}
	}
	if prompt {
		a.setOpenDenials(document, nil)
	}
	plan := helperOpenPlan{names: names}
	plan.allowed, plan.denied = a.resolvedHelpers(document, names)
	if !prompt || len(names) == 0 {
		return plan
	}

	undecided := make([]helperCandidate, 0, len(names))
	for _, name := range names {
		resolution, _ := a.rt.cfg.ResolveHelper(document, name)
		if resolution.Decided {
			continue
		}
		undecided = append(undecided, helperCandidate{name: name, program: resolution.Program, registered: resolution.Candidates})
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
	switch choice {
	case platform.ConfirmDismissed:
		a.rt.logger.Printf("Helper permission dialog for %s closed without an answer; it will ask again", document)
		return plan
	case platform.ConfirmDeny:
		refused := make([]string, 0, len(undecided))
		for _, candidate := range undecided {
			refused = append(refused, candidate.name)
		}
		a.setOpenDenials(document, refused)
		a.rt.logger.Printf("Helpers denied for this open of %s; the next direct open asks again", document)
		plan.allowed, plan.denied = a.resolvedHelpers(document, names)
		return plan
	case platform.ConfirmAllowOnce, platform.ConfirmAllowAlways:
	default:
		a.reportHelperSetupError(document, errors.New("the helper permission dialog returned an invalid choice"))
		return plan
	}
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
	lines := make([]string, len(candidates))
	for i, candidate := range candidates {
		lines[i] = helperApprovalLine(candidate)
	}
	label := "program"
	if len(lines) != 1 {
		label = "programs"
	}
	return fmt.Sprintf(
		"%s wants to use %d %s:\n\n%s\n\nThe selected programs run as you and may read or change any file your account can access. Deny applies to this open only.",
		filepath.Base(document), len(lines), label, strings.Join(lines, "\n"),
	)
}

// helperApprovalLine says what allowing a name would run: the registered path,
// every registered path when several share the name, or that none is registered
// yet. The last two open the file picker after an Allow, so nothing is guessed.
func helperApprovalLine(candidate helperCandidate) string {
	switch {
	case candidate.program.ID != "":
		line := candidate.name + ": " + candidate.program.Path
		if helperProgramMissing(candidate.program.Path) {
			line += " (missing)"
		}
		return line
	case len(candidate.registered) > 1:
		line := fmt.Sprintf("%s: %d registered programs share this name, and you will choose which one:", candidate.name, len(candidate.registered))
		for _, program := range candidate.registered {
			line += "\n    " + program.Path
			if helperProgramMissing(program.Path) {
				line += " (missing)"
			}
		}
		return line
	default:
		return candidate.name + ": not registered yet, and you will choose the program"
	}
}

// A name that is in neither result was never decided: nobody was asked, the
// picker was cancelled, or the program behind an old allow is gone.
func (a *app) resolvedHelpers(document string, names []string) (map[string]config.HelperProgram, []string) {
	a.helperStateMu.Lock()
	defer a.helperStateMu.Unlock()
	return a.resolvedHelpersLocked(document, names)
}

func (a *app) resolvedHelpersLocked(document string, names []string) (map[string]config.HelperProgram, []string) {
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
	for _, name := range a.openDenials[document] {
		if _, ok := allowed[name]; ok {
			delete(allowed, name)
		}
		if !slices.Contains(denied, name) {
			denied = append(denied, name)
		}
	}
	return allowed, denied
}

func (a *app) setOpenDenials(document string, names []string) {
	a.helperStateMu.Lock()
	defer a.helperStateMu.Unlock()
	if len(names) == 0 {
		delete(a.openDenials, document)
		return
	}
	if a.openDenials == nil {
		a.openDenials = make(map[string][]string)
	}
	a.openDenials[document] = names
}

func (a *app) saveHelperDecisionSet(document string, candidates []helperCandidate, choice platform.ConfirmChoice) error {
	a.helperStateMu.Lock()
	defer a.helperStateMu.Unlock()
	return a.saveHelperDecisionSetLocked(document, candidates, choice)
}

// saveHelperDecisionSetLocked is saveHelperDecisionSet for a caller that already
// holds helperStateMu, so a registration check and the save share one transaction.
func (a *app) saveHelperDecisionSetLocked(document string, candidates []helperCandidate, choice platform.ConfirmChoice) error {
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

	for i := range candidates {
		if candidates[i].program.ID != "" {
			continue
		}
		if program, ok := registeredAtPath(candidates[i].registered, candidates[i].path); ok {
			candidates[i].program = program
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
			Program:   candidate.program.ID,
			Allowed:   true,
			DecidedAt: now + int64(i),
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
	for _, program := range added {
		a.rt.logger.Printf("Helper program registered: id=%s name=%s path=%s", program.ID, program.Name, program.Path)
	}
	scope := "document"
	if choice == platform.ConfirmAllowAlways {
		scope = "any document"
	}
	for i, candidate := range candidates {
		previous := ""
		if decisionUndo[i].had {
			previous = decisionUndo[i].previous.Program
		}
		a.rt.logger.Printf("Helper binding saved: document=%s name=%s program=%s path=%s scope=%s previous=%s", document, candidate.name, candidate.program.ID, candidate.program.Path, scope, previous)
	}
	return nil
}

// registeredAtPath finds the registration a picked file already belongs to, so
// choosing between programs that share a name reuses the chosen one instead of
// registering it a second time.
func registeredAtPath(programs []config.HelperProgram, path string) (config.HelperProgram, bool) {
	for _, program := range programs {
		if program.Path == path {
			return program, true
		}
	}
	picked, err := os.Stat(path)
	if err != nil {
		return config.HelperProgram{}, false
	}
	for _, program := range programs {
		if info, err := os.Stat(program.Path); err == nil && os.SameFile(info, picked) {
			return program, true
		}
	}
	return config.HelperProgram{}, false
}

func (a *app) applyHelperPlan(s *site, document string, plan helperOpenPlan) {
	a.helperStateMu.Lock()
	defer a.helperStateMu.Unlock()
	if len(plan.names) == 0 {
		s.srv.DetachHelpers(document)
		return
	}
	allowed, denied := a.resolvedHelpersLocked(document, plan.names)
	if err := s.srv.AttachHelpers(document, allowed, denied); err != nil {
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
		a.applyHelperPlan(document.site, document.path, a.helpersForNavigation(document.path))
	}
}

// cancelAIEdits stops every running AI edit on every site, after the AI Editing
// switch is turned off.
func (a *app) cancelAIEdits() {
	a.mu.Lock()
	sites := append([]*site(nil), a.sites...)
	a.mu.Unlock()
	for _, s := range sites {
		if s.srv != nil {
			s.srv.CancelAIEdits()
		}
	}
}

func (a *app) reportHelperSetupError(document string, err error) {
	a.rt.logger.Printf("Could not configure helpers for %s: %v", document, err)
	message := fmt.Sprintf("%s could not configure its programs: %v", filepath.Base(document), err)
	if notifyErr := a.notifyUser("HTML Clay could not configure programs", message); notifyErr != nil {
		a.rt.logger.Printf("Could not show helper notification: %v", notifyErr)
	}
}
