//go:build linux

package platform_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/panphora/htmlclay/internal/platform"
)

// The startup check the tray row and the banner are built on. With neither tool
// installed every permission prompt fails closed, and the whole point of the
// check is that the user is told before they hit one.
//
// PATH is the only input, so the test is a PATH: a temp directory with nothing
// in it stands in for a minimal desktop, and a file named zenity in it stands in
// for one that has it. exec.LookPath needs the file to be executable, which is
// why the mode matters.
func TestMissingDialogAdviceOnADesktopWithNeitherTool(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	advice := platform.MissingDialogAdvice()
	if advice == "" {
		t.Fatal("with no zenity and no kdialog, the user has to be told something")
	}
	for _, want := range []string{"zenity", "kdialog"} {
		if !strings.Contains(advice, want) {
			t.Errorf("the advice must name %s so there is something to act on, got %q", want, advice)
		}
	}
}

func TestMissingDialogAdviceIsSilentWhenAToolIsPresent(t *testing.T) {
	for _, tool := range []string{"zenity", "kdialog"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, tool), []byte("#!/bin/sh\n"), 0755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir)
		if advice := platform.MissingDialogAdvice(); advice != "" {
			t.Errorf("with %s installed there is nothing to warn about, got %q", tool, advice)
		}
	}
}
