//go:build linux

package platform

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/godbus/dbus/v5"
)

func installLinuxDialogTool(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, name)
	body := `#!/bin/sh
printf '%s\0' "$@" > "$HTMLCLAY_TEST_ARGS"
printf '%s' "$HTMLCLAY_TEST_OUTPUT"
exit "$HTMLCLAY_TEST_EXIT"
`
	if err := os.WriteFile(bin, []byte(body), 0755); err != nil {
		t.Fatal(err)
	}
	args := filepath.Join(dir, "args")
	t.Setenv("PATH", dir)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+filepath.Join(dir, "missing-bus"))
	t.Setenv("HTMLCLAY_TEST_ARGS", args)
	t.Setenv("HTMLCLAY_TEST_OUTPUT", "")
	t.Setenv("HTMLCLAY_TEST_EXIT", "0")
	return args
}

func linuxDialogResult(t *testing.T, output string, exit int) {
	t.Helper()
	t.Setenv("HTMLCLAY_TEST_OUTPUT", output)
	t.Setenv("HTMLCLAY_TEST_EXIT", strconv.Itoa(exit))
}

func linuxDialogArgs(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parts := bytes.Split(b, []byte{0})
	args := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) > 0 {
			args = append(args, string(part))
		}
	}
	return args
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func TestSelectFileLinuxZenityFallbackSelectsAFile(t *testing.T) {
	argsPath := installLinuxDialogTool(t, "zenity")
	linuxDialogResult(t, "/tmp/helper  \n", 0)

	path, ok, err := SelectFile("Choose a helper")
	if err != nil || !ok || path != "/tmp/helper  " {
		t.Fatalf("SelectFile() = (%q, %v, %v)", path, ok, err)
	}
	args := linuxDialogArgs(t, argsPath)
	if !containsArg(args, "--file-selection") || containsArg(args, "--directory") {
		t.Fatalf("zenity args = %q, want file selection without directory mode", args)
	}
}

func TestSelectFileLinuxKdialogFallbackAndCancel(t *testing.T) {
	argsPath := installLinuxDialogTool(t, "kdialog")
	linuxDialogResult(t, "/tmp/helper\n", 0)
	if path, ok, err := SelectFile("Choose a helper"); err != nil || !ok || path != "/tmp/helper" {
		t.Fatalf("SelectFile() = (%q, %v, %v)", path, ok, err)
	}
	if args := linuxDialogArgs(t, argsPath); !containsArg(args, "--getopenfilename") {
		t.Fatalf("kdialog args = %q, want an open-file dialog", args)
	}

	linuxDialogResult(t, "", 1)
	if path, ok, err := SelectFile("Choose a helper"); err != nil || ok || path != "" {
		t.Fatalf("cancel = (%q, %v, %v), want a clean no-choice", path, ok, err)
	}
}

func TestPromptNameLinuxUsesBothBackends(t *testing.T) {
	for _, tc := range []struct {
		tool string
		arg  string
	}{
		{"zenity", "--entry"},
		{"kdialog", "--inputbox"},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			argsPath := installLinuxDialogTool(t, tc.tool)
			linuxDialogResult(t, "search  \n", 0)
			value, ok, err := PromptName("Add", "Name", "initial")
			if err != nil || !ok || value != "search  " {
				t.Fatalf("PromptName() = (%q, %v, %v)", value, ok, err)
			}
			if args := linuxDialogArgs(t, argsPath); !containsArg(args, tc.arg) || !containsArg(args, "initial") {
				t.Fatalf("%s args = %q", tc.tool, args)
			}
		})
	}
}

func TestManageProgramLinuxUsesRadiolistsOnBothBackends(t *testing.T) {
	for _, tc := range []struct {
		tool   string
		output string
		want   ManageChoice
	}{
		{"zenity", forgetDecisionsLabel + "\n", ManageForgetDecisions},
		{"kdialog", "remove\n", ManageRemove},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			argsPath := installLinuxDialogTool(t, tc.tool)
			linuxDialogResult(t, tc.output, 0)
			p := ProgramSummary{Name: "search", Path: "/tmp/search", Decisions: 3}
			got, err := ManageProgram(p)
			if err != nil || got != tc.want {
				t.Fatalf("ManageProgram() = (%v, %v), want (%v, nil)", got, err, tc.want)
			}
			args := linuxDialogArgs(t, argsPath)
			for _, want := range []string{"--radiolist", manageToggleLabel(false), forgetDecisionsLabel, removeProgramLabel} {
				if !containsArg(args, want) {
					t.Errorf("%s args do not contain %q: %q", tc.tool, want, args)
				}
			}
			if tc.tool == "zenity" && (!containsArg(args, "--print-column") || !containsArg(args, "2")) {
				t.Errorf("zenity must return the action column explicitly: %q", args)
			}
			if tc.tool == "zenity" && containsArg(args, "--no-markup") {
				t.Errorf("zenity list dialogs do not accept --no-markup: %q", args)
			}
			if tc.tool == "kdialog" && (len(args) < 7 || args[4] != "cancel" || args[5] != "No change") {
				t.Errorf("kdialog returns the first row on Return, so it must change nothing: %q", args)
			}
		})
	}
}

func TestManageProgramLinuxCancelAndFailureFailClosed(t *testing.T) {
	installLinuxDialogTool(t, "zenity")
	p := ProgramSummary{Name: "search"}

	linuxDialogResult(t, "", 1)
	if got, err := ManageProgram(p); err != nil || got != ManageCancel {
		t.Fatalf("cancel = (%v, %v), want (ManageCancel, nil)", got, err)
	}

	linuxDialogResult(t, "broken\n", 2)
	if got, err := ManageProgram(p); err == nil || got != ManageCancel {
		t.Fatalf("failure must fail closed, got (%v, %v)", got, err)
	}
}

func TestPortalFileMapsEveryOutcome(t *testing.T) {
	uris := func(u ...string) map[string]dbus.Variant {
		return map[string]dbus.Variant{"uris": dbus.MakeVariant(u)}
	}

	if path, ok, err := portalFile(0, uris("file:///home/me/My%20Helper")); err != nil || !ok || path != "/home/me/My Helper" {
		t.Errorf("success = (%q, %v, %v), want the unescaped path", path, ok, err)
	}
	if path, ok, err := portalFile(1, nil); err != nil || ok || path != "" {
		t.Errorf("cancel = (%q, %v, %v), want a clean no-choice", path, ok, err)
	}
	if _, ok, err := portalFile(2, nil); err == nil || ok {
		t.Error("a portal failure must surface as an error")
	}
	if _, ok, err := portalFile(0, map[string]dbus.Variant{}); err == nil || ok {
		t.Error("success with no uris must fail closed")
	}
	if _, ok, err := portalFile(0, uris("https://example.com/helper")); err == nil || ok {
		t.Error("a non-local uri must be refused")
	}
}

func TestLinuxUnexpectedManagementOutputFailsClosed(t *testing.T) {
	installLinuxDialogTool(t, "kdialog")
	linuxDialogResult(t, strings.Repeat("x", 3)+"\n", 0)
	if got, err := ManageProgram(ProgramSummary{Name: "search"}); err == nil || got != ManageCancel {
		t.Fatalf("unexpected output must fail closed, got (%v, %v)", got, err)
	}
}

// Recorded against zenity 4.0.1 under Xvfb: without --default-cancel, Return
// activates OK, which granted "Allow for This Document". With it, focus starts
// on the cancel button, and Return, Escape and Space there all exit 1 printing
// nothing. Deny is an extra button so its click prints "Deny" and is the only
// exit that reads as a refusal.
func TestConfirmLinuxZenityPutsReturnOnLaterAndReadsDenyByItsLabel(t *testing.T) {
	argsPath := installLinuxDialogTool(t, "zenity")
	labels := ConfirmLabels{Allow: "Allow for This Document", Always: "Allow for Any Document", Later: "Not Now"}
	for _, tc := range []struct {
		out  string
		exit int
		want ConfirmChoice
	}{
		{"Deny\n", 1, ConfirmDeny},
		{"", 1, ConfirmDismissed},
		{"", 0, ConfirmAllowOnce},
		{"Allow for Any Document\n", 1, ConfirmAllowAlways},
	} {
		linuxDialogResult(t, tc.out, tc.exit)
		if got, err := Confirm("Allow document programs?", "message", labels); err != nil || got != tc.want {
			t.Errorf("Confirm() on (%q, %d) = (%v, %v), want %v", tc.out, tc.exit, got, err, tc.want)
		}
	}
	args := linuxDialogArgs(t, argsPath)
	want := []string{"--question", "--no-markup", "--title", "Allow document programs?", "--text", "message",
		"--ok-label", "Allow for This Document", "--extra-button", "Allow for Any Document",
		"--extra-button", "Deny", "--cancel-label", "Not Now", "--default-cancel"}
	if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("zenity args = %q\nwant %q", args, want)
	}

	labels.Later = ""
	linuxDialogResult(t, "", 1)
	if got, _ := Confirm("Allow access?", "message", labels); got != ConfirmDeny {
		t.Fatalf("without Later the cancel button is Deny, got %v", got)
	}
	args = linuxDialogArgs(t, argsPath)
	if !containsArg(args, "--default-cancel") || containsArg(args, "Not Now") {
		t.Fatalf("the read prompt must keep Return on Deny and draw no Later button: %q", args)
	}
}

// Recorded against kdialog 23.08.5 under Xvfb: Return on --warningyesnocancel
// is Yes whatever the labels say, so the fallback is a radiolist whose first row,
// the one Return returns, decides nothing.
func TestConfirmLinuxKDialogPutsReturnOnTheFirstRow(t *testing.T) {
	argsPath := installLinuxDialogTool(t, "kdialog")
	labels := ConfirmLabels{Allow: "Allow for This Document", Always: "Allow for Any Document", Later: "Not Now"}
	linuxDialogResult(t, "later\n", 0)
	if got, err := Confirm("Allow document programs?", "message", labels); err != nil || got != ConfirmDismissed {
		t.Fatalf("the Later row = (%v, %v), want ConfirmDismissed", got, err)
	}
	args := linuxDialogArgs(t, argsPath)
	want := []string{"--title", "Allow document programs?", "--radiolist", "message",
		"later", "Not Now", "off", "deny", "Deny", "off",
		"once", "Allow for This Document", "off", "always", "Allow for Any Document", "off"}
	if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("kdialog args = %q\nwant %q", args, want)
	}
	linuxDialogResult(t, "deny\n", 0)
	if got, _ := Confirm("Allow document programs?", "message", labels); got != ConfirmDeny {
		t.Fatalf("the Deny row = %v, want ConfirmDeny", got)
	}
	linuxDialogResult(t, "", 1)
	if got, _ := Confirm("Allow document programs?", "message", labels); got != ConfirmDismissed {
		t.Fatalf("Escape = %v, want ConfirmDismissed", got)
	}

	labels.Later = ""
	linuxDialogResult(t, "deny\n", 0)
	Confirm("Allow access?", "message", labels)
	if args := linuxDialogArgs(t, argsPath); len(args) < 5 || args[4] != "deny" {
		t.Fatalf("without Later, Deny must be the first row: %q", args)
	}
}

func TestConfirmLinuxWithNoDialogToolIsNoAnswer(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	got, err := Confirm("Title", "message", ConfirmLabels{Allow: "Allow", Always: "Always", Later: "Not Now"})
	if err == nil || got != ConfirmDismissed {
		t.Fatalf("Confirm() with no tool = (%v, %v), want (ConfirmDismissed, error)", got, err)
	}
}

// kdialog's Yes holds Return, so the two-button fallback puts Deny there and
// the affirmative on No; zenity gets --default-cancel for the same reason.
func TestConfirmWithButtonsLinuxKeepsReturnOnDeny(t *testing.T) {
	argsPath := installLinuxDialogTool(t, "kdialog")
	linuxDialogResult(t, "", 0)
	if ok, err := ConfirmWithButtons("Title", "message", "Trust Folder"); err != nil || ok {
		t.Fatalf("kdialog Yes (Return) = (%v, %v), want a refusal", ok, err)
	}
	args := linuxDialogArgs(t, argsPath)
	want := []string{"--title", "Title", "--yes-label", "Deny", "--no-label", "Trust Folder",
		"--cancel-label", "Cancel", "--warningyesnocancel", "message"}
	if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("kdialog two-button args = %q\nwant %q", args, want)
	}
	linuxDialogResult(t, "", 1)
	if ok, _ := ConfirmWithButtons("Title", "message", "Trust Folder"); !ok {
		t.Fatal("kdialog No is the affirmative here")
	}
	linuxDialogResult(t, "", 2)
	if ok, _ := ConfirmWithButtons("Title", "message", "Trust Folder"); ok {
		t.Fatal("kdialog Escape must not allow")
	}

	argsPath = installLinuxDialogTool(t, "zenity")
	linuxDialogResult(t, "", 1)
	ConfirmWithButtons("Title", "message", "Trust Folder")
	if !containsArg(linuxDialogArgs(t, argsPath), "--default-cancel") {
		t.Fatal("zenity two-button dialog must keep Return on Deny")
	}
}
