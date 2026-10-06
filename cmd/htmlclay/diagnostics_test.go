package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/panphora/htmlclay/internal/config"
	"github.com/panphora/htmlclay/internal/logging"
	"github.com/panphora/htmlclay/internal/platform"
)

// diagnosticLog points the app's logger at a file in a temporary directory. The
// logger serializes its own writes, so a test that serves requests on other
// goroutines can read the whole file without racing them; every read sees only
// completed lines.
func diagnosticLog(t *testing.T) (*logging.Logger, func() string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "htmlclay.log")
	logger, err := logging.New(path)
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	t.Cleanup(logger.Close)
	return logger, func() string {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read log: %v", err)
		}
		return string(data)
	}
}

func diagnosticProgramAtPath(t *testing.T, a *app, path string) config.HelperProgram {
	t.Helper()
	for _, program := range a.rt.cfg.HelperProgramList() {
		if program.Path == path {
			return program
		}
	}
	t.Fatalf("no registered program at %s", path)
	return config.HelperProgram{}
}

// blockDiagnosticConfigDir puts a file where the config directory should be, so
// every Save fails the way a full disk or an unwritable home would.
func blockDiagnosticConfigDir(t *testing.T, cfgBase string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(cfgBase, "htmlclay"), []byte("blocks the config directory"), 0600); err != nil {
		t.Fatal(err)
	}
}

func writeDiagnosticConfig(t *testing.T, cfgBase, body string) {
	t.Helper()
	dir := config.DirFrom(cfgBase)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestUsabilityDiagnosticsHelperProgramAddAndRemoveAreLogged(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	logger, read := diagnosticLog(t)
	a.rt.logger = logger
	programPath := writeTestProgram(t, t.TempDir(), "search", 0755)

	a.pickHelperProgramWith(helperProgramDialogs{
		promptName: func(string, string, string) (string, bool, error) { return "search", true, nil },
		selectFile: func(string) (string, bool, error) { return programPath, true, nil },
		confirmExecutable: func(string, string, string) (bool, error) {
			t.Error("an executable program asked to be made executable")
			return false, nil
		},
	})

	program := diagnosticProgramAtPath(t, a, programPath)
	registered := fmt.Sprintf("Helper program registered: id=%s name=%s path=%s", program.ID, program.Name, program.Path)
	if log := read(); !strings.Contains(log, registered) {
		t.Fatalf("a successful registration is not in the log:\n%s", log)
	}

	a.manageHelperProgramWith(program.ID, func(platform.ProgramSummary) (platform.ManageChoice, error) {
		return platform.ManageRemove, nil
	})
	removed := fmt.Sprintf("Helper program removed: id=%s name=%s path=%s", program.ID, program.Name, program.Path)
	if log := read(); !strings.Contains(log, removed) {
		t.Fatalf("a successful removal is not in the log:\n%s", log)
	}
}

func TestUsabilityDiagnosticsHelperBindingLogsTheDocumentAndPreviousProgram(t *testing.T) {
	home, err := resolveSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := newTestApp(t, home)
	logger, read := diagnosticLog(t)
	a.rt.logger = logger
	document := writeHelperPage(t, home, "search")
	a.openForTest(t, document)

	firstPath := writeTestProgram(t, t.TempDir(), "first", 0755)
	a.configureHelperDocumentWith(helperDocumentKey(document, "search"), allowDocumentDialogs(firstPath, platform.ConfirmAllowOnce, nil))
	first := diagnosticProgramAtPath(t, a, firstPath)
	log := read()
	requested := fmt.Sprintf("Native helper configuration requested: document=%s name=search status=", document)
	if !strings.Contains(log, requested) {
		t.Fatalf("a user-driven configuration is not in the log:\n%s", log)
	}
	bound := fmt.Sprintf("Helper binding saved: document=%s name=search program=%s path=%s scope=document previous=", document, first.ID, first.Path)
	if !strings.Contains(log, bound) {
		t.Fatalf("the saved binding is not in the log:\n%s", log)
	}

	secondPath := writeTestProgram(t, t.TempDir(), "second", 0755)
	a.configureHelperDocumentWith(helperDocumentKey(document, "search"), allowDocumentDialogs(secondPath, platform.ConfirmAllowAlways, nil))
	second := diagnosticProgramAtPath(t, a, secondPath)
	rebound := fmt.Sprintf("Helper binding saved: document=%s name=search program=%s path=%s scope=any document previous=%s", document, second.ID, second.Path, first.ID)
	if log := read(); !strings.Contains(log, rebound) {
		t.Fatalf("the rebinding did not record the program it replaced:\n%s", log)
	}
}

func TestUsabilityDiagnosticsFailedHelperSavesLogNoSuccess(t *testing.T) {
	t.Run("register", func(t *testing.T) {
		cfgBase := t.TempDir()
		a := newTestAppWithConfigDir(t, t.TempDir(), cfgBase)
		logger, read := diagnosticLog(t)
		a.rt.logger = logger
		programPath := writeTestProgram(t, t.TempDir(), "search", 0755)
		blockDiagnosticConfigDir(t, cfgBase)

		a.pickHelperProgramWith(helperProgramDialogs{
			promptName: func(string, string, string) (string, bool, error) { return "search", true, nil },
			selectFile: func(string) (string, bool, error) { return programPath, true, nil },
			confirmExecutable: func(string, string, string) (bool, error) {
				t.Error("an executable program asked to be made executable")
				return false, nil
			},
		})
		if log := read(); strings.Contains(log, "Helper program registered") {
			t.Fatalf("a failed registration logged a success:\n%s", log)
		}
	})

	t.Run("manage", func(t *testing.T) {
		cfgBase := t.TempDir()
		a := newTestAppWithConfigDir(t, t.TempDir(), cfgBase)
		logger, read := diagnosticLog(t)
		a.rt.logger = logger
		program, err := a.rt.cfg.AddHelperProgram("search", writeTestProgram(t, t.TempDir(), "search", 0755))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := a.rt.cfg.DecideHelper(config.HelperDecision{
			Document:  "/docs/search.htmlclay",
			Name:      program.Name,
			Program:   program.ID,
			Allowed:   true,
			DecidedAt: 42,
		}); err != nil {
			t.Fatal(err)
		}
		blockDiagnosticConfigDir(t, cfgBase)

		for _, choice := range []platform.ManageChoice{
			platform.ManageToggleAnyDocument,
			platform.ManageForgetDecisions,
			platform.ManageRemove,
		} {
			a.manageHelperProgramWith(program.ID, func(platform.ProgramSummary) (platform.ManageChoice, error) {
				return choice, nil
			})
		}
		log := read()
		for _, success := range []string{
			"Helper program permission changed",
			"Helper program approvals forgotten",
			"Helper program removed",
		} {
			if strings.Contains(log, success) {
				t.Fatalf("a failed save logged %q:\n%s", success, log)
			}
		}
	})

	t.Run("binding", func(t *testing.T) {
		home, err := resolveSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		cfgBase := t.TempDir()
		a := newTestAppWithConfigDir(t, home, cfgBase)
		logger, read := diagnosticLog(t)
		a.rt.logger = logger
		document := writeHelperPage(t, home, "search")
		a.openForTest(t, document)
		blockDiagnosticConfigDir(t, cfgBase)

		programPath := writeTestProgram(t, t.TempDir(), "search", 0755)
		a.configureHelperDocumentWith(helperDocumentKey(document, "search"), allowDocumentDialogs(programPath, platform.ConfirmAllowOnce, nil))
		if log := read(); strings.Contains(log, "Helper binding saved") {
			t.Fatalf("a failed save logged a saved binding:\n%s", log)
		}
	})
}

func TestUsabilityDiagnosticsNormalizationCountsAreLogged(t *testing.T) {
	t.Run("records dropped", func(t *testing.T) {
		home, err := resolveSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		cfgBase := t.TempDir()
		writeDiagnosticConfig(t, cfgBase, `{
  "helperPrograms": [
    {"id": "kept", "name": "search", "path": "/helpers/search", "addedAt": 2},
    {"id": "kept", "name": "other", "path": "/helpers/duplicate", "addedAt": 3},
    {"id": "invalid", "name": "Not Valid", "path": "/helpers/invalid", "addedAt": 1}
  ],
  "helperDecisions": [
    {"document": "/docs/a", "name": "search", "program": "kept", "allowed": true, "decidedAt": 1},
    {"document": "/docs/a", "name": "search", "program": "kept", "allowed": true, "decidedAt": 5},
    {"document": "/docs/b", "name": "Not Valid", "decidedAt": 2},
    {"document": "/docs/c", "name": "other", "program": "missing", "allowed": true, "decidedAt": 3},
    {"document": "/docs/d", "name": "format", "program": "kept", "decidedAt": 4}
  ]
}`)
		a := newTestAppWithConfigDir(t, home, cfgBase)
		logger, read := diagnosticLog(t)
		a.rt.logger = logger

		_, res, err := config.LoadFrom(cfgBase, platform.DirIdentity)
		if err != nil {
			t.Fatal(err)
		}
		if res.DroppedHelperPrograms == 0 || res.DroppedHelperDecisions == 0 {
			t.Fatalf("the fixture did not drop any helper records: %+v", res)
		}
		a.loaded = res
		a.finishUpgrade()

		want := fmt.Sprintf("Helper config normalization dropped records: programs=%d decisions=%d", res.DroppedHelperPrograms, res.DroppedHelperDecisions)
		if log := read(); !strings.Contains(log, want) {
			t.Fatalf("normalization dropped records without saying so:\n%s", log)
		}
	})

	t.Run("nothing dropped", func(t *testing.T) {
		a := newTestApp(t, t.TempDir())
		logger, read := diagnosticLog(t)
		a.rt.logger = logger
		a.finishUpgrade()
		if log := read(); strings.Contains(log, "Helper config normalization dropped records") {
			t.Fatalf("a settled launch reported dropped records:\n%s", log)
		}
	})
}

func TestUsabilityDiagnosticsOccupiedPortLogsTheFallbackAndTheChange(t *testing.T) {
	home, _ := resolveSymlinks(t.TempDir())
	proj := filepath.Join(home, "proj")
	page := filepath.Join(proj, "index.htmlclay")
	writeTestFile(t, page, "<html><body>index</body></html>")

	cfgBase := t.TempDir()
	first := newTestAppWithConfigDir(t, home, cfgBase)
	if err := first.trustFolder(proj); err != nil {
		t.Fatal(err)
	}
	s, _ := first.openForTest(t, page)
	held := s.port
	first.shutdown()
	requirePortFree(t, held)

	blocker, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", held))
	if err != nil {
		t.Fatalf("occupy the remembered port %d: %v", held, err)
	}
	defer blocker.Close()

	second := newTestAppWithConfigDir(t, home, cfgBase)
	logger, read := diagnosticLog(t)
	second.rt.logger = logger
	second.startSites()

	moved := liveSite(t, second, proj).port
	if moved == 0 || moved == held {
		t.Fatalf("the origin did not move off the occupied port %d: got %d", held, moved)
	}
	log := read()
	taken := fmt.Sprintf("Remembered port %d for %s is taken; using another port, so old bookmarks cannot be held", held, proj)
	if !strings.Contains(log, taken) {
		t.Fatalf("an occupied remembered port is not in the log:\n%s", log)
	}
	changed := fmt.Sprintf("Origin port changed: anchor=%s previous=%d current=%d", proj, held, moved)
	if !strings.Contains(log, changed) {
		t.Fatalf("the port change did not record both ports:\n%s", log)
	}
}

func TestUsabilityDiagnosticsShadowedOriginLogsBothAnchorsWithoutRequests(t *testing.T) {
	home, _ := resolveSymlinks(t.TempDir())
	proj := filepath.Join(home, "proj")
	sub := filepath.Join(proj, "sub")
	page := filepath.Join(sub, "deep.htmlclay")
	writeTestFile(t, page, "<html><body>deep</body></html>")

	cfgBase := t.TempDir()
	first := newTestAppWithConfigDir(t, home, cfgBase)
	if err := first.trustFolder(sub); err != nil {
		t.Fatal(err)
	}
	s, rel := first.openForTest(t, page)
	subPort := s.port
	if err := first.trustFolder(proj); err != nil {
		t.Fatal(err)
	}
	first.shutdown()

	second := newTestAppWithConfigDir(t, home, cfgBase)
	logger, read := diagnosticLog(t)
	second.rt.logger = logger
	second.startSites()

	projPort := liveSite(t, second, proj).port
	if subPort == projPort {
		t.Fatalf("the fixture needs two distinct origins, both are %d", subPort)
	}
	log := read()
	shadowed := fmt.Sprintf("Trusted folder %s is covered by %s; its remembered origin uses navigation recovery", sub, proj)
	if !strings.Contains(log, shadowed) {
		t.Fatalf("a shadowed trusted folder is not in the log:\n%s", log)
	}
	holding := fmt.Sprintf("Holding remembered origin: anchor=%s port=%d; current trust covers it at anchor=%s port=%d for eligible navigations", sub, subPort, proj, projPort)
	if !strings.Contains(log, holding) {
		t.Fatalf("the held shadowed origin did not name both anchors and ports:\n%s", log)
	}

	before := read()
	code, _, _ := do(t, browserClient(), navRequest(t, fileURL(subPort, rel)))
	if code != http.StatusFound {
		t.Fatalf("a navigation on the held port = %d, want 302 to the current origin", code)
	}
	if after := read(); after != before {
		t.Fatalf("a request to the held port wrote to the log:\n%s", strings.TrimPrefix(after, before))
	}
}
