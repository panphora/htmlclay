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
		Missing:     true,
	}
	message := manageProgramMessage(p)
	for _, want := range []string{p.Name, p.Path, "missing", "any document", "7"} {
		if !strings.Contains(message, want) {
			t.Errorf("management summary does not contain %q: %q", want, message)
		}
	}
}
