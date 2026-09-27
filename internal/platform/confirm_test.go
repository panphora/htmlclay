package platform

import "testing"

var testLabels = ConfirmLabels{Allow: "Allow for This Document", Always: "Allow for Any Document", Later: "Not Now"}

// The zero value is the one outcome that can never be stored as a refusal.
func TestConfirmChoiceZeroValueIsNoAnswer(t *testing.T) {
	var choice ConfirmChoice
	if choice != ConfirmDismissed || choice.String() != "dismissed" {
		t.Fatalf("zero ConfirmChoice = %v (%s), want ConfirmDismissed", int(choice), choice)
	}
}

func TestChoiceFromOSAScriptStoresOnlyAClickOnDeny(t *testing.T) {
	for _, tc := range []struct {
		button string
		labels ConfirmLabels
		want   ConfirmChoice
		why    string
	}{
		{"Deny", testLabels, ConfirmDeny, "a click on Deny"},
		{"Allow for This Document", testLabels, ConfirmAllowOnce, "the narrower grant"},
		{"Allow for Any Document", testLabels, ConfirmAllowAlways, "the wider grant"},
		{"", testLabels, ConfirmDismissed, "the dialog gave up"},
		{"", ConfirmLabels{}, ConfirmDismissed, "an empty label never matches a dialog that gave up"},
		{"Not Now", testLabels, ConfirmDismissed, "macOS never draws Later, and an unknown label is no answer"},
		{"deny", testLabels, ConfirmDismissed, "the comparison is exact"},
	} {
		if got := choiceFromOSAScript(tc.button, tc.labels); got != tc.want {
			t.Errorf("choiceFromOSAScript(%q) = %v, want %v (%s)", tc.button, got, tc.want, tc.why)
		}
	}
}

// The exits below were recorded from zenity 4.0.1 under Xvfb: OK exits 0 with
// nothing printed, an extra button exits 1 printing its label, and the cancel
// button, Return on it, and Escape exit 1 printing nothing.
func TestChoiceFromZenityStoresOnlyAClickOnDeny(t *testing.T) {
	readLabels := ConfirmLabels{Allow: "Allow Once", Always: "Trust This Folder"}
	for _, tc := range []struct {
		out    string
		exit   int
		labels ConfirmLabels
		want   ConfirmChoice
		why    string
	}{
		{"Deny\n", 1, testLabels, ConfirmDeny, "the Deny extra button"},
		{"", 0, testLabels, ConfirmAllowOnce, "OK"},
		{"Allow for Any Document\n", 1, testLabels, ConfirmAllowAlways, "the wider extra button"},
		{"", 1, testLabels, ConfirmDismissed, "Not Now, Return on it, Escape, or the close box"},
		{"", -1, testLabels, ConfirmDismissed, "killed at the deadline"},
		{"", 5, testLabels, ConfirmDismissed, "zenity's own timeout"},
		{"Allow for Any Document\n", 0, testLabels, ConfirmDismissed, "a label with a clean exit is not a known answer"},
		{"", 1, readLabels, ConfirmDeny, "without Later the cancel button is Deny"},
		{"Trust This Folder\n", 1, readLabels, ConfirmAllowAlways, "the read prompt's wider grant"},
		{"", 1, ConfirmLabels{Later: "Not Now"}, ConfirmDismissed, "an empty Always label never matches a silent exit"},
	} {
		if got := choiceFromZenity(tc.out, tc.exit, tc.labels); got != tc.want {
			t.Errorf("choiceFromZenity(%q, %d) = %v, want %v (%s)", tc.out, tc.exit, got, tc.want, tc.why)
		}
	}
}

// kdialog 23.08's radiolist returns the highlighted row's tag on OK even when
// nothing is checked, and exits 1 on Cancel, Escape and the close box.
func TestChoiceFromKDialogStoresOnlyAChosenDeny(t *testing.T) {
	for _, tc := range []struct {
		out  string
		exit int
		want ConfirmChoice
	}{
		{"deny\n", 0, ConfirmDeny},
		{"once\n", 0, ConfirmAllowOnce},
		{"always\n", 0, ConfirmAllowAlways},
		{"later\n", 0, ConfirmDismissed},
		{"\n", 1, ConfirmDismissed},
		{"deny\n", 1, ConfirmDismissed},
		{"", -1, ConfirmDismissed},
		{"Deny\n", 0, ConfirmDismissed},
	} {
		if got := choiceFromKDialog(tc.out, tc.exit); got != tc.want {
			t.Errorf("choiceFromKDialog(%q, %d) = %v, want %v", tc.out, tc.exit, got, tc.want)
		}
	}
}

// Everything unrecognized is no answer: a truncated line, a localized result
// name, or a PowerShell that printed something unexpected must never come back
// as a grant, and must not be remembered as a refusal either.
func TestChoiceFromDialogResultStoresOnlyAClickOnDeny(t *testing.T) {
	for _, tc := range []struct {
		out  string
		want ConfirmChoice
		why  string
	}{
		{"Yes", ConfirmAllowAlways, "the wider grant"},
		{"OK", ConfirmAllowOnce, "the narrower grant"},
		{"No", ConfirmDeny, "the Deny button"},
		{"Cancel", ConfirmDismissed, "Later, Escape, Return on Later, and the close box"},
		{"Yes\r\n", ConfirmAllowAlways, "PowerShell writes CRLF"},
		{"  OK  \n", ConfirmAllowOnce, "surrounding whitespace is not meaning"},
		{"", ConfirmDismissed, "no output at all"},
		{"Ja", ConfirmDismissed, "a localized or unexpected name"},
		{"NO", ConfirmDismissed, "the comparison is exact"},
	} {
		if got := choiceFromDialogResult(tc.out); got != tc.want {
			t.Errorf("choiceFromDialogResult(%q) = %v, want %v (%s)", tc.out, got, tc.want, tc.why)
		}
	}
}
