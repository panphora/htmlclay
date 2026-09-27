package platform

import (
	"strings"
	"testing"
)

func TestManageChoiceFromResultMapsEveryAction(t *testing.T) {
	for _, p := range []ProgramSummary{{}, {AnyDocument: true}} {
		for _, tc := range []struct {
			result string
			want   ManageChoice
		}{
			{"", ManageCancel},
			{"cancel\n", ManageCancel},
			{"toggle\r\n", ManageToggleAnyDocument},
			{manageToggleLabel(p.AnyDocument) + "\n", ManageToggleAnyDocument},
			{"forget\n", ManageForgetDecisions},
			{forgetDecisionsLabel + "\n", ManageForgetDecisions},
			{"remove\n", ManageRemove},
			{removeProgramLabel + "\n", ManageRemove},
		} {
			got, ok := manageChoiceFromResult(tc.result, p)
			if !ok || got != tc.want {
				t.Errorf("manageChoiceFromResult(%q, %+v) = (%v, %v), want (%v, true)", tc.result, p, got, ok, tc.want)
			}
		}
	}

	if got, ok := manageChoiceFromResult("unexpected\n", ProgramSummary{}); ok || got != ManageCancel {
		t.Fatalf("an unexpected dialog result must fail closed, got (%v, %v)", got, ok)
	}
}

func TestManageProgramMessageCarriesTheWholeSummary(t *testing.T) {
	p := ProgramSummary{
		Name:        "search",
		Path:        "/opt/helpers/search",
		AnyDocument: true,
		Decisions:   7,
		Refusals:    3,
		Missing:     true,
	}
	message := manageProgramMessage(p)
	for _, want := range []string{p.Name, p.Path, "missing", "any document", "7", "refused search: 3"} {
		if !strings.Contains(message, want) {
			t.Errorf("management summary does not contain %q: %q", want, message)
		}
	}
}

// Forgetting a refusal is harmless unless something that name runs is allowed
// for any document: then the forget hands that program the document without a
// prompt. The summary says so, and says nothing when there is no such program.
func TestManageProgramMessageWarnsWhenForgettingRunsAProgram(t *testing.T) {
	with := manageProgramMessage(ProgramSummary{Name: "search", Path: "/opt/search", Refusals: 1, RunsOnForget: "/bin/x"})
	if !strings.Contains(with, "without asking") || !strings.Contains(with, "/bin/x") {
		t.Errorf("a single any-document program must be named in the warning: %q", with)
	}
	noProgram := manageProgramMessage(ProgramSummary{Name: "search", Path: "/opt/search", Refusals: 1})
	if strings.Contains(noProgram, "without asking") {
		t.Errorf("no any-document program, no warning: %q", noProgram)
	}
	noRefusals := manageProgramMessage(ProgramSummary{Name: "search", Path: "/opt/search", RunsOnForget: "/bin/x"})
	if strings.Contains(noRefusals, "without asking") {
		t.Errorf("nothing to forget, no warning: %q", noRefusals)
	}
}
