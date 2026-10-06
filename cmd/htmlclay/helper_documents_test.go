package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/panphora/htmlclay/internal/config"
	"github.com/panphora/htmlclay/internal/platform"
	"github.com/panphora/htmlclay/internal/session"
	"github.com/panphora/htmlclay/internal/testutil"
	"github.com/panphora/htmlclay/internal/tray"
)

func helperDocumentKey(document, name string) string {
	return document + "\x00" + name
}

// allowDocumentDialogs picks path and answers with choice, reporting what the
// picker and the prompt were asked.
func allowDocumentDialogs(path string, choice platform.ConfirmChoice, seen *[]string) helperApprovalDialogs {
	return helperApprovalDialogs{
		choose: func(prompt string) (string, bool, error) {
			if seen != nil {
				*seen = append(*seen, "choose:"+prompt)
			}
			return path, true, nil
		},
		confirm: func(title, message string, allowBroad bool) (platform.ConfirmChoice, error) {
			if seen != nil {
				*seen = append(*seen, "confirm:"+title+":"+message)
			}
			return choice, nil
		},
		prepare: func(prepared string) (bool, error) {
			if seen != nil {
				*seen = append(*seen, "prepare:"+prepared)
			}
			return true, nil
		},
	}
}

// recordingHelperProgram is a harmless program that proves it ran by appending to
// marker, and answers with one structured result.
func recordingHelperProgram(t *testing.T, dir, marker string) string {
	t.Helper()
	path := filepath.Join(dir, "recorder")
	script := "#!/bin/sh\ncat > /dev/null\necho ran >> '" + marker + "'\nprintf '{\"type\":\"result\",\"value\":{\"ran\":true}}\\n'\n"
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

func wireHelperRequest(t *testing.T, s *site, token, id, document string) string {
	t.Helper()
	body := fmt.Sprintf(`{"v":1,"type":"wire/request","id":%q,"file":%q,"helper":"search","document":"none","payload":{"query":"clay"}}`, id, document)
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/_/wire/send", s.port), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Save-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /_/wire/send: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("wire send = %d: %s", resp.StatusCode, out)
	}
	return string(out)
}

func openDenialsOf(t *testing.T, a *app, document string) []string {
	t.Helper()
	a.helperStateMu.Lock()
	defer a.helperStateMu.Unlock()
	return append([]string(nil), a.openDenials[document]...)
}

func helperRowByPath(rows []tray.Row, path string) (tray.Row, bool) {
	for _, row := range rows {
		if row.Path == path {
			return row, true
		}
	}
	return tray.Row{}, false
}

// A document reached through a bookmark or a trusted-folder navigation has to be
// configurable without going back through Finder: the row is built from the
// registrations the app already holds, and the navigation alone registers it.
func TestHelperDocumentRowsComeFromBookmarkNavigationWithoutAnOsOpen(t *testing.T) {
	home, err := resolveSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	folder := filepath.Join(home, "proj")
	document := filepath.Join(folder, "page.htmlclay")
	writeTestFile(t, document, `<!doctype html><head><meta name="htmlclay-helper" content="search"></head>`)
	opened := filepath.Join(folder, "opened.htmlclay")
	writeTestFile(t, opened, `<!doctype html><head><meta name="htmlclay-helper" content="ocr"></head>`)
	quiet := filepath.Join(folder, "quiet.htmlclay")
	writeTestFile(t, quiet, `<!doctype html><head></head>`)

	programPath := writeTestProgram(t, t.TempDir(), "search", 0755)
	a := newTestApp(t, home)
	if _, err := a.rt.cfg.AddHelperProgram("search", programPath); err != nil {
		t.Fatal(err)
	}
	if err := a.trustFolder(folder); err != nil {
		t.Fatal(err)
	}
	s := liveSite(t, a, folder)
	if _, ok := s.sessions.LookupByPath(document); ok {
		t.Fatal("the fixture registered the document before any navigation")
	}

	if status, body := fetchNav(t, fileURL(s.port, filepath.Join("proj", "page.htmlclay"))); status != 200 {
		t.Fatalf("bookmark navigation = %d: %s", status, body)
	}
	if _, ok := s.sessions.LookupByPath(document); !ok {
		t.Fatal("the navigation did not register the document")
	}
	if via := s.sessions.Via(document); via != session.ViaTrusted {
		t.Fatalf("provenance = %v, want only ViaTrusted: a bookmark must not need an OS open", via)
	}
	a.openForTest(t, opened)
	a.openForTest(t, quiet)

	rows := a.helperDocumentRows()
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want one row per declaring open document", rows)
	}
	bookmarked, ok := helperRowByPath(rows, helperDocumentKey(document, "search"))
	if !ok {
		t.Fatalf("no row for the bookmark-opened document: %+v", rows)
	}
	want := "~/proj/page.htmlclay: search (approval needed: " + programPath + ")"
	if bookmarked.Label != want {
		t.Fatalf("row label = %q, want %q", bookmarked.Label, want)
	}
	osOpened, ok := helperRowByPath(rows, helperDocumentKey(opened, "ocr"))
	if !ok {
		t.Fatalf("no row for the opened document: %+v", rows)
	}
	if want := "~/proj/opened.htmlclay: ocr (no program registered)"; osOpened.Label != want {
		t.Fatalf("row label = %q, want %q", osOpened.Label, want)
	}
}

// Choosing a file that is already registered under another name binds this
// document to that registration: no second registration, and the binding
// survives a restart because it is stored, not held in the tray.
func TestHelperDocumentConfigureReusesAnExistingRegistration(t *testing.T) {
	home, err := resolveSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfgBase := t.TempDir()
	a := newTestAppWithConfigDir(t, home, cfgBase)
	document := writeHelperPage(t, home, "search")
	programPath := writeTestProgram(t, t.TempDir(), "other", 0755)
	program, err := a.rt.cfg.AddHelperProgram("other", programPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.rt.cfg.AddHelperProgram("unrelated", writeTestProgram(t, t.TempDir(), "unrelated", 0755)); err != nil {
		t.Fatal(err)
	}
	if err := a.rt.cfg.Save(); err != nil {
		t.Fatal(err)
	}
	a.openForTest(t, document)

	rows := a.configureHelperDocumentWith(helperDocumentKey(document, "search"), allowDocumentDialogs(programPath, platform.ConfirmAllowOnce, nil))
	if len(rows) != 1 || rows[0].Label != "~/page.htmlclay: search (allowed: "+programPath+")" {
		t.Fatalf("rows after the allow = %+v", rows)
	}
	programs := a.rt.cfg.HelperProgramList()
	if len(programs) != 2 {
		t.Fatalf("registrations = %+v, want the chosen one reused, not added again", programs)
	}
	kept, ok := a.rt.cfg.LookupHelperProgram(program.ID)
	if !ok || kept.Path != programPath || kept.Name != "other" {
		t.Fatalf("the chosen registration changed: %+v", kept)
	}
	decisions := a.rt.cfg.HelperDecisionList()
	if len(decisions) != 1 || decisions[0].Document != document || decisions[0].Name != "search" || decisions[0].Program != program.ID || !decisions[0].Allowed {
		t.Fatalf("decisions = %+v", decisions)
	}

	restarted := newTestAppWithConfigDir(t, home, cfgBase)
	resolution, found := restarted.rt.cfg.ResolveHelper(document, "search")
	if !found || !resolution.Decided || !resolution.Allowed || resolution.Program.ID != program.ID {
		t.Fatalf("resolution after a restart = %+v, %v", resolution, found)
	}
}

// Configuring has to reach the dispatcher that is already attached to the open
// document, not only the config: the program must run.
func TestHelperDocumentConfigureMakesTheOpenDispatcherRunTheProgram(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture program is a POSIX shell script")
	}
	home, err := resolveSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := newTestApp(t, home)
	document := writeHelperPage(t, home, "search")
	marker := filepath.Join(t.TempDir(), "ran")
	programPath := recordingHelperProgram(t, t.TempDir(), marker)
	if _, err := a.rt.cfg.AddHelperProgram("search", programPath); err != nil {
		t.Fatal(err)
	}
	s, _ := a.openForTest(t, document)
	a.applyHelperPlan(s, document, a.helpersForOpen(document, false))
	if !s.srv.HelpersBound(document) {
		t.Fatal("the open document has no dispatcher to refresh")
	}
	f, ok := s.sessions.LookupByPath(document)
	if !ok {
		t.Fatal("the document is not registered")
	}
	if got := wireHelperRequest(t, s, f.Token, "before", document); !strings.Contains(got, `"delivered":1`) {
		t.Fatalf("the request did not reach the dispatcher: %s", got)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a helper with no decision ran")
	}

	a.configureHelperDocumentWith(helperDocumentKey(document, "search"), allowDocumentDialogs(programPath, platform.ConfirmAllowOnce, nil))

	if got := wireHelperRequest(t, s, f.Token, "after", document); !strings.Contains(got, `"delivered":1`) {
		t.Fatalf("the refreshed dispatcher did not accept the request: %s", got)
	}
	testutil.Eventually(t, 5*time.Second, "the refreshed dispatcher to run the configured program", func() bool {
		data, err := os.ReadFile(marker)
		return err == nil && strings.Contains(string(data), "ran")
	})
}

// The wider grant is only ever made by the user choosing it. "Allow for This
// Document" binds the name to the program for this file and leaves every other
// document alone.
func TestHelperDocumentAnyDocumentGrantNeedsTheExplicitChoice(t *testing.T) {
	for _, tc := range []struct {
		name        string
		choice      platform.ConfirmChoice
		anyDocument bool
		allowed     bool
	}{
		{"this document", platform.ConfirmAllowOnce, false, true},
		{"any document", platform.ConfirmAllowAlways, true, true},
		{"denied", platform.ConfirmDeny, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, err := resolveSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			a := newTestApp(t, home)
			document := writeHelperPage(t, home, "search")
			programPath := writeTestProgram(t, t.TempDir(), "search", 0755)
			program, err := a.rt.cfg.AddHelperProgram("search", programPath)
			if err != nil {
				t.Fatal(err)
			}
			a.openForTest(t, document)
			seen := []string{}
			a.configureHelperDocumentWith(helperDocumentKey(document, "search"), allowDocumentDialogs(programPath, tc.choice, &seen))

			if tc.choice == platform.ConfirmDeny {
				for _, call := range seen {
					if strings.HasPrefix(call, "prepare:") {
						t.Fatalf("a refusal prepared a program: %v", seen)
					}
				}
			}
			message := ""
			for _, call := range seen {
				if strings.HasPrefix(call, "confirm:Allow document program?:") {
					message = strings.TrimPrefix(call, "confirm:Allow document program?:")
				}
			}
			for _, want := range []string{document, "search", programPath, "approval needed: " + programPath} {
				if !strings.Contains(message, want) {
					t.Errorf("the prompt does not state %q:\n%s", want, message)
				}
			}

			kept, ok := a.rt.cfg.LookupHelperProgram(program.ID)
			if !ok || kept.AnyDocument != tc.anyDocument {
				t.Fatalf("any-document flag = %+v, want %v", kept, tc.anyDocument)
			}
			resolution, _ := a.rt.cfg.ResolveHelper(document, "search")
			if resolution.Allowed != tc.allowed {
				t.Fatalf("resolution = %+v, want allowed %v", resolution, tc.allowed)
			}
			decisions := a.rt.cfg.HelperDecisionList()
			if tc.allowed != (len(decisions) == 1) {
				t.Fatalf("decisions = %+v, want a stored decision: %v", decisions, tc.allowed)
			}
		})
	}
}

// Cancelling the picker, refusing the prompt, or failing to prepare the program
// stores nothing and must leave this open's own refusals exactly as they were.
func TestHelperDocumentCancelledOrFailedDialogsChangeNothing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		dialogs  func(t *testing.T, path string) helperApprovalDialogs
		reported bool
	}{
		{"picker cancelled", func(t *testing.T, _ string) helperApprovalDialogs {
			return helperApprovalDialogs{
				choose: func(string) (string, bool, error) { return "", false, nil },
				confirm: func(string, string, bool) (platform.ConfirmChoice, error) {
					t.Error("a cancelled picker asked for approval")
					return platform.ConfirmAllowOnce, nil
				},
				prepare: func(string) (bool, error) {
					t.Error("a cancelled picker prepared a program")
					return false, nil
				},
			}
		}, false},
		{"picker failed", func(t *testing.T, _ string) helperApprovalDialogs {
			return helperApprovalDialogs{
				choose: func(string) (string, bool, error) { return "", false, errHelperFixture },
			}
		}, true},
		{"approval refused", func(t *testing.T, path string) helperApprovalDialogs {
			return helperApprovalDialogs{
				choose:  func(string) (string, bool, error) { return path, true, nil },
				confirm: func(string, string, bool) (platform.ConfirmChoice, error) { return platform.ConfirmDeny, nil },
				prepare: func(string) (bool, error) {
					t.Error("a refusal prepared a program")
					return false, nil
				},
			}
		}, false},
		{"approval dismissed", func(t *testing.T, path string) helperApprovalDialogs {
			return helperApprovalDialogs{
				choose:  func(string) (string, bool, error) { return path, true, nil },
				confirm: func(string, string, bool) (platform.ConfirmChoice, error) { return platform.ConfirmDismissed, nil },
			}
		}, false},
		{"approval failed", func(t *testing.T, path string) helperApprovalDialogs {
			return helperApprovalDialogs{
				choose: func(string) (string, bool, error) { return path, true, nil },
				confirm: func(string, string, bool) (platform.ConfirmChoice, error) {
					return platform.ConfirmDismissed, errHelperFixture
				},
			}
		}, true},
		{"prepare refused", func(t *testing.T, path string) helperApprovalDialogs {
			return helperApprovalDialogs{
				choose:  func(string) (string, bool, error) { return path, true, nil },
				confirm: func(string, string, bool) (platform.ConfirmChoice, error) { return platform.ConfirmAllowOnce, nil },
				prepare: func(string) (bool, error) { return false, nil },
			}
		}, false},
		{"prepare failed", func(t *testing.T, path string) helperApprovalDialogs {
			return helperApprovalDialogs{
				choose:  func(string) (string, bool, error) { return path, true, nil },
				confirm: func(string, string, bool) (platform.ConfirmChoice, error) { return platform.ConfirmAllowOnce, nil },
				prepare: func(string) (bool, error) { return false, errHelperFixture },
			}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, err := resolveSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			a := newTestApp(t, home)
			var reported atomic.Int32
			a.rt.notify = func(string, string) error {
				reported.Add(1)
				return nil
			}
			document := writeHelperPage(t, home, "search")
			programPath := writeTestProgram(t, t.TempDir(), "search", 0755)
			if _, err := a.rt.cfg.AddHelperProgram("search", programPath); err != nil {
				t.Fatal(err)
			}
			a.openForTest(t, document)
			a.setOpenDenials(document, []string{"search"})
			beforePrograms := a.rt.cfg.HelperProgramList()
			beforeDecisions := a.rt.cfg.HelperDecisionList()

			a.configureHelperDocumentWith(helperDocumentKey(document, "search"), tc.dialogs(t, programPath))

			if got := a.rt.cfg.HelperProgramList(); !equalHelperPrograms(got, beforePrograms) {
				t.Fatalf("programs = %+v, want %+v", got, beforePrograms)
			}
			if got := a.rt.cfg.HelperDecisionList(); !equalHelperDecisions(got, beforeDecisions) {
				t.Fatalf("decisions = %+v, want %+v", got, beforeDecisions)
			}
			if got := openDenialsOf(t, a, document); strings.Join(got, ",") != "search" {
				t.Fatalf("this open's refusal changed: %v", got)
			}
			if resolution, _ := a.rt.cfg.ResolveHelper(document, "search"); resolution.Allowed {
				t.Fatalf("resolution = %+v, want nothing granted", resolution)
			}
			if tc.reported {
				testutil.Eventually(t, 5*time.Second, "the failure to reach the user", func() bool { return reported.Load() > 0 })
			}
			if !tc.reported && reported.Load() != 0 {
				t.Fatalf("a dialog the user closed raised %d notifications", reported.Load())
			}
		})
	}
}

// A config write that fails leaves no registration, no decision, no wider grant
// and no lost refusal behind.
func TestHelperDocumentSaveFailureRollsBackAndKeepsTheDenial(t *testing.T) {
	home, err := resolveSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfgBase := t.TempDir()
	a := newTestAppWithConfigDir(t, home, cfgBase)
	var reported atomic.Int32
	a.rt.notify = func(string, string) error {
		reported.Add(1)
		return nil
	}
	document := writeHelperPage(t, home, "search")
	programPath := writeTestProgram(t, t.TempDir(), "other", 0755)
	program, err := a.rt.cfg.AddHelperProgram("other", programPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.DirFrom(cfgBase), []byte("blocks the config directory"), 0600); err != nil {
		t.Fatal(err)
	}
	a.openForTest(t, document)
	a.setOpenDenials(document, []string{"search"})

	a.configureHelperDocumentWith(helperDocumentKey(document, "search"), allowDocumentDialogs(programPath, platform.ConfirmAllowAlways, nil))

	if got := a.rt.cfg.HelperProgramList(); len(got) != 1 || got[0].ID != program.ID {
		t.Fatalf("registrations after a failed save = %+v", got)
	}
	kept, ok := a.rt.cfg.LookupHelperProgram(program.ID)
	if !ok || kept.AnyDocument {
		t.Fatalf("the wider grant survived a failed save: %+v", kept)
	}
	if got := a.rt.cfg.HelperDecisionList(); len(got) != 0 {
		t.Fatalf("decisions after a failed save = %+v", got)
	}
	if got := openDenialsOf(t, a, document); strings.Join(got, ",") != "search" {
		t.Fatalf("this open's refusal was dropped by a failed save: %v", got)
	}
	testutil.Eventually(t, 5*time.Second, "the failed save to reach the user", func() bool { return reported.Load() > 0 })
}

// A name whose registration is gone, and a name two registrations share, are
// both stated as such and neither grants anything by itself.
func TestHelperDocumentStatusReportsRemovedAndDuplicateRegistrations(t *testing.T) {
	t.Run("previous program removed", func(t *testing.T) {
		home, err := resolveSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		a := newTestApp(t, home)
		document := writeHelperPage(t, home, "search")
		program, err := a.rt.cfg.AddHelperProgram("search", writeTestProgram(t, t.TempDir(), "search", 0755))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := a.rt.cfg.DecideHelper(config.HelperDecision{
			Document: document, Name: "search", Program: program.ID, Allowed: true,
		}); err != nil {
			t.Fatal(err)
		}
		if _, ok := a.rt.cfg.RemoveHelperProgram(program.ID); !ok {
			t.Fatal("the registration was not removed")
		}
		s, _ := a.openForTest(t, document)
		a.applyHelperPlan(s, document, a.helpersForOpen(document, false))

		if got := a.helperBindingStatus(document, "search"); got != "previous program was removed" {
			t.Fatalf("status = %q", got)
		}
		rows := a.helperDocumentRows()
		row, ok := helperRowByPath(rows, helperDocumentKey(document, "search"))
		if !ok || !strings.HasSuffix(row.Label, "(previous program was removed)") {
			t.Fatalf("row = %+v", rows)
		}
		if resolution, _ := a.rt.cfg.ResolveHelper(document, "search"); resolution.Allowed {
			t.Fatalf("a removed registration still granted the name: %+v", resolution)
		}
		if state := helperDiscoveryState(t, s, document); state != "unavailable" {
			t.Fatalf("helper state = %q, want unavailable", state)
		}
	})

	t.Run("duplicate registrations", func(t *testing.T) {
		home, err := resolveSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		a := newTestApp(t, home)
		document := writeHelperPage(t, home, "search")
		for _, name := range []string{"first", "second"} {
			if _, err := a.rt.cfg.AddHelperProgram("search", writeTestProgram(t, t.TempDir(), name, 0755)); err != nil {
				t.Fatal(err)
			}
		}
		s, _ := a.openForTest(t, document)
		a.applyHelperPlan(s, document, a.helpersForOpen(document, false))

		if got := a.helperBindingStatus(document, "search"); got != "choose between registered programs" {
			t.Fatalf("status = %q", got)
		}
		if resolution, _ := a.rt.cfg.ResolveHelper(document, "search"); resolution.Allowed || resolution.Program.ID != "" {
			t.Fatalf("two registrations resolved to one program without asking: %+v", resolution)
		}
		if state := helperDiscoveryState(t, s, document); state != "unavailable" {
			t.Fatalf("helper state = %q, want unavailable", state)
		}
	})
}

// Replacing a binding the user picked wrongly is an explicit choice, and it
// touches nothing else: the other names in this document and the decisions in
// other documents stay exactly as they were.
func TestHelperDocumentReplacingAWrongBindingKeepsUnrelatedDecisions(t *testing.T) {
	home, err := resolveSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := newTestApp(t, home)
	document := writeHelperPage(t, home, "search", "ocr")
	other := filepath.Join(home, "other.htmlclay")
	writeTestFile(t, other, `<!doctype html><head><meta name="htmlclay-helper" content="search"></head>`)
	wrong, err := a.rt.cfg.AddHelperProgram("search", writeTestProgram(t, t.TempDir(), "wrong", 0755))
	if err != nil {
		t.Fatal(err)
	}
	rightPath := writeTestProgram(t, t.TempDir(), "right", 0755)
	right, err := a.rt.cfg.AddHelperProgram("search", rightPath)
	if err != nil {
		t.Fatal(err)
	}
	ocrPath := writeTestProgram(t, t.TempDir(), "ocr", 0755)
	ocr, err := a.rt.cfg.AddHelperProgram("ocr", ocrPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, decision := range []config.HelperDecision{
		{Document: document, Name: "search", Program: wrong.ID, Allowed: true},
		{Document: document, Name: "ocr", Program: ocr.ID, Allowed: true},
		{Document: other, Name: "search", Program: wrong.ID, Allowed: true},
	} {
		if _, _, err := a.rt.cfg.DecideHelper(decision); err != nil {
			t.Fatal(err)
		}
	}
	a.openForTest(t, document)

	rows := a.configureHelperDocumentWith(helperDocumentKey(document, "search"), allowDocumentDialogs(rightPath, platform.ConfirmAllowOnce, nil))

	resolution, _ := a.rt.cfg.ResolveHelper(document, "search")
	if !resolution.Allowed || resolution.Program.ID != right.ID {
		t.Fatalf("the replaced binding = %+v, want %s", resolution, right.ID)
	}
	want := map[string]string{
		document + "/search": right.ID,
		document + "/ocr":    ocr.ID,
		other + "/search":    wrong.ID,
	}
	got := map[string]string{}
	for _, decision := range a.rt.cfg.HelperDecisionList() {
		got[decision.Document+"/"+decision.Name] = decision.Program
	}
	if len(got) != len(want) {
		t.Fatalf("decisions = %+v, want %+v", got, want)
	}
	for key, program := range want {
		if got[key] != program {
			t.Errorf("decision %s = %q, want %q", key, got[key], program)
		}
	}
	if programs := a.rt.cfg.HelperProgramList(); len(programs) != 3 {
		t.Fatalf("registrations = %+v, want the chosen registration reused", programs)
	}
	searchRow, ok := helperRowByPath(rows, helperDocumentKey(document, "search"))
	if !ok || !strings.HasSuffix(searchRow.Label, "(allowed: "+rightPath+")") {
		t.Fatalf("row = %+v", searchRow)
	}
	ocrRow, ok := helperRowByPath(rows, helperDocumentKey(document, "ocr"))
	if !ok || !strings.HasSuffix(ocrRow.Label, "(allowed: "+ocrPath+")") {
		t.Fatalf("the other name's row changed: %+v", ocrRow)
	}
}

// Clearing this open's refusal for the configured name must not clear the rest of
// them: the others were never answered.
func TestHelperDocumentConfigureClearsOnlyTheAffectedDenial(t *testing.T) {
	home, err := resolveSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := newTestApp(t, home)
	document := writeHelperPage(t, home, "search", "ocr")
	programPath := writeTestProgram(t, t.TempDir(), "search", 0755)
	if _, err := a.rt.cfg.AddHelperProgram("search", programPath); err != nil {
		t.Fatal(err)
	}
	a.openForTest(t, document)
	a.setOpenDenials(document, []string{"search", "ocr"})

	a.configureHelperDocumentWith(helperDocumentKey(document, "search"), allowDocumentDialogs(programPath, platform.ConfirmAllowOnce, nil))

	if got := openDenialsOf(t, a, document); strings.Join(got, ",") != "ocr" {
		t.Fatalf("refusals after the allow = %v, want only the untouched name", got)
	}
	if got := a.helperBindingStatus(document, "search"); got != "allowed: "+programPath {
		t.Fatalf("status = %q", got)
	}
	if got := a.helperBindingStatus(document, "ocr"); got != "denied for this open" {
		t.Fatalf("status = %q", got)
	}
}

// A program file that is gone, or that this machine cannot execute, says so in
// the status instead of reading as a decision.
func TestHelperDocumentStatusReportsMissingAndNonExecutablePrograms(t *testing.T) {
	home, err := resolveSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := newTestApp(t, home)
	document := writeHelperPage(t, home, "search", "ocr")
	missing := filepath.Join(t.TempDir(), "gone")
	notExecutable := filepath.Join(t.TempDir(), "not-executable")
	if err := os.WriteFile(notExecutable, []byte("#!/bin/sh\n"), 0644); err != nil {
		t.Fatal(err)
	}
	searchProgram, err := a.rt.cfg.AddHelperProgram("search", missing)
	if err != nil {
		t.Fatal(err)
	}
	ocrProgram, err := a.rt.cfg.AddHelperProgram("ocr", notExecutable)
	if err != nil {
		t.Fatal(err)
	}
	a.openForTest(t, document)

	if got := a.helperBindingStatus(document, "search"); got != "program file missing: "+missing {
		t.Fatalf("status = %q", got)
	}
	if runtime.GOOS == "windows" {
		if got := a.helperBindingStatus(document, "ocr"); got != "approval needed: "+notExecutable {
			t.Fatalf("status = %q", got)
		}
	} else if got := a.helperBindingStatus(document, "ocr"); got != "program is not executable: "+notExecutable {
		t.Fatalf("status = %q", got)
	}

	rows := a.helperDocumentRows()
	row, ok := helperRowByPath(rows, helperDocumentKey(document, "search"))
	if !ok || !strings.HasSuffix(row.Label, "(program file missing: "+missing+")") {
		t.Fatalf("row = %+v", row)
	}
	if resolution, _ := a.rt.cfg.ResolveHelper(document, "search"); resolution.Allowed {
		t.Fatalf("a program file that is gone still resolved as allowed: %+v", resolution)
	}

	labels := map[string]string{}
	for _, row := range a.helperProgramRows() {
		labels[row.Path] = row.Label
	}
	if labels[searchProgram.ID] != "search  (missing)" {
		t.Errorf("program row = %q, want the missing status", labels[searchProgram.ID])
	}
	wantOcr := "ocr  (not executable)"
	if runtime.GOOS == "windows" {
		wantOcr = "ocr  (0 documents)"
	}
	if labels[ocrProgram.ID] != wantOcr {
		t.Errorf("program row = %q, want %q", labels[ocrProgram.ID], wantOcr)
	}
}

// The document can change while the native dialogs are open. Whatever the answer
// was, it must not be stored against a document that no longer declares the name,
// is no longer open, or is being shut down.
func TestHelperDocumentConfigureRefusesADocumentThatChangedDuringTheDialog(t *testing.T) {
	for _, tc := range []struct {
		name   string
		during func(t *testing.T, a *app, document, folder string)
	}{
		{"declaration removed", func(t *testing.T, a *app, document, _ string) {
			writeTestFile(t, document, `<!doctype html><head></head>`)
		}},
		{"untrusted", func(t *testing.T, a *app, _, folder string) {
			if err := a.untrustFolder(folder); err != nil {
				t.Fatal(err)
			}
		}},
		{"shutting down", func(t *testing.T, a *app, _, _ string) {
			a.mu.Lock()
			a.stopping = true
			a.mu.Unlock()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, err := resolveSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			folder := filepath.Join(home, "proj")
			document := filepath.Join(folder, "page.htmlclay")
			writeTestFile(t, document, `<!doctype html><head><meta name="htmlclay-helper" content="search"></head>`)
			programPath := writeTestProgram(t, t.TempDir(), "search", 0755)
			a := newTestApp(t, home)
			var reported atomic.Int32
			a.rt.notify = func(string, string) error {
				reported.Add(1)
				return nil
			}
			if _, err := a.rt.cfg.AddHelperProgram("search", programPath); err != nil {
				t.Fatal(err)
			}
			if err := a.trustFolder(folder); err != nil {
				t.Fatal(err)
			}
			s := liveSite(t, a, folder)
			if status, body := fetchNav(t, fileURL(s.port, filepath.Join("proj", "page.htmlclay"))); status != 200 {
				t.Fatalf("bookmark navigation = %d: %s", status, body)
			}
			if _, ok := s.sessions.LookupByPath(document); !ok {
				t.Fatal("the navigation did not register the document")
			}

			dialogs := allowDocumentDialogs(programPath, platform.ConfirmAllowOnce, nil)
			prepare := dialogs.prepare
			dialogs.prepare = func(path string) (bool, error) {
				ok, err := prepare(path)
				if err != nil || !ok {
					return ok, err
				}
				tc.during(t, a, document, folder)
				return true, nil
			}
			rows := a.configureHelperDocumentWith(helperDocumentKey(document, "search"), dialogs)

			if got := a.rt.cfg.HelperDecisionList(); len(got) != 0 {
				t.Fatalf("a decision was stored for a document that changed: %+v", got)
			}
			if resolution, _ := a.rt.cfg.ResolveHelper(document, "search"); resolution.Allowed {
				t.Fatalf("the name was granted anyway: %+v", resolution)
			}
			for _, row := range rows {
				if strings.HasPrefix(row.Path, document+"\x00") {
					t.Fatalf("a row survived for a document that is gone: %+v", row)
				}
			}
			testutil.Eventually(t, 5*time.Second, "the refusal to reach the user", func() bool { return reported.Load() > 0 })
		})
	}
}

// A row that is malformed, names a document that is not open, or names something
// the document does not declare asks nothing and changes nothing.
func TestHelperDocumentStaleOrMalformedRowAsksNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  func(home, document string) string
		stop bool
	}{
		{"no separator", func(_, document string) string { return document }, false},
		{"empty name", func(_, document string) string { return document + "\x00" }, false},
		{"empty document", func(_, _ string) string { return "\x00search" }, false},
		{"missing document", func(home, _ string) string { return filepath.Join(home, "missing.htmlclay") + "\x00search" }, false},
		{"undeclared name", func(_, document string) string { return document + "\x00ocr" }, false},
		{"not open", func(home, _ string) string { return filepath.Join(home, "other.htmlclay") + "\x00search" }, false},
		{"shutting down", func(_, document string) string { return document + "\x00search" }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, err := resolveSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			a := newTestApp(t, home)
			document := writeHelperPage(t, home, "search")
			other := filepath.Join(home, "other.htmlclay")
			writeTestFile(t, other, `<!doctype html><head><meta name="htmlclay-helper" content="search"></head>`)
			if _, err := a.rt.cfg.AddHelperProgram("search", writeTestProgram(t, t.TempDir(), "search", 0755)); err != nil {
				t.Fatal(err)
			}
			a.openForTest(t, document)
			if tc.stop {
				a.mu.Lock()
				a.stopping = true
				a.mu.Unlock()
			}

			dialogs := helperApprovalDialogs{
				choose: func(string) (string, bool, error) { t.Error("a stale row opened the picker"); return "", false, nil },
				confirm: func(string, string, bool) (platform.ConfirmChoice, error) {
					t.Error("a stale row asked for approval")
					return platform.ConfirmDeny, nil
				},
				prepare: func(string) (bool, error) { t.Error("a stale row prepared a program"); return false, nil },
			}
			a.configureHelperDocumentWith(tc.key(home, document), dialogs)

			if got := a.rt.cfg.HelperDecisionList(); len(got) != 0 {
				t.Fatalf("a stale row stored a decision: %+v", got)
			}
		})
	}
}

var errHelperFixture = errors.New("the dialog tool is unavailable")

// A native repair reuses an existing registration only when its stored path is
// the selected path. Selecting a stable symlink whose target an older
// registration already names gets the symlink its own registration, so package
// manager updates reach the binding; the older registration and its approvals
// stay untouched, and an Allow for This Document stays document-only.
func TestHelperDocumentNativeRepairKeepsTheSelectedSymlink(t *testing.T) {
	home, err := resolveSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := newTestApp(t, home)
	document := writeHelperPage(t, home, "search")
	bin := t.TempDir()
	versioned := writeTestProgram(t, bin, "search-v1", 0755)
	selected := filepath.Join(bin, "search-current")
	if err := os.Symlink(versioned, selected); err != nil {
		t.Fatal(err)
	}
	old, err := a.rt.cfg.AddHelperProgram("old-tool", versioned)
	if err != nil {
		t.Fatal(err)
	}
	a.openForTest(t, document)

	var seen []string
	a.configureHelperDocumentWith(helperDocumentKey(document, "search"), allowDocumentDialogs(selected, platform.ConfirmAllowOnce, &seen))
	if !strings.Contains(strings.Join(seen, "\n"), "Selected program:\n"+selected) {
		t.Fatal("the prompt did not show the selected path")
	}
	resolution, _ := a.rt.cfg.ResolveHelper(document, "search")
	if !resolution.Allowed {
		t.Fatal("the document did not approve the helper")
	}
	if resolution.Program.Path != selected {
		t.Errorf("native approval persisted a different executable path: got %q, want %q", resolution.Program.Path, selected)
	}
	if resolution.Program.ID == old.ID {
		t.Errorf("the selected symlink reused the registration for %q", versioned)
	}
	if resolution.Program.Name != "search" {
		t.Errorf("the new registration is named %q, want search", resolution.Program.Name)
	}
	kept, ok := a.rt.cfg.LookupHelperProgram(old.ID)
	if !ok || kept.Path != versioned || kept.Name != "old-tool" {
		t.Errorf("the older registration changed: %+v, %v", kept, ok)
	}
	if programs := a.rt.cfg.HelperProgramList(); len(programs) != 2 {
		t.Errorf("registrations = %+v, want the old one plus the selected symlink", programs)
	}
	decisions := a.rt.cfg.HelperDecisionList()
	if len(decisions) != 1 || decisions[0].Document != document || decisions[0].Name != "search" || decisions[0].Program != resolution.Program.ID || !decisions[0].Allowed {
		t.Fatalf("decisions = %+v", decisions)
	}
	for _, program := range a.rt.cfg.HelperProgramList() {
		if program.AnyDocument {
			t.Errorf("an Allow for This Document decision widened %s to any document", program.Path)
		}
	}
}

// A picked executable that is already registered under another name still binds
// the declaring document. Allow for This Document reuses that registration under
// its own name; Allow for Any Document leaves it alone and registers the selected
// path under the declared name, so the wider grant belongs to the name the dialog
// showed and no unshown registered name is approved.
func TestHelperDocumentCrossNameWiderApprovalUsesTheDeclaredName(t *testing.T) {
	for _, tc := range []struct {
		name   string
		choice platform.ConfirmChoice
		wider  bool
	}{
		{"allow once", platform.ConfirmAllowOnce, false},
		{"allow always", platform.ConfirmAllowAlways, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, err := resolveSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			a := newTestApp(t, home)
			document := writeHelperPage(t, home, "search")
			otherSearch := filepath.Join(home, "other-search.htmlclay")
			writeTestFile(t, otherSearch, `<!doctype html><head><meta name="htmlclay-helper" content="search"></head>`)
			otherXyd := filepath.Join(home, "other-xyd.htmlclay")
			writeTestFile(t, otherXyd, `<!doctype html><head><meta name="htmlclay-helper" content="xyd"></head>`)
			programPath := writeTestProgram(t, t.TempDir(), "clay-search", 0755)
			xyd, err := a.rt.cfg.AddHelperProgram("xyd", programPath)
			if err != nil {
				t.Fatal(err)
			}
			a.openForTest(t, document)

			var seen []string
			a.configureHelperDocumentWith(helperDocumentKey(document, "search"), allowDocumentDialogs(programPath, tc.choice, &seen))

			resolution, _ := a.rt.cfg.ResolveHelper(document, "search")
			if !resolution.Decided || !resolution.Allowed || resolution.Program.Path != programPath {
				t.Fatalf("the current document = %+v, want the selected path approved", resolution)
			}
			message := ""
			for _, call := range seen {
				if strings.HasPrefix(call, "confirm:Allow document program?:") {
					message = strings.TrimPrefix(call, "confirm:Allow document program?:")
				}
			}
			for _, want := range []string{document, "Selected program:\n" + programPath, "other documents that request search"} {
				if !strings.Contains(message, want) {
					t.Errorf("the prompt does not state %q:\n%s", want, message)
				}
			}
			if strings.Contains(message, "its registered name") {
				t.Errorf("the prompt describes a grant under an unshown registered name:\n%s", message)
			}
			if got, _ := a.rt.cfg.ResolveHelper(otherXyd, "xyd"); got.Allowed {
				t.Errorf("another document declaring xyd was allowed: %+v", got)
			}

			programs := a.rt.cfg.HelperProgramList()
			if !tc.wider {
				if len(programs) != 1 || resolution.Program.ID != xyd.ID {
					t.Fatalf("the document-only approval = %+v with %+v, want the registration at the selected path reused", resolution, programs)
				}
				kept, ok := a.rt.cfg.LookupHelperProgram(xyd.ID)
				if !ok || kept.AnyDocument {
					t.Fatalf("the reused registration = %+v, %v, want xyd left document-only", kept, ok)
				}
				if got, _ := a.rt.cfg.ResolveHelper(otherSearch, "search"); got.Allowed {
					t.Errorf("another document declaring search was allowed by the document-only choice: %+v", got)
				}
				return
			}
			kept, ok := a.rt.cfg.LookupHelperProgram(xyd.ID)
			if !ok || kept.Name != "xyd" || kept.Path != programPath || kept.AnyDocument {
				t.Fatalf("the older registration changed: %+v, %v", kept, ok)
			}
			var declared []config.HelperProgram
			for _, program := range programs {
				if program.Name == "search" {
					declared = append(declared, program)
				}
			}
			if len(declared) != 1 || declared[0].Path != programPath || !declared[0].AnyDocument || declared[0].ID == xyd.ID {
				t.Fatalf("the declared-name registration = %+v, want one new search registration at %s", declared, programPath)
			}
			if resolution.Program.ID != declared[0].ID {
				t.Fatalf("the current document binds %+v, want the new registration %+v", resolution.Program, declared[0])
			}
			other, _ := a.rt.cfg.ResolveHelper(otherSearch, "search")
			if !other.Allowed || other.Program.ID != declared[0].ID {
				t.Fatalf("another document declaring search = %+v, want the wider grant on %+v", other, declared[0])
			}
		})
	}
}
