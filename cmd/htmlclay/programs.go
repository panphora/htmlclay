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

const maxProgramFirstLine = 4096

type helperProgramDialogs struct {
	promptName        func(title, message, initial string) (string, bool, error)
	selectFile        func(prompt string) (string, bool, error)
	confirmExecutable func(title, message, affirmative string) (bool, error)
	manageProgram     func(platform.ProgramSummary) (platform.ManageChoice, error)
}

func systemHelperProgramDialogs() helperProgramDialogs {
	return helperProgramDialogs{
		promptName:        platform.PromptName,
		selectFile:        platform.SelectFile,
		confirmExecutable: platform.ConfirmWithButtons,
		manageProgram:     platform.ManageProgram,
	}
}

func (a *app) helperProgramRows() []tray.Row {
	programs := a.rt.cfg.HelperProgramList()
	decisions := a.rt.cfg.HelperDecisionList()
	decisionCount := make(map[string]int, len(programs))
	for _, decision := range decisions {
		if decision.Allowed {
			decisionCount[decision.Program]++
		}
	}

	rows := make([]tray.Row, 0, len(programs))
	for _, program := range programs {
		count := decisionCount[program.ID]
		missing := helperProgramMissing(program.Path)
		label := program.Name
		switch {
		case missing:
			label += "  (missing)"
		case program.AnyDocument:
			label += "  (any document)"
		case count == 1:
			label += "  (1 document)"
		default:
			label += fmt.Sprintf("  (%d documents)", count)
		}
		rows = append(rows, tray.Row{Path: program.ID, Label: label})
	}
	return rows
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
	return a.manageHelperProgramWith(id, systemHelperProgramDialogs().manageProgram)
}

func (a *app) manageHelperProgramWith(id string, manage func(platform.ProgramSummary) (platform.ManageChoice, error)) []tray.Row {
	program, ok := a.rt.cfg.LookupHelperProgram(id)
	if !ok {
		return a.helperProgramRows()
	}
	decisions := 0
	for _, decision := range a.rt.cfg.HelperDecisionList() {
		if decision.Allowed && decision.Program == id {
			decisions++
		}
	}
	choice, err := manage(platform.ProgramSummary{
		Name:        program.Name,
		Path:        program.Path,
		AnyDocument: program.AnyDocument,
		Decisions:   decisions,
		Missing:     helperProgramMissing(program.Path),
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

func (a *app) reportHelperProgramError(title string, err error) {
	a.rt.logger.Printf("%s: %v", title, err)
	go func() {
		if notifyErr := a.notifyUser(title, err.Error()); notifyErr != nil {
			a.rt.logger.Printf("Could not show the program error: %v", notifyErr)
		}
	}()
}
