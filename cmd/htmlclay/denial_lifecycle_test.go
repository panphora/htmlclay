package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/panphora/htmlclay/internal/config"
	"github.com/panphora/htmlclay/internal/platform"
)

func helperDiscoveryState(t *testing.T, s *site, document string) string {
	t.Helper()
	f, ok := s.sessions.LookupByPath(document)
	if !ok {
		t.Fatal("missing document registration")
	}
	status, body := fetch(t, fmt.Sprintf("http://127.0.0.1:%d/_/meta/%s", s.port, f.Token))
	var meta struct {
		Document struct {
			Helpers []struct{ Name, State string }
		}
	}
	if status != 200 {
		t.Fatalf("meta status %d: %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &meta); err != nil {
		t.Fatal(err)
	}
	// The declared helper, then the built-in ai-edit, which is on by default.
	if len(meta.Document.Helpers) != 2 || meta.Document.Helpers[1].Name != "ai-edit" || meta.Document.Helpers[1].State != "ready" {
		t.Fatalf("helpers: %+v", meta.Document.Helpers)
	}
	return meta.Document.Helpers[0].State
}

func TestADenialLastsForTheRestOfThatOpen(t *testing.T) {
	for _, scenario := range []string{"cancel", "revoke one shared global", "navigation reattach", "ordinary reload"} {
		t.Run(scenario, func(t *testing.T) {
			home, err := resolveSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			a := newTestApp(t, home)
			document := filepath.Join(home, "docs", "page.htmlclay")
			writeTestFile(t, document, `<html><head><meta name="htmlclay-helper" content="search"></head></html>`)
			first, err := a.rt.cfg.AddHelperProgram("search", writeTestProgram(t, home, "first", 0755))
			if err != nil {
				t.Fatal(err)
			}
			var second config.HelperProgram
			if scenario == "revoke one shared global" {
				second, err = a.rt.cfg.AddHelperProgram("search", writeTestProgram(t, home, "second", 0755))
				if err != nil {
					t.Fatal(err)
				}
				a.rt.cfg.SetHelperAnyDocument(first.ID, true)
				a.rt.cfg.SetHelperAnyDocument(second.ID, true)
			}
			calls := 0
			plan := a.helpersForOpenWith(document, true, false, helperApprovalDialogs{
				confirm: func(string, string, bool) (platform.ConfirmChoice, error) {
					calls++
					return platform.ConfirmDeny, nil
				},
			})
			if calls != 1 || len(plan.denied) != 1 || len(plan.allowed) != 0 {
				t.Fatalf("initial plan %+v, calls %d", plan, calls)
			}
			s, rel := a.openForTest(t, document)
			a.applyHelperPlan(s, document, plan)
			if state := helperDiscoveryState(t, s, document); state != "denied" {
				t.Fatalf("initial state %q", state)
			}
			switch scenario {
			case "cancel":
				a.manageHelperProgramWith(first.ID, func(platform.ProgramSummary) (platform.ManageChoice, error) {
					return platform.ManageCancel, nil
				})
				a.refreshHelperDispatchers()
			case "revoke one shared global":
				a.manageHelperProgramWith(second.ID, func(platform.ProgramSummary) (platform.ManageChoice, error) {
					return platform.ManageToggleAnyDocument, nil
				})
				a.refreshHelperDispatchers()
			case "navigation reattach":
				if _, ok := a.serveURL(document); !ok {
					t.Fatal("serveURL failed")
				}
			case "ordinary reload":
				if status, _ := fetchNav(t, fileURL(s.port, rel)); status != 200 {
					t.Fatalf("reload status %d", status)
				}
			}
			state := helperDiscoveryState(t, s, document)
			if state != "denied" {
				t.Errorf("this open's refusal was lost: want denied, got %s", state)
			}
			reopened := 0
			plan = a.helpersForOpenWith(document, true, false, helperApprovalDialogs{
				confirm: func(string, string, bool) (platform.ConfirmChoice, error) {
					reopened++
					return platform.ConfirmDismissed, nil
				},
			})
			if len(plan.denied) != 0 {
				t.Errorf("the next direct open inherited this open's refusal: %+v", plan.denied)
			}
			if _, covered := plan.allowed["search"]; covered {
				if reopened != 0 {
					t.Errorf("an allow already covers this name, yet the open asked again: %d prompts", reopened)
				}
			} else if reopened != 1 {
				t.Errorf("the next direct open did not ask again: %d prompts", reopened)
			}
		})
	}
}
