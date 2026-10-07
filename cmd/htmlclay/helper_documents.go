package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/panphora/htmlclay/internal/platform"
	"github.com/panphora/htmlclay/internal/session"
	"github.com/panphora/htmlclay/internal/tray"
)

func helperProgramStatus(path string) string {
	if helperProgramMissing(path) {
		return "program file missing"
	}
	if runtime.GOOS != "windows" && !helperProgramExecutable(path) {
		return "program is not executable"
	}
	return ""
}

func (a *app) helperBindingStatus(document, name string) string {
	a.helperStateMu.Lock()
	denied := slices.Contains(a.openDenials[document], name)
	a.helperStateMu.Unlock()
	if denied {
		return "denied for this open"
	}
	resolution, found := a.rt.cfg.ResolveHelper(document, name)
	if resolution.Program.ID != "" {
		if status := helperProgramStatus(resolution.Program.Path); status != "" {
			return status + ": " + resolution.Program.Path
		}
	}
	if resolution.Allowed {
		return "allowed: " + resolution.Program.Path
	}
	for _, decision := range a.rt.cfg.HelperDecisionList() {
		if decision.Document == document && decision.Name == name && decision.Allowed {
			if _, ok := a.rt.cfg.LookupHelperProgram(decision.Program); !ok {
				return "previous program was removed"
			}
		}
	}
	if !found {
		return "no program registered"
	}
	if len(resolution.Candidates) > 1 {
		return "choose between registered programs"
	}
	return "approval needed: " + resolution.Program.Path
}

func (a *app) helperDocumentRows() []tray.Row {
	a.mu.Lock()
	documents := make(map[string]bool)
	if !a.stopping {
		for _, s := range a.sites {
			for _, registration := range s.sessions.Registrations() {
				documents[registration.Path] = true
			}
		}
	}
	a.mu.Unlock()
	paths := make([]string, 0, len(documents))
	for document := range documents {
		paths = append(paths, document)
	}
	slices.Sort(paths)
	var rows []tray.Row
	for _, document := range paths {
		names, err := readDocumentHelperNames(document)
		if err != nil {
			continue
		}
		for _, name := range names {
			label := document
			if rel, err := filepath.Rel(a.rt.home, document); err == nil && document != a.rt.home && session.EqualOrUnder(document, a.rt.home) {
				label = "~/" + filepath.ToSlash(rel)
			}
			rows = append(rows, tray.Row{
				Path:  document + "\x00" + name,
				Label: label + ": " + name + " (" + a.helperBindingStatus(document, name) + ")",
			})
		}
	}
	return rows
}

func (a *app) helperDocumentOpenLocked(document string) bool {
	if a.stopping {
		return false
	}
	_, _, ok := a.lookupLocked(document)
	return ok
}

func (a *app) configureHelperDocument(key string) []tray.Row {
	return a.configureHelperDocumentWith(key, a.systemHelperApprovalDialogs())
}

func (a *app) configureHelperDocumentWith(key string, dialogs helperApprovalDialogs) []tray.Row {
	a.helperMu.Lock()
	defer a.helperMu.Unlock()
	document, name, ok := strings.Cut(key, "\x00")
	if !ok || document == "" || name == "" {
		return a.helperDocumentRows()
	}
	a.mu.Lock()
	open := a.helperDocumentOpenLocked(document)
	a.mu.Unlock()
	names, err := readDocumentHelperNames(document)
	if !open || err != nil || !slices.Contains(names, name) {
		return a.helperDocumentRows()
	}
	status := a.helperBindingStatus(document, name)
	a.rt.logger.Printf("Native helper configuration requested: document=%s name=%s status=%s", document, name, status)
	path, selected, err := dialogs.choose("Choose the program for " + name + " in " + document + ". Helpers receive requests on stdin; a program that only opens a page is not a helper.")
	if err != nil {
		a.reportHelperProgramError("Could not choose the program", err)
		return a.helperDocumentRows()
	}
	if !selected {
		return a.helperDocumentRows()
	}
	message := fmt.Sprintf("Configure %s for this document:\n\n%s\n\nCurrent status: %s\n\nSelected program:\n%s\n\nAllow for This Document saves this binding for this file. Allow for Any Document also allows the selected program for other documents that request %s. If more than one program has this wider permission for the same helper name, other documents must choose a program. The program runs as you and may read or change any file your account can access.", name, document, status, path, name)
	choice, err := dialogs.confirm("Allow document program?", message, true)
	if err != nil {
		a.reportHelperProgramError("Could not approve the program", err)
		return a.helperDocumentRows()
	}
	if choice != platform.ConfirmAllowOnce && choice != platform.ConfirmAllowAlways {
		return a.helperDocumentRows()
	}
	prepared, err := dialogs.prepare(path)
	if err != nil {
		a.reportHelperProgramError("Could not prepare the program", err)
		return a.helperDocumentRows()
	}
	if !prepared {
		return a.helperDocumentRows()
	}
	names, err = readDocumentHelperNames(document)
	if err == nil && !slices.Contains(names, name) {
		err = errors.New("the document no longer declares this program")
	}
	a.helperStateMu.Lock()
	a.mu.Lock()
	if err == nil && !a.helperDocumentOpenLocked(document) {
		err = errors.New("the document is no longer open")
	}
	if err == nil {
		candidate := helperCandidate{name: name, path: path}
		for _, program := range a.rt.cfg.HelperProgramList() {
			if program.Path != path {
				continue
			}
			if program.Name == name {
				candidate.program = program
				break
			}
			if choice != platform.ConfirmAllowAlways && candidate.program.ID == "" {
				candidate.program = program
			}
		}
		candidates := []helperCandidate{candidate}
		err = a.saveHelperDecisionSetLocked(document, candidates, choice)
		if err == nil {
			denied := a.openDenials[document]
			kept := make([]string, 0, len(denied))
			for _, other := range denied {
				if other != name {
					kept = append(kept, other)
				}
			}
			if len(kept) == 0 {
				delete(a.openDenials, document)
			} else {
				a.openDenials[document] = kept
			}
		}
	}
	a.mu.Unlock()
	a.helperStateMu.Unlock()
	if err != nil {
		a.reportHelperProgramError("Could not configure the document program", err)
	} else {
		a.refreshHelperDispatchers()
	}
	return a.helperDocumentRows()
}
