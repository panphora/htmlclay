//go:build darwin

package platform

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func installFakeOSAScript(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "osascript")
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
	t.Setenv("HTMLCLAY_TEST_ARGS", args)
	t.Setenv("HTMLCLAY_TEST_OUTPUT", "")
	t.Setenv("HTMLCLAY_TEST_EXIT", "0")
	return args
}

func fakeOSAScriptResult(t *testing.T, output string, exit int) {
	t.Helper()
	t.Setenv("HTMLCLAY_TEST_OUTPUT", output)
	t.Setenv("HTMLCLAY_TEST_EXIT", strconv.Itoa(exit))
}

func osascriptArgs(t *testing.T, path string) []string {
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

func TestSelectFileDarwinForegroundsAndPreservesTrailingSpaces(t *testing.T) {
	argsPath := installFakeOSAScript(t)
	fakeOSAScriptResult(t, "/tmp/my helper  \n", 0)

	path, ok, err := SelectFile(`Choose "a helper"`)
	if err != nil || !ok || path != "/tmp/my helper  " {
		t.Fatalf("SelectFile() = (%q, %v, %v)", path, ok, err)
	}
	args := osascriptArgs(t, argsPath)
	if len(args) != 4 || args[0] != "-e" || args[1] != "activate me" || args[2] != "-e" {
		t.Fatalf("osascript args = %q, want the foreground activation followed by the dialog", args)
	}
	if !strings.Contains(args[3], "choose file") || strings.Contains(args[3], "choose folder") {
		t.Fatalf("picker script does not select a file: %q", args[3])
	}
}

func TestSelectFileDarwinSeparatesCancelFromFailure(t *testing.T) {
	installFakeOSAScript(t)
	fakeOSAScriptResult(t, "execution error: User canceled. (-128)\n", 1)
	if path, ok, err := SelectFile("Choose"); err != nil || ok || path != "" {
		t.Fatalf("cancel = (%q, %v, %v), want a clean no-choice", path, ok, err)
	}

	fakeOSAScriptResult(t, "execution error: broken (-1)\n", 1)
	if _, ok, err := SelectFile("Choose"); err == nil || ok {
		t.Fatalf("a launch failure must surface as an error, got ok=%v err=%v", ok, err)
	}
}

func TestPromptNameDarwinForegroundsAndPreservesTheValue(t *testing.T) {
	argsPath := installFakeOSAScript(t)
	fakeOSAScriptResult(t, "search  \n", 0)

	value, ok, err := PromptName("Add", "Name", "initial")
	if err != nil || !ok || value != "search  " {
		t.Fatalf("PromptName() = (%q, %v, %v)", value, ok, err)
	}
	args := osascriptArgs(t, argsPath)
	if len(args) != 4 || args[1] != "activate me" || !strings.Contains(args[3], "default answer") {
		t.Fatalf("name prompt args = %q", args)
	}
}

func TestPromptNameDarwinTreatsCancelAsNormal(t *testing.T) {
	installFakeOSAScript(t)
	fakeOSAScriptResult(t, "execution error: User canceled. (-128)\n", 1)
	if value, ok, err := PromptName("Add", "Name", ""); err != nil || ok || value != "" {
		t.Fatalf("cancel = (%q, %v, %v), want a clean no-choice", value, ok, err)
	}
}

func TestManageProgramDarwinMapsActionsAndFailsClosed(t *testing.T) {
	argsPath := installFakeOSAScript(t)
	p := ProgramSummary{Name: "search", Path: "/tmp/search", Decisions: 3}
	for _, tc := range []struct {
		output string
		want   ManageChoice
	}{
		{manageToggleLabel(false) + "\n", ManageToggleAnyDocument},
		{forgetDecisionsLabel + "\n", ManageForgetDecisions},
		{removeProgramLabel + "\n", ManageRemove},
		{"cancel\n", ManageCancel},
	} {
		fakeOSAScriptResult(t, tc.output, 0)
		got, err := ManageProgram(p)
		if err != nil || got != tc.want {
			t.Errorf("ManageProgram output %q = (%v, %v), want (%v, nil)", tc.output, got, err, tc.want)
		}
	}
	args := osascriptArgs(t, argsPath)
	if len(args) != 4 || args[1] != "activate me" {
		t.Fatalf("management dialog args = %q", args)
	}
	for _, want := range []string{manageToggleLabel(false), forgetDecisionsLabel, removeProgramLabel, "Cancel"} {
		if !strings.Contains(args[3], want) {
			t.Errorf("management script does not contain %q: %q", want, args[3])
		}
	}

	fakeOSAScriptResult(t, "garbled\n", 0)
	if got, err := ManageProgram(p); err == nil || got != ManageCancel {
		t.Fatalf("unexpected output must fail closed, got (%v, %v)", got, err)
	}
	fakeOSAScriptResult(t, "failed\n", 2)
	if got, err := ManageProgram(p); err == nil || got != ManageCancel {
		t.Fatalf("execution failure must fail closed, got (%v, %v)", got, err)
	}
}

// The AppleScript is checked by the real compiler rather than by reading it.
// osacompile parses and type-checks the source without ever running it, so no
// dialog reaches the screen, and a clause dropped in the wrong place fails here
// instead of on a user's machine.
func TestManageProgramDarwinPreselectsNothingAndStillCompiles(t *testing.T) {
	script := manageProgramScript(ProgramSummary{Name: "search", Path: "/tmp/search", Decisions: 3})
	if strings.Contains(script, "default items") {
		t.Fatalf("the management list must arrive with nothing selected: %q", script)
	}

	dir := t.TempDir()
	source := filepath.Join(dir, "manage.applescript")
	if err := os.WriteFile(source, []byte(script), 0644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("/usr/bin/osacompile", "-o", filepath.Join(dir, "manage.scpt"), source).CombinedOutput()
	if err != nil {
		t.Fatalf("osacompile rejected the management script: %v\n%s\n%s", err, out, script)
	}
}
