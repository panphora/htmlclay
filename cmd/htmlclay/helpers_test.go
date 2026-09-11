package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/panphora/htmlclay/internal/config"
	"github.com/panphora/htmlclay/internal/platform"
)

func writeHelperPage(t *testing.T, dir string, names ...string) string {
	t.Helper()
	var metas strings.Builder
	for _, name := range names {
		metas.WriteString("<meta name=\"htmlclay-helper\" content=\"" + name + "\">")
	}
	path := filepath.Join(dir, "page.htmlclay")
	if err := os.WriteFile(path, []byte("<!doctype html><head>"+metas.String()+"</head>"), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHelperDenialUsesOneDialogAndPersists(t *testing.T) {
	home := t.TempDir()
	cfgBase := t.TempDir()
	a := newTestAppWithConfigDir(t, home, cfgBase)
	document := writeHelperPage(t, home, "search", "ocr")
	for _, name := range []string{"search", "ocr"} {
		if _, err := a.rt.cfg.AddHelperProgram(name, writeTestProgram(t, t.TempDir(), name, 0755)); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	var message string
	dialogs := helperApprovalDialogs{
		confirm: func(_, text string, broad bool) (platform.ConfirmChoice, error) {
			calls++
			message = text
			if !broad {
				t.Fatal("permission dialog did not offer the broad choice")
			}
			return platform.ConfirmDeny, nil
		},
		choose: func(string) (string, bool, error) {
			t.Fatal("denial opened a file picker")
			return "", false, nil
		},
	}
	for i := 0; i < 2; i++ {
		plan := a.helpersForOpenWith(document, true, dialogs)
		if len(plan.allowed) != 0 {
			t.Fatalf("denied plan allowed helpers: %+v", plan.allowed)
		}
	}
	if calls != 1 {
		t.Fatalf("dialog calls = %d, want 1 across repeated opens", calls)
	}
	if !strings.Contains(message, "search") || !strings.Contains(message, "ocr") {
		t.Fatalf("dialog did not name the full undecided set: %q", message)
	}
	decisions := a.rt.cfg.HelperDecisionList()
	if len(decisions) != 2 || decisions[0].Allowed || decisions[1].Allowed {
		t.Fatalf("persisted decisions = %+v", decisions)
	}

	restarted := newTestAppWithConfigDir(t, home, cfgBase)
	restartedPlan := restarted.helpersForOpenWith(document, true, helperApprovalDialogs{
		confirm: func(string, string, bool) (platform.ConfirmChoice, error) {
			t.Fatal("restart prompted after a persisted denial")
			return platform.ConfirmDeny, nil
		},
	})
	if len(restartedPlan.allowed) != 0 {
		t.Fatalf("restarted denied plan allowed helpers: %+v", restartedPlan.allowed)
	}
}

func TestHelperApprovalSelectsThenPersistsTheProgram(t *testing.T) {
	home := t.TempDir()
	a := newTestApp(t, home)
	document := writeHelperPage(t, home, "search")
	programPath := writeTestProgram(t, t.TempDir(), "search", 0755)
	var order []string
	plan := a.helpersForOpenWith(document, true, helperApprovalDialogs{
		confirm: func(string, string, bool) (platform.ConfirmChoice, error) {
			order = append(order, "confirm")
			return platform.ConfirmAllowAlways, nil
		},
		choose: func(prompt string) (string, bool, error) {
			order = append(order, "choose")
			if !strings.Contains(prompt, "search") {
				t.Fatalf("picker prompt = %q", prompt)
			}
			return programPath, true, nil
		},
		prepare: func(path string) (bool, error) {
			order = append(order, "prepare")
			if path != programPath {
				t.Fatalf("prepared %q, want %q", path, programPath)
			}
			return true, nil
		},
	})
	if strings.Join(order, ",") != "confirm,choose,prepare" {
		t.Fatalf("dialog order = %v", order)
	}
	program, ok := plan.allowed["search"]
	if !ok || program.Path != programPath || !program.AnyDocument {
		t.Fatalf("allowed helper = %+v, %v", program, ok)
	}
	resolution, _ := a.rt.cfg.ResolveHelper(document, "search")
	if !resolution.Decided || !resolution.Allowed || resolution.Program.ID != program.ID {
		t.Fatalf("saved resolution = %+v", resolution)
	}
}

func TestHelperDecisionSaveFailureRollsBackExactly(t *testing.T) {
	tests := []struct {
		name       string
		choice     platform.ConfirmChoice
		registered bool
	}{
		{"new registration", platform.ConfirmAllowOnce, false},
		{"broad flag", platform.ConfirmAllowAlways, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfgBase := t.TempDir()
			a := newTestAppWithConfigDir(t, t.TempDir(), cfgBase)
			path := writeTestProgram(t, t.TempDir(), "search", 0755)
			candidate := helperCandidate{name: "search", path: path}
			if tc.registered {
				program, err := a.rt.cfg.AddHelperProgram("search", path)
				if err != nil {
					t.Fatal(err)
				}
				candidate.program = program
			}
			beforePrograms := a.rt.cfg.HelperProgramList()
			beforeDecisions := a.rt.cfg.HelperDecisionList()
			if err := os.WriteFile(config.DirFrom(cfgBase), []byte("blocks config directory"), 0600); err != nil {
				t.Fatal(err)
			}
			err := a.saveHelperDecisionSet("/docs/page.htmlclay", []helperCandidate{candidate}, tc.choice)
			if err == nil {
				t.Fatal("save failure returned nil")
			}
			if got := a.rt.cfg.HelperProgramList(); !equalHelperPrograms(got, beforePrograms) {
				t.Fatalf("program rollback = %+v, want %+v", got, beforePrograms)
			}
			if got := a.rt.cfg.HelperDecisionList(); !equalHelperDecisions(got, beforeDecisions) {
				t.Fatalf("decision rollback = %+v, want %+v", got, beforeDecisions)
			}
		})
	}
}

func TestStoredHelperDecisionRestoresWithoutDialogs(t *testing.T) {
	home := t.TempDir()
	a := newTestApp(t, home)
	document := writeHelperPage(t, home, "search")
	document, err := resolveSymlinks(document)
	if err != nil {
		t.Fatal(err)
	}
	program, err := a.rt.cfg.AddHelperProgram("search", writeTestProgram(t, t.TempDir(), "search", 0755))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.rt.cfg.DecideHelper(config.HelperDecision{
		Document: document, Name: "search", Program: program.ID, Allowed: true,
	}); err != nil {
		t.Fatal(err)
	}
	fail := errors.New("dialog called")
	plan := a.helpersForOpenWith(document, false, helperApprovalDialogs{
		confirm: func(string, string, bool) (platform.ConfirmChoice, error) {
			return platform.ConfirmDeny, fail
		},
		choose: func(string) (string, bool, error) {
			return "", false, fail
		},
		prepare: func(string) (bool, error) {
			return false, fail
		},
	})
	if got := plan.allowed["search"]; got.ID != program.ID {
		t.Fatalf("restored helper = %+v, want %s", got, program.ID)
	}
}

func TestRefreshingOpenDispatchersAppliesRevocationWithoutPrompt(t *testing.T) {
	home := t.TempDir()
	a := newTestApp(t, home)
	document := writeHelperPage(t, home, "search")
	document, err := resolveSymlinks(document)
	if err != nil {
		t.Fatal(err)
	}
	program, err := a.rt.cfg.AddHelperProgram("search", writeTestProgram(t, t.TempDir(), "search", 0755))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.rt.cfg.DecideHelper(config.HelperDecision{
		Document: document, Name: "search", Program: program.ID, Allowed: true,
	}); err != nil {
		t.Fatal(err)
	}
	s, _ := a.openForTest(t, document)
	a.applyHelperPlan(s, document, a.helpersForOpenWith(document, false, helperApprovalDialogs{}))
	before := s.srv.HelperGeneration(document)
	if before == 0 || !s.srv.HelpersBound(document) {
		t.Fatal("allowed helper did not attach")
	}
	if removed := a.rt.cfg.ForgetHelperProgramDecisions(program.ID); len(removed) != 1 {
		t.Fatalf("removed decisions = %+v", removed)
	}

	a.refreshHelperDispatchers()

	after := s.srv.HelperGeneration(document)
	if after <= before || !s.srv.HelpersBound(document) {
		t.Fatalf("revocation refresh = generation %d, bound %v; before %d", after, s.srv.HelpersBound(document), before)
	}
	file, ok := s.sessions.LookupByPath(document)
	if !ok {
		t.Fatal("reopened document is not registered")
	}
	status, raw := fetch(t, fmt.Sprintf("http://127.0.0.1:%d/_/meta/%s", s.port, file.Token))
	if status != 200 {
		t.Fatalf("meta status = %d: %s", status, raw)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		t.Fatal(err)
	}
	block := meta["document"].(map[string]any)
	helpers := block["helpers"].([]any)
	// Forgetting a document permission is not a refusal. The dispatcher stops
	// allowing the name, and discovery has to say so without telling the page
	// the user said no: the next direct open asks again.
	state := helpers[0].(map[string]any)["state"]
	if state != "unavailable" {
		t.Fatalf("helper state after revocation = %v, want unavailable", state)
	}
}

func equalHelperPrograms(a, b []config.HelperProgram) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalHelperDecisions(a, b []config.HelperDecision) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The document-programs prompt must not run through the read prompt's seam. It
// did, and inherited that dialog's buttons: "Allow Once" over a decision that
// stands until the tray changes it, and "Trust this folder" over a grant that
// covers a PROGRAM, in documents anywhere, and no folder at all.
func TestTheDocumentProgramsPromptHasItsOwnSeamAndButtons(t *testing.T) {
	a := &app{rt: &appRuntime{}}
	a.rt.confirm = func(string, string, bool) (platform.ConfirmChoice, error) {
		t.Error("the document-programs prompt used the read prompt's seam, so it also used its buttons")
		return platform.ConfirmDeny, nil
	}
	calls := 0
	a.rt.confirmHelpers = func(string, string, bool) (platform.ConfirmChoice, error) {
		calls++
		return platform.ConfirmDeny, nil
	}
	if _, err := a.systemHelperApprovalDialogs().confirm("Allow document programs?", "message", true); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("confirmHelpers called %d times, want 1", calls)
	}

	if strings.Contains(helperConfirmLabels.Allow, "Once") ||
		strings.Contains(strings.ToLower(helperConfirmLabels.Always), "folder") {
		t.Fatalf("the document-programs buttons still describe the read prompt's grants: %+v", helperConfirmLabels)
	}
}
