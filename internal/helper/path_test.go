//go:build !windows

package helper

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setLoginPathResolver(t *testing.T, resolve func() string) {
	t.Helper()
	old := resolveLoginPath
	resolveLoginPath = resolve
	loginPathCache.Lock()
	loginPathCache.resolved = false
	loginPathCache.value = ""
	loginPathCache.Unlock()
	t.Cleanup(func() {
		resolveLoginPath = old
		loginPathCache.Lock()
		loginPathCache.resolved = false
		loginPathCache.value = ""
		loginPathCache.Unlock()
	})
}

func writePathHelper(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0755); err != nil {
		t.Fatal(err)
	}
	interpreter := filepath.Join(bin, "test-helper-runtime")
	if err := os.WriteFile(interpreter, []byte("#!/bin/sh\nexec /bin/sh \"$@\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	program := filepath.Join(root, "program")
	if err := os.WriteFile(program, []byte("#!/usr/bin/env test-helper-runtime\nprintf '{\"type\":\"result\",\"value\":\"ok\"}\\n'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	return program, bin
}

func runPathHelper(t *testing.T, program string) []Event {
	t.Helper()
	var events []Event
	Run(context.Background(), Spec{
		Argv:       []string{program},
		Env:        []string{"PATH=/usr/bin:/bin"},
		Deadline:   5 * time.Second,
		Structured: true,
		LoginPath:  true,
		Stdin:      []byte(`{"type":"wire/request"}`),
		Stderr:     io.Discard,
	}, func(event Event) {
		events = append(events, event)
	})
	return events
}

func TestLoginPathIsCached(t *testing.T) {
	calls := 0
	setLoginPathResolver(t, func() string {
		calls++
		return "/first"
	})
	if loginPath() != "/first" || loginPath() != "/first" {
		t.Fatal("cached login path changed")
	}
	if calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", calls)
	}
}

func TestResolveLoginPathUsesTheLoginShell(t *testing.T) {
	root := t.TempDir()
	argsPath := filepath.Join(root, "args")
	shell := filepath.Join(root, "shell")
	body := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argsPath + "'\nprintf '/from-login-shell'\n"
	if err := os.WriteFile(shell, []byte(body), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", shell)

	start := time.Now()
	got := resolveLoginPath()
	elapsed := time.Since(start)

	// The argv is asserted first and unconditionally. This file is written by the
	// fake shell, so it exists only if the login shell really ran, and it records
	// exactly what it was asked for.
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("the login shell was never run: %v", err)
	}
	if string(args) != "-l\n-c\nprintf %s \"$PATH\"\n" {
		t.Fatalf("login shell arguments = %q", args)
	}
	// Only the returned value is timing dependent. resolveLoginPath gives the
	// shell loginPathBudget and then falls back to the inherited PATH, and on a
	// loaded machine spawning this freshly written script has taken longer than
	// that on its own, which says nothing about the code under test. Observed
	// once at exactly 3.01s during a full-suite run beside two other builds.
	if got == "" && elapsed >= loginPathBudget {
		t.Skipf("the fake login shell took %s, past the %s resolveLoginPath allows", elapsed, loginPathBudget)
	}
	if got != "/from-login-shell" {
		t.Fatalf("resolved PATH = %q", got)
	}
}

// A login profile that backgrounds anything inheriting stdout keeps that pipe
// open after the shell itself is killed, and Output() reads to EOF. Without
// WaitDelay this call does not return until the descendant exits, however long
// that is, while holding the lock every helper spawn waits on.
func TestResolveLoginPathReturnsWhileADescendantHoldsStdout(t *testing.T) {
	root := t.TempDir()
	shell := filepath.Join(root, "shell")
	body := "#!/bin/sh\nsleep 20 &\nsleep 20\n"
	if err := os.WriteFile(shell, []byte(body), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", shell)

	done := make(chan string, 1)
	go func() { done <- resolveLoginPath() }()
	select {
	case got := <-done:
		if got != "" {
			t.Fatalf("a shell that printed no PATH resolved to %q", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("resolveLoginPath never returned: the descendant's stdout pipe was never closed")
	}
}

func TestLoginPathRefreshesAfterInterpreterExit127(t *testing.T) {
	program, bin := writePathHelper(t)
	calls := 0
	setLoginPathResolver(t, func() string {
		calls++
		if calls == 1 {
			return "/usr/bin:/bin"
		}
		return bin + ":/usr/bin:/bin"
	})
	events := runPathHelper(t, program)
	if calls != 2 {
		t.Fatalf("resolver calls = %d, want 2", calls)
	}
	if len(events) != 1 || events[0].Kind != "result" {
		t.Fatalf("events = %+v, want one result", events)
	}
	var value string
	if err := json.Unmarshal(events[0].Value, &value); err != nil || value != "ok" {
		t.Fatalf("result = %q, %v", events[0].Value, err)
	}
}

func TestMissingInterpreterIsTypedAfterPathRefresh(t *testing.T) {
	program, _ := writePathHelper(t)
	calls := 0
	setLoginPathResolver(t, func() string {
		calls++
		return "/usr/bin:/bin"
	})
	events := runPathHelper(t, program)
	if calls != 2 {
		t.Fatalf("resolver calls = %d, want 2", calls)
	}
	if len(events) != 1 || events[0].Kind != "error" || events[0].Code != "helper_interpreter_missing" {
		t.Fatalf("events = %+v, want helper_interpreter_missing", events)
	}
	if !strings.Contains(events[0].Text, "test-helper-runtime") {
		t.Fatalf("error does not name the interpreter: %q", events[0].Text)
	}
}

func TestHelperExit127IsNotMistakenForAMissingInterpreter(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0755); err != nil {
		t.Fatal(err)
	}
	interpreter := filepath.Join(bin, "test-helper-runtime")
	if err := os.WriteFile(interpreter, []byte("#!/bin/sh\nexit 127\n"), 0755); err != nil {
		t.Fatal(err)
	}
	program := filepath.Join(root, "program")
	if err := os.WriteFile(program, []byte("#!/usr/bin/env test-helper-runtime\n"), 0755); err != nil {
		t.Fatal(err)
	}
	calls := 0
	setLoginPathResolver(t, func() string {
		calls++
		return bin + ":/usr/bin:/bin"
	})
	events := runPathHelper(t, program)
	if calls != 1 {
		t.Fatalf("resolver calls = %d, want no refresh for an available interpreter", calls)
	}
	if len(events) != 1 || events[0].Code != "helper_crashed" {
		t.Fatalf("events = %+v, want helper_crashed", events)
	}
}

func TestRegisteredStartFailuresDistinguishMissingAndNonExecutable(t *testing.T) {
	setLoginPathResolver(t, func() string { return "/usr/bin:/bin" })
	root := t.TempDir()
	missing := filepath.Join(root, "missing-helper")
	nonExecutable := filepath.Join(root, "non-executable-helper")
	if err := os.WriteFile(nonExecutable, []byte("#!/bin/sh\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		want string
	}{
		{missing, "does not exist"},
		{nonExecutable, "not executable"},
	} {
		events := runPathHelper(t, tc.path)
		if len(events) != 1 || events[0].Code != "helper_start_failed" || !strings.Contains(events[0].Text, tc.path) || !strings.Contains(events[0].Text, tc.want) {
			t.Errorf("events for %s = %+v, want typed %q diagnostic naming the path", tc.path, events, tc.want)
		}
	}
}

// The PATH retry exists for the host's own spawn failures. Both of its codes are
// valid APPLICATION codes too, and the protocol separates them only by source,
// so without that check a helper that exits 0 having emitted one is run a second
// time and repeats whatever it already did.
func TestPathRetryIgnoresAnApplicationErrorCarryingASpawnCode(t *testing.T) {
	for _, code := range []string{"helper_start_failed", "helper_interpreter_missing"} {
		if retryablePathFailure(Event{Kind: "error", Source: "application", Code: code}) {
			t.Errorf("an application error with code %q must not re-run the program", code)
		}
		if !retryablePathFailure(Event{Kind: "error", Source: "host", Code: code}) {
			t.Errorf("a host error with code %q must still retry once", code)
		}
	}
}
