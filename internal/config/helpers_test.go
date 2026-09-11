package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestValidHelperName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"search", true},
		{"search-2", true},
		{"a", true},
		{"123", true},
		{"", false},
		{"-search", false},
		{"search-", false},
		{"Search", false},
		{"search_tool", false},
		{"search tool", false},
		{"café", false},
		{"abcdefghijklmnopqrstuvwxyz123456", true},
		{"abcdefghijklmnopqrstuvwxyz1234567", false},
	}
	for _, tc := range cases {
		if got := ValidHelperName(tc.name); got != tc.want {
			t.Errorf("ValidHelperName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestHelperRegistryRoundTripAndResolution(t *testing.T) {
	base := t.TempDir()
	cfg, _, err := LoadFrom(base, noIdentity)
	if err != nil {
		t.Fatal(err)
	}
	first, err := cfg.AddHelperProgram("search", "/helpers/first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := cfg.AddHelperProgram("search", "/helpers/second")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID || first.ID == "" || second.ID == "" {
		t.Fatalf("program IDs are not distinct opaque values: %q, %q", first.ID, second.ID)
	}

	resolution, ok := cfg.ResolveHelper("/docs/app.htmlclay", "search")
	if !ok || resolution.Decided || resolution.Allowed || resolution.Program.ID != first.ID {
		t.Fatalf("undecided resolution = %+v, %v", resolution, ok)
	}
	if previous, ok := cfg.SetHelperAnyDocument(second.ID, true); !ok || previous {
		t.Fatalf("SetHelperAnyDocument = %v, %v", previous, ok)
	}
	resolution, ok = cfg.ResolveHelper("/docs/app.htmlclay", "search")
	if !ok || !resolution.Decided || !resolution.Allowed || resolution.Program.ID != second.ID {
		t.Fatalf("any-document resolution = %+v, %v", resolution, ok)
	}

	denial := HelperDecision{Document: "/docs/app.htmlclay", Name: "search", DecidedAt: 10}
	if _, _, err := cfg.DecideHelper(denial); err != nil {
		t.Fatal(err)
	}
	resolution, ok = cfg.ResolveHelper(denial.Document, denial.Name)
	if !ok || !resolution.Decided || resolution.Allowed || resolution.Program.ID != "" {
		t.Fatalf("denied resolution = %+v, %v", resolution, ok)
	}

	allow := HelperDecision{Document: denial.Document, Name: denial.Name, Program: first.ID, Allowed: true, DecidedAt: 20}
	previous, had, err := cfg.DecideHelper(allow)
	if err != nil || !had || previous != denial {
		t.Fatalf("replace decision = %+v, %v, %v", previous, had, err)
	}
	resolution, ok = cfg.ResolveHelper(allow.Document, allow.Name)
	if !ok || !resolution.Decided || !resolution.Allowed || resolution.Program.ID != first.ID {
		t.Fatalf("allowed resolution = %+v, %v", resolution, ok)
	}

	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := LoadFrom(base, noIdentity)
	if err != nil {
		t.Fatal(err)
	}
	resolution, ok = loaded.ResolveHelper(allow.Document, allow.Name)
	if !ok || !resolution.Decided || !resolution.Allowed || resolution.Program.Path != first.Path {
		t.Fatalf("round-trip resolution = %+v, %v", resolution, ok)
	}
	data, err := os.ReadFile(filepath.Join(DirFrom(base), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["helperPrograms"] == nil || fields["helperDecisions"] == nil {
		t.Fatalf("helper fields use the wrong JSON keys: %s", data)
	}
}

func TestHelperMutationsCanRestoreExactSavedBytes(t *testing.T) {
	base := t.TempDir()
	cfg, _, err := LoadFrom(base, noIdentity)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := cfg.AddHelperProgram("search", "/helpers/first")
	second, _ := cfg.AddHelperProgram("format", "/helpers/second")
	decisions := []HelperDecision{
		{Document: "/docs/a.htmlclay", Name: "search", Program: first.ID, Allowed: true, DecidedAt: 30},
		{Document: "/docs/b.htmlclay", Name: "format", Program: second.ID, Allowed: true, DecidedAt: 10},
		{Document: "/docs/c.htmlclay", Name: "search", Program: first.ID, Allowed: true, DecidedAt: 20},
	}
	for _, d := range decisions {
		if _, _, err := cfg.DecideHelper(d); err != nil {
			t.Fatal(err)
		}
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(DirFrom(base), "config.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	removedProgram, ok := cfg.RemoveHelperProgram(first.ID)
	if !ok || removedProgram != first {
		t.Fatalf("remove returned %+v, %v", removedProgram, ok)
	}
	if got := cfg.HelperDecisionList(); len(got) != len(decisions) {
		t.Fatalf("removing a program dropped decision rows: %+v", got)
	}
	if !cfg.RestoreHelperProgram(removedProgram) {
		t.Fatal("program restore failed")
	}
	forgotten := cfg.ForgetHelperProgramDecisions(first.ID)
	if got := cfg.RestoreHelperDecisions(forgotten); got != len(forgotten) {
		t.Fatalf("restored %d forgotten decisions, want %d", got, len(forgotten))
	}
	one, ok := cfg.ForgetHelperDecision(decisions[1].Document, decisions[1].Name)
	if !ok || one != decisions[1] || cfg.RestoreHelperDecisions([]HelperDecision{one}) != 1 {
		t.Fatalf("single decision restore failed: %+v, %v", one, ok)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("restore changed saved bytes:\nbefore: %s\nafter:  %s", before, after)
	}
}

func TestHelperCapsRefuseInsertion(t *testing.T) {
	cfg := &Config{}
	for i := 0; i < helperProgramCap; i++ {
		if _, err := cfg.AddHelperProgram(fmt.Sprintf("helper%d", i), fmt.Sprintf("/helper/%d", i)); err != nil {
			t.Fatalf("program %d: %v", i, err)
		}
	}
	if _, err := cfg.AddHelperProgram("overflow", "/helper/overflow"); !errors.Is(err, ErrHelperRegistryFull) {
		t.Fatalf("program overflow error = %v", err)
	}
	program := cfg.HelperProgramList()[0]
	for i := 0; i < helperDecisionCap; i++ {
		d := HelperDecision{Document: fmt.Sprintf("/doc/%d", i), Name: "search", Program: program.ID, Allowed: true, DecidedAt: int64(i)}
		if _, _, err := cfg.DecideHelper(d); err != nil {
			t.Fatalf("decision %d: %v", i, err)
		}
	}
	if _, _, err := cfg.DecideHelper(HelperDecision{Document: "/overflow", Name: "search", Program: program.ID, Allowed: true}); !errors.Is(err, ErrHelperDecisionsFull) {
		t.Fatalf("decision overflow error = %v", err)
	}
	updated := HelperDecision{Document: "/doc/0", Name: "search", DecidedAt: 999}
	if previous, had, err := cfg.DecideHelper(updated); err != nil || !had || previous.Document != updated.Document {
		t.Fatalf("replacement at cap = %+v, %v, %v", previous, had, err)
	}
}

func TestHelperLoadNormalization(t *testing.T) {
	base := t.TempDir()
	programs := []HelperProgram{
		{ID: "kept", Name: "search", Path: "/helpers/search", AddedAt: 2},
		{ID: "kept", Name: "other", Path: "/helpers/duplicate", AddedAt: 3},
		{ID: "invalid", Name: "Not Valid", Path: "/helpers/invalid", AddedAt: 1},
	}
	decisions := []HelperDecision{
		{Document: "/docs/a", Name: "search", Program: "kept", Allowed: true, DecidedAt: 1},
		{Document: "/docs/a", Name: "search", Program: "kept", Allowed: true, DecidedAt: 5},
		{Document: "/docs/b", Name: "Not Valid", DecidedAt: 2},
		{Document: "/docs/c", Name: "other", Program: "missing", Allowed: true, DecidedAt: 3},
		{Document: "/docs/d", Name: "format", Program: "kept", DecidedAt: 4},
	}
	raw, err := json.Marshal(map[string]any{
		"startOnLogin":    true,
		"helperPrograms":  programs,
		"helperDecisions": decisions,
	})
	if err != nil {
		t.Fatal(err)
	}
	writeConfigJSON(t, base, string(raw))

	cfg, res, err := LoadFrom(base, noIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if res.DroppedHelperPrograms != 2 || res.DroppedHelperDecisions != 2 {
		t.Fatalf("normalization result = %+v", res)
	}
	if got := cfg.HelperProgramList(); len(got) != 1 || got[0] != programs[0] {
		t.Fatalf("programs after normalization = %+v", got)
	}
	// Ordered by DecidedAt. /docs/c names a program this config does not
	// register: it survives the load and reads as undecided, the same way it
	// survives RemoveHelperProgram, rather than being deleted behind the user.
	got := cfg.HelperDecisionList()
	if len(got) != 3 ||
		got[0].Document != "/docs/c" || got[0].Program != "missing" ||
		got[1].Document != "/docs/d" || got[1].Program != "" ||
		got[2].DecidedAt != 5 {
		t.Fatalf("decisions after normalization = %+v", got)
	}
	if resolution, _ := cfg.ResolveHelper("/docs/c", "other"); resolution.Decided {
		t.Fatalf("a kept decision naming an unregistered program must read as undecided: %+v", resolution)
	}
	if !cfg.StartOnLoginEnabled() {
		t.Fatal("normalization reset an unrelated setting")
	}
}

func TestHelperLoadCapsMissingThenOldestDecision(t *testing.T) {
	base := t.TempDir()
	present := t.TempDir()
	programs := make([]HelperProgram, helperProgramCap+1)
	for i := range programs {
		programs[i] = HelperProgram{ID: fmt.Sprintf("p%d", i), Name: fmt.Sprintf("p%d", i), Path: fmt.Sprintf("/p/%d", i), AddedAt: int64(i)}
	}
	decisions := make([]HelperDecision, 0, helperDecisionCap+2)
	for i := 0; i < helperDecisionCap+1; i++ {
		decisions = append(decisions, HelperDecision{Document: present, Name: fmt.Sprintf("h%d", i), DecidedAt: int64(i + 1)})
	}
	missing := HelperDecision{Document: filepath.Join(base, "missing.htmlclay"), Name: "missing", DecidedAt: 9999}
	decisions = append(decisions, missing)
	raw, err := json.Marshal(map[string]any{"helperPrograms": programs, "helperDecisions": decisions})
	if err != nil {
		t.Fatal(err)
	}
	writeConfigJSON(t, base, string(raw))

	cfg, res, err := LoadFrom(base, noIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.HelperProgramList()) != helperProgramCap || res.DroppedHelperPrograms != 1 {
		t.Fatalf("program cap result = %d, %+v", len(cfg.HelperProgramList()), res)
	}
	got := cfg.HelperDecisionList()
	if len(got) != helperDecisionCap || res.DroppedHelperDecisions != 2 {
		t.Fatalf("decision cap result = %d, %+v", len(got), res)
	}
	for _, d := range got {
		if d == missing || d.Name == "h0" {
			t.Fatalf("load retained an eviction-priority decision: %+v", d)
		}
	}
}

func TestHelperErrorsAndListCopies(t *testing.T) {
	cfg := &Config{}
	if _, err := cfg.AddHelperProgram("Bad Name", "/helper"); !errors.Is(err, ErrHelperNameInvalid) {
		t.Fatalf("invalid program name error = %v", err)
	}
	program, err := cfg.AddHelperProgram("search", "/helper")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := cfg.DecideHelper(HelperDecision{Name: "Bad Name"}); !errors.Is(err, ErrHelperNameInvalid) {
		t.Fatalf("invalid decision name error = %v", err)
	}
	if _, _, err := cfg.DecideHelper(HelperDecision{Document: "/doc", Name: "search", Program: "missing", Allowed: true}); err == nil {
		t.Fatal("allowed decision for an unknown program succeeded")
	}
	if _, _, err := cfg.DecideHelper(HelperDecision{Document: "/doc", Name: "search", Program: program.ID, Allowed: true}); err != nil {
		t.Fatal(err)
	}
	programList := cfg.HelperProgramList()
	decisionList := cfg.HelperDecisionList()
	programList[0].Name = "changed"
	decisionList[0].Name = "changed"
	if cfg.HelperProgramList()[0].Name != "search" || cfg.HelperDecisionList()[0].Name != "search" {
		t.Fatal("list method exposed mutable config storage")
	}
}

// The failure matrix's "decision exists, program unregistered" row. Unregistering
// a program must leave its decision rows alone: they read as undecided while it
// is gone, and re-registering the same ID brings them back rather than asking
// every affected document again.
func TestUnregisteringAProgramKeepsItsDecisions(t *testing.T) {
	cfg := &Config{}
	program, err := cfg.AddHelperProgram("search", "/helpers/search")
	if err != nil {
		t.Fatal(err)
	}
	decision := HelperDecision{Document: "/docs/a.htmlclay", Name: "search", Program: program.ID, Allowed: true, DecidedAt: 1}
	if _, _, err := cfg.DecideHelper(decision); err != nil {
		t.Fatal(err)
	}

	if _, ok := cfg.RemoveHelperProgram(program.ID); !ok {
		t.Fatal("remove reported the program was not registered")
	}
	if got := cfg.HelperDecisionList(); len(got) != 1 || got[0] != decision {
		t.Fatalf("removal dropped the decision row: %+v", got)
	}
	if resolution, _ := cfg.ResolveHelper(decision.Document, decision.Name); resolution.Decided {
		t.Fatalf("a decision naming an unregistered program must read as undecided: %+v", resolution)
	}

	if !cfg.RestoreHelperProgram(program) {
		t.Fatal("restoring the program failed")
	}
	resolution, ok := cfg.ResolveHelper(decision.Document, decision.Name)
	if !ok || !resolution.Decided || !resolution.Allowed || resolution.Program.ID != program.ID {
		t.Fatalf("re-registering the same ID must restore the decision: %+v, %v", resolution, ok)
	}
}

// Rollback must be able to put back exactly the row it displaced, including one
// whose program is no longer registered, because removal and load both keep that
// row. A failed approval used to delete the replacement and silently fail to
// restore the original.
func TestRestoringADecisionWhoseProgramIsGoneSucceeds(t *testing.T) {
	cfg := &Config{}
	stale := HelperDecision{Document: "/docs/a.htmlclay", Name: "search", Program: "gone", Allowed: true, DecidedAt: 1}
	if got := cfg.RestoreHelperDecisions([]HelperDecision{stale}); got != 1 {
		t.Fatalf("restored %d rows, want 1", got)
	}
	if got := cfg.HelperDecisionList(); len(got) != 1 || got[0] != stale {
		t.Fatalf("decisions after restore = %+v", got)
	}
	// Allowed with no program at all stays refused: that row is malformed, not
	// stale, and nothing can ever bind it.
	malformed := HelperDecision{Document: "/docs/b.htmlclay", Name: "search", Allowed: true, DecidedAt: 2}
	if got := cfg.RestoreHelperDecisions([]HelperDecision{malformed}); got != 0 {
		t.Fatalf("restored %d malformed rows, want 0", got)
	}
}
