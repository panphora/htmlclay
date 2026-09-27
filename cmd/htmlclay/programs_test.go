package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/panphora/htmlclay/internal/config"
	"github.com/panphora/htmlclay/internal/platform"
)

func writeTestProgram(t *testing.T, dir, name string, mode os.FileMode) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho ok\n"), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHelperProgramRowsCarryIDsAndStates(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	dir := t.TempDir()
	normalPath := writeTestProgram(t, dir, "normal", 0755)
	anyPath := writeTestProgram(t, dir, "any", 0755)

	normal, err := a.rt.cfg.AddHelperProgram("search", normalPath)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := a.rt.cfg.AddHelperProgram("search", filepath.Join(dir, "gone"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a.rt.cfg.SetHelperAnyDocument(missing.ID, true); !ok {
		t.Fatal("missing program was not found")
	}
	any, err := a.rt.cfg.AddHelperProgram("index", anyPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a.rt.cfg.SetHelperAnyDocument(any.ID, true); !ok {
		t.Fatal("any-document program was not found")
	}
	for i, document := range []string{"/docs/one.htmlclay", "/docs/two.htmlclay", "/docs/three.htmlclay"} {
		_, _, err := a.rt.cfg.DecideHelper(config.HelperDecision{
			Document:  document,
			Name:      normal.Name,
			Program:   normal.ID,
			Allowed:   true,
			DecidedAt: int64(i + 1),
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	labels := map[string]string{}
	for _, row := range a.helperProgramRows() {
		labels[row.Path] = row.Label
	}
	want := map[string]string{
		normal.ID:  "search  (3 documents)",
		missing.ID: "search  (missing)",
		any.ID:     "index  (any document)",
	}
	if len(labels) != len(want) {
		t.Fatalf("rows = %#v, want %d", labels, len(want))
	}
	for id, label := range want {
		if labels[id] != label {
			t.Errorf("row %q label = %q, want %q", id, labels[id], label)
		}
	}
}

func TestPickHelperProgramDialogOrderAndCancellation(t *testing.T) {
	tests := []struct {
		name       string
		promptOK   bool
		selectOK   bool
		wantEvents []string
	}{
		{name: "name cancelled", promptOK: false, wantEvents: []string{"name"}},
		{name: "file cancelled", promptOK: true, selectOK: false, wantEvents: []string{"name", "file"}},
		{name: "registered", promptOK: true, selectOK: true, wantEvents: []string{"name", "file"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfgBase := t.TempDir()
			a := newTestAppWithConfigDir(t, t.TempDir(), cfgBase)
			programPath := writeTestProgram(t, t.TempDir(), "search", 0755)
			var events []string
			dialogs := helperProgramDialogs{
				promptName: func(string, string, string) (string, bool, error) {
					events = append(events, "name")
					return "search", tt.promptOK, nil
				},
				selectFile: func(string) (string, bool, error) {
					events = append(events, "file")
					if got := len(a.rt.cfg.HelperProgramList()); got != 0 {
						t.Fatalf("registry changed before file selection: %d entries", got)
					}
					return programPath, tt.selectOK, nil
				},
				confirmExecutable: func(string, string, string) (bool, error) {
					t.Fatal("executable program asked for chmod")
					return false, nil
				},
			}

			rows := a.pickHelperProgramWith(dialogs)
			if strings.Join(events, ",") != strings.Join(tt.wantEvents, ",") {
				t.Fatalf("dialog order = %v, want %v", events, tt.wantEvents)
			}
			programs := a.rt.cfg.HelperProgramList()
			if !tt.selectOK || !tt.promptOK {
				if len(programs) != 0 || len(rows) != 0 {
					t.Fatalf("cancelled registration left programs=%+v rows=%+v", programs, rows)
				}
				return
			}
			if len(programs) != 1 || programs[0].Name != "search" || programs[0].Path != programPath {
				t.Fatalf("programs = %+v", programs)
			}
			if len(rows) != 1 || rows[0].Path != programs[0].ID {
				t.Fatalf("rows = %+v, program ID = %q", rows, programs[0].ID)
			}
			reloaded, _, err := config.LoadFrom(cfgBase, platform.DirIdentity)
			if err != nil {
				t.Fatal(err)
			}
			if got := reloaded.HelperProgramList(); len(got) != 1 || got[0] != programs[0] {
				t.Fatalf("saved programs = %+v, want %+v", got, programs)
			}
		})
	}
}

func TestPickHelperProgramRepromptsInvalidNameBeforeFilePicker(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	programPath := writeTestProgram(t, t.TempDir(), "search", 0755)
	var initials []string
	var events []string
	dialogs := helperProgramDialogs{
		promptName: func(_, _, initial string) (string, bool, error) {
			initials = append(initials, initial)
			events = append(events, "name")
			if len(initials) == 1 {
				return "Search Helper", true, nil
			}
			return "search-helper", true, nil
		},
		selectFile: func(string) (string, bool, error) {
			events = append(events, "file")
			return programPath, true, nil
		},
		confirmExecutable: func(string, string, string) (bool, error) {
			t.Fatal("executable program asked for chmod")
			return false, nil
		},
	}

	a.pickHelperProgramWith(dialogs)
	if got, want := strings.Join(events, ","), "name,name,file"; got != want {
		t.Fatalf("dialog order = %s, want %s", got, want)
	}
	if len(initials) != 2 || initials[1] != "Search Helper" {
		t.Fatalf("prompt initials = %#v", initials)
	}
}

func TestPickHelperProgramOffersChmodWithFirstLine(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows selects runnable extensions instead of executable bits")
	}
	a := newTestApp(t, t.TempDir())
	programPath := writeTestProgram(t, t.TempDir(), "search", 0644)
	var confirmation string
	dialogs := helperProgramDialogs{
		promptName: func(string, string, string) (string, bool, error) {
			return "search", true, nil
		},
		selectFile: func(string) (string, bool, error) {
			return programPath, true, nil
		},
		confirmExecutable: func(_, message, _ string) (bool, error) {
			confirmation = message
			return true, nil
		},
	}

	rows := a.pickHelperProgramWith(dialogs)
	if !strings.Contains(confirmation, `"#!/bin/sh"`) {
		t.Fatalf("confirmation did not show the first line: %q", confirmation)
	}
	info, err := os.Stat(programPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0111 == 0 {
		t.Fatalf("mode = %v, executable bits were not added", info.Mode())
	}
	if len(rows) != 1 || len(a.rt.cfg.HelperProgramList()) != 1 {
		t.Fatalf("program was not registered: rows=%+v", rows)
	}
}

func TestPickHelperProgramStopsOnDialogErrors(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	wantErr := errors.New("dialog failed")
	dialogs := helperProgramDialogs{
		promptName: func(string, string, string) (string, bool, error) {
			return "", false, wantErr
		},
		selectFile: func(string) (string, bool, error) {
			t.Fatal("file picker ran after the name dialog failed")
			return "", false, nil
		},
	}
	a.pickHelperProgramWith(dialogs)
	if got := len(a.rt.cfg.HelperProgramList()); got != 0 {
		t.Fatalf("failed dialog registered %d programs", got)
	}
}

func TestPickHelperProgramRollsBackWhenConfigSaveFails(t *testing.T) {
	cfgBase := t.TempDir()
	a := newTestAppWithConfigDir(t, t.TempDir(), cfgBase)
	programPath := writeTestProgram(t, t.TempDir(), "search", 0755)
	if err := os.WriteFile(filepath.Join(cfgBase, "htmlclay"), []byte("blocks config directory"), 0600); err != nil {
		t.Fatal(err)
	}
	dialogs := helperProgramDialogs{
		promptName: func(string, string, string) (string, bool, error) {
			return "search", true, nil
		},
		selectFile: func(string) (string, bool, error) {
			return programPath, true, nil
		},
		confirmExecutable: func(string, string, string) (bool, error) {
			t.Fatal("executable program asked for chmod")
			return false, nil
		},
	}

	a.pickHelperProgramWith(dialogs)
	if got := a.rt.cfg.HelperProgramList(); len(got) != 0 {
		t.Fatalf("failed save left registrations in memory: %+v", got)
	}
}

func TestManageHelperProgramUsesRegistrationID(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	dir := t.TempDir()
	first, err := a.rt.cfg.AddHelperProgram("search", writeTestProgram(t, dir, "first", 0755))
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.rt.cfg.AddHelperProgram("search", writeTestProgram(t, dir, "second", 0755))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = a.rt.cfg.DecideHelper(config.HelperDecision{
		Document: "/docs/search.htmlclay",
		Name:     second.Name,
		Program:  second.ID,
		Allowed:  true,
	})
	if err != nil {
		t.Fatal(err)
	}

	rows := a.manageHelperProgramWith(second.ID, func(summary platform.ProgramSummary) (platform.ManageChoice, error) {
		if summary.Name != second.Name || summary.Path != second.Path || summary.Decisions != 1 || summary.Missing {
			t.Fatalf("summary = %+v", summary)
		}
		return platform.ManageRemove, nil
	})
	if _, ok := a.rt.cfg.LookupHelperProgram(second.ID); ok {
		t.Fatal("clicked registration was not removed")
	}
	if got, ok := a.rt.cfg.LookupHelperProgram(first.ID); !ok || got.Path != first.Path {
		t.Fatalf("same-named registration was changed: %+v, %v", got, ok)
	}
	if len(rows) != 1 || rows[0].Path != first.ID {
		t.Fatalf("rows after removal = %+v", rows)
	}
}

func TestManageHelperProgramActions(t *testing.T) {
	tests := []struct {
		name          string
		choice        platform.ManageChoice
		wantProgram   bool
		wantAny       bool
		wantDecisions int
	}{
		{name: "cancel", choice: platform.ManageCancel, wantProgram: true, wantDecisions: 1},
		{name: "toggle any document", choice: platform.ManageToggleAnyDocument, wantProgram: true, wantAny: true, wantDecisions: 1},
		{name: "forget decisions", choice: platform.ManageForgetDecisions, wantProgram: true},
		// Removing a program is not forgetting its decisions. The rows survive and
		// read as undecided, which is what lets re-registering the same ID restore
		// them, and what stops a menu item that says "remove the program" from also
		// clearing a refusal the user meant to be permanent. Forgetting is the row
		// above, and is its own tray action.
		{name: "remove", choice: platform.ManageRemove, wantDecisions: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfgBase := t.TempDir()
			a := newTestAppWithConfigDir(t, t.TempDir(), cfgBase)
			program, err := a.rt.cfg.AddHelperProgram("search", writeTestProgram(t, t.TempDir(), "search", 0755))
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = a.rt.cfg.DecideHelper(config.HelperDecision{
				Document: "/docs/search.htmlclay",
				Name:     program.Name,
				Program:  program.ID,
				Allowed:  true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := a.rt.cfg.Save(); err != nil {
				t.Fatal(err)
			}

			a.manageHelperProgramWith(program.ID, func(platform.ProgramSummary) (platform.ManageChoice, error) {
				return tt.choice, nil
			})
			got, exists := a.rt.cfg.LookupHelperProgram(program.ID)
			if exists != tt.wantProgram {
				t.Fatalf("program exists = %v, want %v", exists, tt.wantProgram)
			}
			if exists && got.AnyDocument != tt.wantAny {
				t.Fatalf("AnyDocument = %v, want %v", got.AnyDocument, tt.wantAny)
			}
			if count := len(a.rt.cfg.HelperDecisionList()); count != tt.wantDecisions {
				t.Fatalf("decisions = %d, want %d", count, tt.wantDecisions)
			}
			reloaded, _, err := config.LoadFrom(cfgBase, platform.DirIdentity)
			if err != nil {
				t.Fatal(err)
			}
			saved, exists := reloaded.LookupHelperProgram(program.ID)
			if exists != tt.wantProgram {
				t.Fatalf("saved program exists = %v, want %v", exists, tt.wantProgram)
			}
			if exists && saved.AnyDocument != tt.wantAny {
				t.Fatalf("saved AnyDocument = %v, want %v", saved.AnyDocument, tt.wantAny)
			}
			if count := len(reloaded.HelperDecisionList()); count != tt.wantDecisions {
				t.Fatalf("saved decisions = %d, want %d", count, tt.wantDecisions)
			}
		})
	}
}

func TestManageHelperProgramRollsBackWhenConfigSaveFails(t *testing.T) {
	for _, tt := range []struct {
		name   string
		choice platform.ManageChoice
	}{
		{name: "toggle", choice: platform.ManageToggleAnyDocument},
		{name: "forget", choice: platform.ManageForgetDecisions},
		{name: "remove", choice: platform.ManageRemove},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfgBase := t.TempDir()
			a := newTestAppWithConfigDir(t, t.TempDir(), cfgBase)
			program, err := a.rt.cfg.AddHelperProgram("search", writeTestProgram(t, t.TempDir(), "search", 0755))
			if err != nil {
				t.Fatal(err)
			}
			decision := config.HelperDecision{
				Document:  "/docs/search.htmlclay",
				Name:      program.Name,
				Program:   program.ID,
				Allowed:   true,
				DecidedAt: 42,
			}
			if _, _, err := a.rt.cfg.DecideHelper(decision); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cfgBase, "htmlclay"), []byte("blocks config directory"), 0600); err != nil {
				t.Fatal(err)
			}

			a.manageHelperProgramWith(program.ID, func(platform.ProgramSummary) (platform.ManageChoice, error) {
				return tt.choice, nil
			})
			if got, ok := a.rt.cfg.LookupHelperProgram(program.ID); !ok || got != program {
				t.Fatalf("program rollback = %+v, %v, want %+v", got, ok, program)
			}
			if got := a.rt.cfg.HelperDecisionList(); len(got) != 1 || got[0] != decision {
				t.Fatalf("decision rollback = %+v, want %+v", got, decision)
			}
		})
	}
}

func TestPrepareHelperProgramWindowsAcceptsOnlyLaunchableExtensions(t *testing.T) {
	// PATHEXT lists .js and Windows would run it under Windows Script Host
	// rather than node, so a node helper with a correct shebang must be refused
	// here instead of failing strangely at launch.
	dir := t.TempDir()
	refuse := func(string, string, string) (bool, error) {
		t.Fatal("the Windows branch must never reach the executable-bit prompt")
		return false, nil
	}
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"helper.exe", true},
		{"helper.bat", true},
		{"helper.cmd", true},
		{"HELPER.CMD", true},
		{"helper.js", false},
		{"helper.ps1", false},
		{"helper.py", false},
		{"helper", false},
	} {
		path := filepath.Join(dir, tc.name)
		if err := os.WriteFile(path, []byte("echo hi\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		ok, err := prepareHelperProgramOn("windows", path, refuse)
		if ok != tc.want {
			t.Errorf("prepareHelperProgramOn(windows, %s) = %v, want %v (err %v)", tc.name, ok, tc.want, err)
		}
		if !tc.want && err == nil {
			t.Errorf("%s was refused with no explanation; the refusal must say to use a .cmd wrapper", tc.name)
		}
		if !tc.want && err != nil && !strings.Contains(err.Error(), ".cmd wrapper") {
			t.Errorf("%s refusal does not say what to do: %v", tc.name, err)
		}
	}
}
