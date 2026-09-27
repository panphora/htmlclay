//go:build windows

package platform

import (
	"strings"
	"testing"
)

// WinForms makes the focused button the default, so the first TabIndex is what
// Return clicks. It, Escape and the close box must land on Later when the caller
// has one, and on Deny when it does not, never on a grant.
func TestConfirmFormKeepsTheKeyboardOffEveryGrant(t *testing.T) {
	for _, tc := range []struct {
		later bool
		first string
	}{
		{true, "$order = @($later, $deny, $allow, $always)"},
		{false, "$order = @($deny, $allow, $always)"},
	} {
		script := confirmFormScript(tc.later)
		for _, want := range []string{tc.first, "$order[$i].TabIndex = $i", "$f.CancelButton = $order[0]", "$f.AcceptButton = $order[0]"} {
			if !strings.Contains(script, want) {
				t.Errorf("confirmFormScript(%v) is missing %q", tc.later, want)
			}
		}
		if strings.Contains(script, "$f.AcceptButton = $always") || strings.Contains(script, "$f.AcceptButton = $allow") {
			t.Errorf("confirmFormScript(%v) makes a grant the accept button", tc.later)
		}
	}
	if strings.Contains(confirmFormScript(false), "$later") {
		t.Error("a prompt without a Later label drew a Later button")
	}
}

// A message naming a program path can run past the 640px cap, so the text has
// to scroll instead of being cut off. A read-only TextBox also draws "&"
// literally, which a Label with mnemonics would swallow.
func TestConfirmFormScrollsAMessageThatDoesNotFit(t *testing.T) {
	for _, later := range []bool{true, false} {
		script := confirmFormScript(later)
		for _, want := range []string{"ScrollBars = 'Vertical'", "$l.Multiline = $true", "$l.ReadOnly = $true", "$f.Controls.AddRange(@($p, $row))"} {
			if !strings.Contains(script, want) {
				t.Errorf("confirmFormScript(%v) is missing %q", later, want)
			}
		}
	}
}

func TestConfirmFormHeightGrowsWithTheMessageAndStaysOnScreen(t *testing.T) {
	short := confirmFormHeight("a.htmlclay wants to use 1 program:\n\nsearch: C:\\bin\\search.exe")
	long := confirmFormHeight(strings.Repeat("search: C:\\Users\\someone\\AppData\\Local\\Programs\\search\\search.exe\n", 12))
	if short != 220 {
		t.Errorf("short message height = %d, want the 220 floor", short)
	}
	if long <= short || long > 640 {
		t.Errorf("long message height = %d, want more than %d and at most 640", long, short)
	}
}
