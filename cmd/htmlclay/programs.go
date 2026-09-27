package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/panphora/htmlclay/internal/config"
	"github.com/panphora/htmlclay/internal/platform"
	"github.com/panphora/htmlclay/internal/tray"
)

const (
	maxProgramFirstLine = 4096
	// refusedRowPrefix marks the tray row for a name that documents refused and
	// no program is registered under. A program ID is 32 hex digits and a name
	// never contains a colon, so the row path cannot collide with either.
	refusedRowPrefix               = "refused:"
	forgetDocumentPermissionsLabel = "Forget document permissions"
	maxRefusalDocumentsShown       = 8
)

type helperProgramDialogs struct {
	promptName        func(title, message, initial string) (string, bool, error)
	selectFile        func(prompt string) (string, bool, error)
	confirmExecutable func(title, message, affirmative string) (bool, error)
	manageProgram     func(platform.ProgramSummary) (platform.ManageChoice, error)
	confirmForget     func(title, message, affirmative string) (bool, error)
}

func systemHelperProgramDialogs() helperProgramDialogs {
	return helperProgramDialogs{
		promptName:        platform.PromptName,
		selectFile:        platform.SelectFile,
		confirmExecutable: platform.ConfirmWithButtons,
		manageProgram:     platform.ManageProgram,
		confirmForget:     platform.ConfirmWithButtons,
	}
}

// helperProgramRows lists one row per registered program and one per refused
// name that has no program. A refusal names no program, so it is shown on every
// row of its name, and a name nobody registered gets a row of its own: without
// it a refusal made before any program was chosen could never be forgotten.
func (a *app) helperProgramRows() []tray.Row {
	programs := a.rt.cfg.HelperProgramList()
	decisions := a.rt.cfg.HelperDecisionList()
	allowed := make(map[string]int, len(programs))
	refused := make(map[string]int)
	var refusedNames []string
	for _, decision := range decisions {
		if decision.Allowed {
			allowed[decision.Program]++
			continue
		}
		if refused[decision.Name] == 0 {
			refusedNames = append(refusedNames, decision.Name)
		}
		refused[decision.Name]++
	}

	rows := make([]tray.Row, 0, len(programs)+len(refusedNames))
	registered := make(map[string]bool, len(programs))
	for _, program := range programs {
		registered[program.Name] = true
		count := allowed[program.ID]
		var status string
		switch {
		case helperProgramMissing(program.Path):
			status = "missing"
		case program.AnyDocument:
			status = "any document"
		case count == 1:
			status = "1 document"
		default:
			status = fmt.Sprintf("%d documents", count)
		}
		rows = append(rows, tray.Row{Path: program.ID, Label: helperRowLabel(program.Name, status, refused[program.Name])})
	}
	for _, name := range refusedNames {
		if !registered[name] {
			rows = append(rows, tray.Row{Path: refusedRowPrefix + name, Label: helperRowLabel(name, "not registered", refused[name])})
		}
	}
	return rows
}

func helperRowLabel(name, status string, refused int) string {
	if refused > 0 {
		status += fmt.Sprintf(", %d refused", refused)
	}
	return name + "  (" + status + ")"
}

func helperProgramMissing(path string) bool {
	info, err := os.Stat(path)
	return err != nil || !info.Mode().IsRegular()
}

func (a *app) pickHelperProgram() []tray.Row {
	return a.pickHelperProgramWith(systemHelperProgramDialogs())
}

func (a *app) pickHelperProgramWith(dialogs helperProgramDialogs) []tray.Row {
	name, ok, err := promptHelperName(dialogs.promptName)
	if err != nil {
		a.reportHelperProgramError("Could not name the program", err)
		return a.helperProgramRows()
	}
	if !ok {
		return a.helperProgramRows()
	}

	path, ok, err := dialogs.selectFile("Choose the program named " + name)
	if err != nil {
		a.reportHelperProgramError("Could not choose the program", err)
		return a.helperProgramRows()
	}
	if !ok {
		return a.helperProgramRows()
	}
	if ok, err := prepareHelperProgram(path, dialogs.confirmExecutable); err != nil {
		a.reportHelperProgramError("Could not add the program", err)
		return a.helperProgramRows()
	} else if !ok {
		return a.helperProgramRows()
	}

	a.helperStateMu.Lock()
	a.mu.Lock()
	program, err := a.rt.cfg.AddHelperProgram(name, path)
	if err == nil {
		err = a.rt.cfg.Save()
		if err != nil {
			a.rt.cfg.RemoveHelperProgram(program.ID)
		}
	}
	a.mu.Unlock()
	a.helperStateMu.Unlock()
	if err != nil {
		a.reportHelperProgramError("Could not add the program", err)
	}
	return a.helperProgramRows()
}

func promptHelperName(prompt func(title, message, initial string) (string, bool, error)) (string, bool, error) {
	message := "Name this program using lowercase letters, numbers, and hyphens. The name must be 1 to 32 characters and cannot start or end with a hyphen."
	initial := ""
	for {
		name, ok, err := prompt("Add a Program", message, initial)
		if err != nil || !ok {
			return "", ok, err
		}
		if config.ValidHelperName(name) {
			return name, true, nil
		}
		message = "That name is not valid. Use 1 to 32 lowercase letters, numbers, and hyphens, with no hyphen at the start or end."
		initial = name
	}
}

func prepareHelperProgram(path string, confirm func(title, message, affirmative string) (bool, error)) (bool, error) {
	return prepareHelperProgramOn(runtime.GOOS, path, confirm)
}

// The platform is a parameter so the Windows extension rule can be exercised
// from any machine. There is no Windows hardware here, and a branch that can
// only run where nobody can test it ships unverified.
func prepareHelperProgramOn(goos, path string, confirm func(title, message, affirmative string) (bool, error)) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, fmt.Errorf("cannot inspect %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("%s is not a file", path)
	}
	if goos == "windows" {
		switch strings.ToLower(filepath.Ext(path)) {
		case ".exe", ".bat", ".cmd":
			return true, nil
		default:
			return false, errors.New("Windows helper programs must be .exe, .bat, or .cmd files; use a .cmd wrapper for scripts")
		}
	}
	if info.Mode()&0111 != 0 {
		return true, nil
	}

	firstLine, err := readProgramFirstLine(path)
	if err != nil {
		return false, fmt.Errorf("cannot read the first line of %s: %w", path, err)
	}
	message := fmt.Sprintf("This file is not executable:\n\n%s\n\nFirst line:\n%q\n\nMake it executable so HTML Clay can run it?", path, firstLine)
	ok, err := confirm("Make Program Executable", message, "Make Executable")
	if err != nil || !ok {
		return false, err
	}
	if err := os.Chmod(path, info.Mode()|0111); err != nil {
		return false, fmt.Errorf("cannot make %s executable: %w", path, err)
	}
	return true, nil
}

func readProgramFirstLine(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	line, err := bufio.NewReader(io.LimitReader(f, maxProgramFirstLine+1)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if len(line) > maxProgramFirstLine {
		line = line[:maxProgramFirstLine] + "…"
	}
	return strings.ToValidUTF8(line, "�"), nil
}

func (a *app) manageHelperProgram(id string) []tray.Row {
	return a.helperProgramRowClicked(id, systemHelperProgramDialogs())
}

func (a *app) helperProgramRowClicked(id string, dialogs helperProgramDialogs) []tray.Row {
	if name, ok := strings.CutPrefix(id, refusedRowPrefix); ok {
		return a.forgetHelperRefusalsWith(name, dialogs.confirmForget)
	}
	return a.manageHelperProgramWith(id, dialogs.manageProgram)
}

func (a *app) manageHelperProgramWith(id string, manage func(platform.ProgramSummary) (platform.ManageChoice, error)) []tray.Row {
	program, ok := a.rt.cfg.LookupHelperProgram(id)
	if !ok {
		return a.helperProgramRows()
	}
	decisions, refusals := 0, 0
	for _, decision := range a.rt.cfg.HelperDecisionList() {
		switch {
		case decision.Allowed && decision.Program == id:
			decisions++
		case !decision.Allowed && decision.Name == program.Name:
			refusals++
		}
	}
	runsOnForget := ""
	if refusals > 0 {
		var anyDocument []config.HelperProgram
		for _, p := range a.rt.cfg.HelperProgramList() {
			if p.Name == program.Name && p.AnyDocument {
				anyDocument = append(anyDocument, p)
			}
		}
		if len(anyDocument) == 1 {
			runsOnForget = anyDocument[0].Path
		}
	}
	choice, err := manage(platform.ProgramSummary{
		Name:         program.Name,
		Path:         program.Path,
		AnyDocument:  program.AnyDocument,
		Decisions:    decisions,
		Refusals:     refusals,
		RunsOnForget: runsOnForget,
		Missing:      helperProgramMissing(program.Path),
	})
	if err != nil {
		a.reportHelperProgramError("Could not manage the program", err)
		return a.helperProgramRows()
	}

	var saveErr error
	a.helperStateMu.Lock()
	a.mu.Lock()
	switch choice {
	case platform.ManageToggleAnyDocument:
		if current, found := a.rt.cfg.LookupHelperProgram(id); found {
			previous, _ := a.rt.cfg.SetHelperAnyDocument(id, !current.AnyDocument)
			if saveErr = a.rt.cfg.Save(); saveErr != nil {
				a.rt.cfg.SetHelperAnyDocument(id, previous)
			}
		}
	case platform.ManageForgetDecisions:
		removed := a.rt.cfg.ForgetHelperProgramDecisions(id)
		if len(removed) > 0 {
			if saveErr = a.rt.cfg.Save(); saveErr != nil {
				a.rt.cfg.RestoreHelperDecisions(removed)
			}
		}
	case platform.ManageRemove:
		removedProgram, found := a.rt.cfg.RemoveHelperProgram(id)
		if found {
			if saveErr = a.rt.cfg.Save(); saveErr != nil {
				a.rt.cfg.RestoreHelperProgram(removedProgram)
			}
		}
	}
	a.mu.Unlock()
	a.helperStateMu.Unlock()
	if saveErr != nil {
		a.reportHelperProgramError("Could not save the program change", saveErr)
	}
	return a.helperProgramRows()
}

// forgetHelperRefusalsWith is the row for a refused name with no program. It
// lists the documents that refused it and, once confirmed, forgets them all, so
// each asks again at its next direct open. Forgetting grants nothing.
func (a *app) forgetHelperRefusalsWith(name string, confirm func(title, message, affirmative string) (bool, error)) []tray.Row {
	for _, program := range a.rt.cfg.HelperProgramList() {
		if program.Name == name {
			return a.helperProgramRows()
		}
	}
	var documents []string
	for _, decision := range a.rt.cfg.HelperDecisionList() {
		if !decision.Allowed && decision.Name == name {
			documents = append(documents, decision.Document)
		}
	}
	if len(documents) == 0 {
		return a.helperProgramRows()
	}
	ok, err := confirm(forgetDocumentPermissionsLabel, helperRefusalMessage(name, documents), forgetDocumentPermissionsLabel)
	if err != nil {
		a.reportHelperProgramError("Could not forget the document permissions", err)
		return a.helperProgramRows()
	}
	if !ok {
		return a.helperProgramRows()
	}

	var saveErr error
	a.helperStateMu.Lock()
	a.mu.Lock()
	if removed := a.rt.cfg.ForgetHelperRefusals(name); len(removed) > 0 {
		if saveErr = a.rt.cfg.Save(); saveErr != nil {
			a.rt.cfg.RestoreHelperDecisions(removed)
		}
	}
	a.mu.Unlock()
	a.helperStateMu.Unlock()
	if saveErr != nil {
		a.reportHelperProgramError("Could not save the program change", saveErr)
	}
	return a.helperProgramRows()
}

func helperRefusalMessage(name string, documents []string) string {
	shown := documents
	more := ""
	if len(shown) > maxRefusalDocumentsShown {
		more = fmt.Sprintf("\nand %d more", len(shown)-maxRefusalDocumentsShown)
		shown = shown[:maxRefusalDocumentsShown]
	}
	return fmt.Sprintf("No program named %s is registered, and these documents refused it:\n\n%s%s\n\nForgetting makes each document ask again the next time you open it.",
		name, strings.Join(shown, "\n"), more)
}

func (a *app) reportHelperProgramError(title string, err error) {
	a.rt.logger.Printf("%s: %v", title, err)
	go func() {
		if notifyErr := a.notifyUser(title, err.Error()); notifyErr != nil {
			a.rt.logger.Printf("Could not show the program error: %v", notifyErr)
		}
	}()
}
