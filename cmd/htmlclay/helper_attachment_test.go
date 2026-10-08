package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/panphora/htmlclay/internal/config"
	"github.com/panphora/htmlclay/internal/platform"
)

type attachmentTerminal struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Payload struct {
		Program string `json:"program"`
		Code    string `json:"code"`
	} `json:"payload"`
}

// The test binary doubles as the helper program. The dispatcher starts a
// registered program with no arguments and passes this process's environment
// through, so TestMain picks the fake by environment variable, and the copy's
// file name tells it which program it is.
const attachmentHelperMode = "HTMLCLAY_ATTACHMENT_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(attachmentHelperMode) != "" {
		os.Exit(runAttachmentHelper())
	}
	os.Exit(m.Run())
}

func runAttachmentHelper() int {
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		return 1
	}
	self := os.Args[0]
	marker, err := os.OpenFile(self+".ran", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return 1
	}
	if _, err := marker.WriteString("ran\n"); err != nil {
		marker.Close()
		return 1
	}
	if err := marker.Close(); err != nil {
		return 1
	}
	label := strings.TrimSuffix(filepath.Base(self), ".exe")
	result, err := json.Marshal(map[string]any{"type": "result", "value": map[string]string{"program": label}})
	if err != nil {
		return 1
	}
	fmt.Println(string(result))
	return 0
}

func attachmentProgram(t *testing.T, label string) (path, marker string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(t.TempDir(), label)
	if runtime.GOOS == "windows" {
		path += ".exe"
	}
	// Windows will not delete a hard link to a running image, so TempDir cleanup
	// would fail on the link to this test binary: copy there instead.
	if runtime.GOOS == "windows" || os.Link(self, path) != nil {
		source, err := os.Open(self)
		if err != nil {
			t.Fatal(err)
		}
		defer source.Close()
		target, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0755)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(target, source); err != nil {
			target.Close()
			t.Fatal(err)
		}
		if err := target.Close(); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(attachmentHelperMode, "1")
	return path, path + ".ran"
}

func attachmentRequestTerminal(t *testing.T, s *site, document, token, id string) attachmentTerminal {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	query := url.Values{"file": {document}, "token": {token}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/_/wire/subscribe?%s", s.port, query.Encode()), nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("subscribe status %d", response.StatusCode)
	}
	wireHelperRequest(t, s, token, id, document)
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var frame attachmentTerminal
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &frame); err != nil {
			t.Fatal(err)
		}
		if frame.ID == id && (frame.Type == "wire/done" || frame.Type == "wire/error") {
			return frame
		}
	}
	t.Fatalf("no terminal result for %s: %v", id, scanner.Err())
	return attachmentTerminal{}
}

func TestHelperAttachmentUsesCurrentPermissions(t *testing.T) {
	for _, kind := range []string{"replacement", "remove", "forget", "new denial", "cleared denial"} {
		t.Run(kind, func(t *testing.T) {
			home, err := resolveSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			a := newTestApp(t, home)
			document := writeHelperPage(t, home, "search")
			s, _ := a.openForTest(t, document)
			oldPath, oldMarker := attachmentProgram(t, "old")
			newPath, newMarker := attachmentProgram(t, "replacement")
			old, err := a.rt.cfg.AddHelperProgram("search", oldPath)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := a.rt.cfg.DecideHelper(config.HelperDecision{Document: document, Name: "search", Program: old.ID, Allowed: true}); err != nil {
				t.Fatal(err)
			}
			if kind == "cleared denial" {
				a.setOpenDenials(document, []string{"search"})
			}
			stale := a.helpersForNavigation(document)
			if kind == "cleared denial" {
				if len(stale.allowed) != 0 || len(stale.denied) != 1 {
					t.Fatal("fixture did not capture denied permissions")
				}
			} else if stale.allowed["search"].ID != old.ID {
				t.Fatal("fixture did not capture old allowed permissions")
			}
			switch kind {
			case "replacement", "cleared denial":
				a.configureHelperDocumentWith(helperDocumentKey(document, "search"), allowDocumentDialogs(newPath, platform.ConfirmAllowOnce, nil))
			case "remove", "forget", "new denial":
				choice := platform.ManageForgetDecisions
				if kind == "remove" {
					choice = platform.ManageRemove
				}
				a.manageHelperProgramWith(old.ID, func(platform.ProgramSummary) (platform.ManageChoice, error) { return choice, nil })
				a.refreshHelperDispatchers()
				if kind == "new denial" {
					denied := a.helpersForOpenWith(document, true, false, allowDocumentDialogs(oldPath, platform.ConfirmDeny, nil))
					a.applyHelperPlan(s, document, denied)
				}
			}
			a.applyHelperPlan(s, document, stale)
			registered, ok := s.sessions.LookupByPath(document)
			if !ok {
				t.Fatal("document registration disappeared")
			}
			terminal := attachmentRequestTerminal(t, s, document, registered.Token, "current-permission-request")
			if kind == "replacement" || kind == "cleared denial" {
				if terminal.Type != "wire/done" || terminal.Payload.Program != "replacement" {
					t.Fatalf("stale attachment executed wrong program: %+v", terminal)
				}
				current, _ := a.rt.cfg.ResolveHelper(document, "search")
				if !current.Allowed || current.Program.Path != newPath {
					t.Fatal("replacement config changed")
				}
				if _, err := os.Stat(newMarker); err != nil {
					t.Fatalf("replacement did not execute: %v", err)
				}
			} else {
				if terminal.Type != "wire/error" || terminal.Payload.Code != "helper_not_granted" {
					t.Fatalf("stale allow was restored: %+v", terminal)
				}
				state := helperDiscoveryState(t, s, document)
				want := "unavailable"
				if kind == "new denial" {
					want = "denied"
				}
				if state != want {
					t.Fatalf("helper state %q, want %q", state, want)
				}
			}
			if _, err := os.Stat(oldMarker); !os.IsNotExist(err) {
				t.Fatalf("old program executed or marker inspection failed: %v", err)
			}
		})
	}
}

func TestHelperRefreshDoesNotNotifyForAnUnrelatedDeletedDocument(t *testing.T) {
	home, err := resolveSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := newTestApp(t, home)
	document := writeHelperPage(t, home, "search")
	gone := filepath.Join(home, "old-notes.htmlclay")
	writeTestFile(t, gone, `<head><meta name="htmlclay-helper" content="ocr"></head>`)
	a.openForTest(t, document)
	a.openForTest(t, gone)
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	var notifications atomic.Int64
	a.rt.notify = func(string, string) error { notifications.Add(1); return nil }
	path := writeTestProgram(t, t.TempDir(), "search", 0755)
	a.configureHelperDocumentWith(helperDocumentKey(document, "search"), allowDocumentDialogs(path, platform.ConfirmAllowOnce, nil))
	current, _ := a.rt.cfg.ResolveHelper(document, "search")
	if !current.Allowed || current.Program.Path != path {
		t.Fatal("repair did not save the selected program")
	}
	if got := notifications.Load(); got != 0 {
		t.Fatalf("repair notified about an unrelated document %d times", got)
	}
}
