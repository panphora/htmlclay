//go:build windows

package platform

import "testing"

// The mapping from the form's printed DialogResult onto a choice. No test can
// click a native window, so this is the part of the new three-button dialog that
// can be checked automatically, and it is the part where a wrong answer would
// silently hand out a durable grant.
//
// The gate that matters is that everything unrecognized denies: a truncated line,
// a localized result name, or a PowerShell that printed something unexpected must
// never come back as trust.
func TestChoiceFromDialogResultFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		out  string
		want ConfirmChoice
		why  string
	}{
		{"Yes", ConfirmTrustFolder, "the Trust This Folder button"},
		{"OK", ConfirmAllowOnce, "the Allow Once button"},
		{"Cancel", ConfirmDeny, "the Deny button, and Escape, and the close box"},
		{"Yes\r\n", ConfirmTrustFolder, "PowerShell writes CRLF"},
		{"  OK  \n", ConfirmAllowOnce, "surrounding whitespace is not meaning"},
		{"", ConfirmDeny, "no output at all"},
		{"Ja", ConfirmDeny, "a localized or unexpected name is not a grant"},
		{"YES", ConfirmDeny, "the comparison is exact, and anything else denies"},
	} {
		if got := choiceFromDialogResult(tc.out); got != tc.want {
			t.Errorf("choiceFromDialogResult(%q) = %v, want %v (%s)", tc.out, got, tc.want, tc.why)
		}
	}
}
