//go:build windows

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

const detachedHostTestMode = "HTMLCLAY_DETACHED_HOST_TEST_MODE"

// A host without a console, the shape a GUI build or a DETACHED_PROCESS launch
// gives, has to give its child a console of its own and attach to it to send
// Ctrl+Break. The test binary is re-run detached as that host. It runs the
// invalid-output scenario, whose child records the stop in a marker file.
func TestRunStructuredAsksAChildToStopFromAHostWithoutAConsole(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "stopped")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	host := exec.Command(exe, "-test.run=^TestStructuredDetachedHostProcess$", "-test.v")
	host.Env = append(os.Environ(), detachedHostTestMode+"=1", "HTMLCLAY_STRUCTURED_STOPPED="+marker)
	host.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS}
	out, err := host.CombinedOutput()
	if err != nil {
		t.Fatalf("detached host failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the child of a console-less host did not record the stop: %v\n%s", err, out)
	}
}

func TestStructuredDetachedHostProcess(t *testing.T) {
	if os.Getenv(detachedHostTestMode) == "" {
		return
	}
	events := runnerEvents(context.Background(), structuredRunnerCommand(t, "invalid"))
	requireStructuredTerminal(t, events, "helper_bad_output")
	if _, err := os.Stat(os.Getenv("HTMLCLAY_STRUCTURED_STOPPED")); err != nil {
		t.Fatalf("child did not record the cleanup signal: %v", err)
	}
}
