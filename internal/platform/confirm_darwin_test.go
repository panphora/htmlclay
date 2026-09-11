//go:build darwin

package platform

import (
	"strings"
	"testing"
)

// The three-button dialog takes both affirmative labels from its caller, because
// the same shape asks two unrelated questions: trust a folder, or allow a
// program. A prompt that borrowed the other question's buttons would name the
// wrong grant, which is the one thing a permission dialog must never do.
func TestConfirmDarwinCarriesTheCallersLabels(t *testing.T) {
	argsPath := installFakeOSAScript(t)
	fakeOSAScriptResult(t, "button returned:Allow for Any Document, gave up:false\n", 0)

	choice, err := Confirm("Allow document programs?", "a.htmlclay wants to use 1 program",
		ConfirmLabels{Allow: "Allow for This Document", Always: "Allow for Any Document"})
	if err != nil || choice != ConfirmAllowAlways {
		t.Fatalf("Confirm() = (%v, %v), want ConfirmAllowAlways", choice, err)
	}
	args := osascriptArgs(t, argsPath)
	if len(args) != 4 || args[1] != "activate me" {
		t.Fatalf("confirm args = %q, want the foreground activation followed by the dialog", args)
	}
	want := `buttons {"Deny", "Allow for This Document", "Allow for Any Document"}`
	if !strings.Contains(args[3], want) {
		t.Fatalf("dialog script does not carry the caller's labels:\n%s\nwant %s", args[3], want)
	}
}

// Substring matching was enough while both labels were fixed and unrelated. It
// is not enough now: one label can be a prefix of the other, and CombinedOutput
// does not separate the answer from the message text quoted above the buttons.
func TestConfirmDarwinMatchesTheClickedButtonExactly(t *testing.T) {
	installFakeOSAScript(t)
	labels := ConfirmLabels{Allow: "Allow", Always: "Allow for Any Document"}
	cases := []struct {
		out  string
		want ConfirmChoice
		why  string
	}{
		{"button returned:Allow, gave up:false\n", ConfirmAllowOnce, "the narrower label is a prefix of the wider one"},
		{"button returned:Allow for Any Document, gave up:false\n", ConfirmAllowAlways, "the wider grant"},
		{"button returned:Deny, gave up:false\n", ConfirmDeny, "an explicit refusal"},
		{"button returned:, gave up:true\n", ConfirmDeny, "an ignored dialog gives up carrying no button"},
		{"Allow for Any Document\n", ConfirmDeny, "text that is not an answer is never a click"},
		{"", ConfirmDeny, "no output at all fails closed"},
	}
	for _, tc := range cases {
		fakeOSAScriptResult(t, tc.out, 0)
		got, err := Confirm("Title", "Allow for Any Document also appears in this message", labels)
		if err != nil || got != tc.want {
			t.Errorf("Confirm() on %q = (%v, %v), want %v (%s)", tc.out, got, err, tc.want, tc.why)
		}
	}
}

func TestConfirmWithButtonsDarwinMatchesTheClickedButtonExactly(t *testing.T) {
	installFakeOSAScript(t)
	fakeOSAScriptResult(t, "button returned:Make Executable, gave up:false\n", 0)
	if ok, err := confirmTwoButtons("Title", "Make Executable is quoted here too", "Make Executable"); err != nil || !ok {
		t.Fatalf("confirmTwoButtons() = (%v, %v), want true", ok, err)
	}
	fakeOSAScriptResult(t, "button returned:Deny, gave up:false\nMake Executable\n", 0)
	if ok, err := confirmTwoButtons("Title", "message", "Make Executable"); err != nil || ok {
		t.Fatalf("a refusal followed by the label in the output = (%v, %v), want false", ok, err)
	}
}
