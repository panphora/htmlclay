package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
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

func attachmentProgram(t *testing.T, label, marker string) string {
	t.Helper()
	result, err := json.Marshal(map[string]any{"type": "result", "value": map[string]string{"program": label}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "program")
	script := "#!/bin/sh\ncat > /dev/null\necho ran >> " + strconv.Quote(marker) + "\nprintf '%s\\n' '" + string(result) + "'\n"
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return path
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
			oldMarker := filepath.Join(t.TempDir(), "old-ran")
			newMarker := filepath.Join(t.TempDir(), "replacement-ran")
			oldPath := attachmentProgram(t, "old", oldMarker)
			newPath := attachmentProgram(t, "replacement", newMarker)
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
