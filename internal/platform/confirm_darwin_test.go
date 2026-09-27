//go:build darwin

package platform

import (
	"os"
	"os/exec"
	"path/filepath"
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
		{"button returned:, gave up:true\n", ConfirmDismissed, "an ignored dialog gives up carrying no button, which is no answer"},
		{"Allow for Any Document\n", ConfirmDismissed, "text that is not an answer is never a click"},
		{"", ConfirmDismissed, "no output at all fails closed without a refusal"},
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

// display dialog has room for three buttons, so a Later label cannot be drawn.
// What it buys on macOS is the absence of a default button and of a cancel
// button: Return and Escape then do nothing, and only a click answers. Without
// Later the read prompt keeps Deny as its default, as it always had.
func TestConfirmDarwinKeepsReturnOffDenyWhenARefusalIsStored(t *testing.T) {
	labels := ConfirmLabels{Allow: "Allow for This Document", Always: "Allow for Any Document", Later: "Not Now"}
	stored := confirmDialogScript("Allow document programs?", "a.htmlclay wants to use 1 program", labels)
	if strings.Contains(stored, "default button") || strings.Contains(stored, "cancel button") {
		t.Fatalf("a prompt whose refusal is stored must have no default and no cancel button:\n%s", stored)
	}
	if strings.Contains(stored, "Not Now") {
		t.Fatalf("a fourth button does not fit display dialog:\n%s", stored)
	}
	labels.Later = ""
	read := confirmDialogScript("Allow access?", "a.htmlclay wants to read a folder", labels)
	if !strings.Contains(read, `default button "Deny"`) {
		t.Fatalf("without Later, Deny stays the default:\n%s", read)
	}

	dir := t.TempDir()
	for i, script := range []string{stored, read} {
		source := filepath.Join(dir, "confirm.applescript")
		if err := os.WriteFile(source, []byte(script), 0644); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("/usr/bin/osacompile", "-o", filepath.Join(dir, "confirm.scpt"), source).CombinedOutput()
		if err != nil {
			t.Fatalf("osacompile rejected confirm script %d: %v\n%s\n%s", i, err, out, script)
		}
	}
}

func TestConfirmDarwinReportsAFailedDialogAsNoAnswer(t *testing.T) {
	installFakeOSAScript(t)
	fakeOSAScriptResult(t, "execution error\n", 1)
	got, err := Confirm("Title", "message", ConfirmLabels{Allow: "Allow", Always: "Always", Later: "Not Now"})
	if err == nil || got != ConfirmDismissed {
		t.Fatalf("Confirm() on a failed osascript = (%v, %v), want (ConfirmDismissed, error)", got, err)
	}
}
