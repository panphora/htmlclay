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
