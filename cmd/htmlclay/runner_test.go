package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const runnerTestMode = "HTMLCLAY_RUNNER_TEST_MODE"

func runnerCommand(t *testing.T, mode string) HelperSpec {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return HelperSpec{
		Argv:   []string{exe, "-test.run=TestRunnerHelperProcess"},
		Env:    append(os.Environ(), runnerTestMode+"="+mode),
		Stderr: io.Discard,
	}
}

func runnerEvents(ctx context.Context, spec HelperSpec) []HelperEvent {
	var events []HelperEvent
	Run(ctx, spec, func(event HelperEvent) {
		events = append(events, event)
	})
	return events
}

func requireRunnerResult(t *testing.T, events []HelperEvent) {
	t.Helper()
	if len(events) == 0 {
		t.Fatal("runner emitted no events")
	}
	if terminal := events[len(events)-1]; terminal.Kind != "result" {
		t.Fatalf("terminal event = %+v, want result; all events: %+v", terminal, events)
	}
	for i, event := range events[:len(events)-1] {
		if event.Kind != "status" {
			t.Fatalf("event %d = %+v, want only status before the terminal", i, event)
		}
	}
}

func TestRunRawInheritsTheCallersDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "relative.txt"), []byte("from the caller cwd"), 0644); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	relativeExe, err := filepath.Rel(root, exe)
	if err != nil {
		t.Fatal(err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldDir) })

	spec := runnerCommand(t, "relative")
	spec.Argv[0] = relativeExe
	events := runnerEvents(context.Background(), spec)
	requireRunnerResult(t, events)
	if len(events) != 2 || events[0].Text != "from the caller cwd" {
		t.Fatalf("events = %+v, want the relative file followed by result", events)
	}
}

func TestRunRawHasNoHostDeadline(t *testing.T) {
	// Waiting out a long helper would only prove the deadline is not shorter
	// than the wait. Assert the branch itself: at zero, Run adds no bound.
	ctx, cancel := runDeadline(context.Background(), 0)
	defer cancel()
	if deadline, ok := ctx.Deadline(); ok {
		t.Fatalf("raw mode context carries a deadline at %s, want none", deadline)
	}

	ctx, cancel = runDeadline(context.Background(), time.Minute)
	defer cancel()
	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("structured mode context carries no deadline, want one")
	}
}

func TestRunRawOutlivesTheStructuredDeadline(t *testing.T) {
	// The behavioural half: a raw helper that runs well past the structured
	// mode's own ceiling still reaches its terminal.
	spec := runnerCommand(t, "silent")
	spec.Env = append(spec.Env, "HTMLCLAY_RUNNER_TEST_DURATION=1s")

	started := time.Now()
	requireRunnerResult(t, runnerEvents(context.Background(), spec))
	if elapsed := time.Since(started); elapsed < 900*time.Millisecond {
		t.Fatalf("silent helper returned after %s, want it to finish its own wait", elapsed)
	}
}

func TestRunRawPassesStdinAndEnvironment(t *testing.T) {
	spec := runnerCommand(t, "input")
	spec.Env = append(spec.Env, "HTMLCLAY_RUNNER_TEST_VALUE=from-env")
	spec.Stdin = []byte(`{"request":true}`)
	events := runnerEvents(context.Background(), spec)
	requireRunnerResult(t, events)
	if len(events) != 2 || events[0].Text != `from-env:{"request":true}` {
		t.Fatalf("events = %+v, want the environment and verbatim stdin followed by result", events)
	}
}

func TestRunRawCancellationSignalsTheChild(t *testing.T) {
	spec := runnerCommand(t, "silent")
	spec.Env = append(spec.Env, "HTMLCLAY_RUNNER_TEST_DURATION=20s")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	events := runnerEvents(ctx, spec)
	if len(events) != 1 || events[0].Kind != "error" || events[0].Text != "cancelled" {
		t.Fatalf("events = %+v, want one cancelled terminal", events)
	}
}

func TestRunRawSucceedsWhenADescendantHoldsStdout(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "descendant.pid")
	spec := runnerCommand(t, "orphan")
	spec.Env = append(spec.Env, "HTMLCLAY_RUNNER_TEST_PIDFILE="+pidFile)
	t.Cleanup(func() {
		raw, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil {
			t.Errorf("descendant pid file holds %q: %v", raw, err)
			return
		}
		if process, err := os.FindProcess(pid); err == nil {
			_ = process.Kill()
		}
	})

	started := time.Now()
	events := runnerEvents(context.Background(), spec)
	requireRunnerResult(t, events)
	if elapsed := time.Since(started); elapsed >= 10*time.Second {
		t.Fatalf("runner waited %s for the descendant instead of closing its pipe", elapsed)
	}
}

func TestRunRawTreatsExitZeroAfterWaitDelayAsSuccess(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "descendant.pid")
	var diagnostics bytes.Buffer
	spec := runnerCommand(t, "orphan-stderr")
	spec.Env = append(spec.Env, "HTMLCLAY_RUNNER_TEST_PIDFILE="+pidFile)
	spec.Stderr = &diagnostics
	t.Cleanup(func() {
		raw, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil {
			t.Errorf("descendant pid file holds %q: %v", raw, err)
			return
		}
		if process, err := os.FindProcess(pid); err == nil {
			_ = process.Kill()
		}
	})

	events := runnerEvents(context.Background(), spec)
	requireRunnerResult(t, events)
	if !strings.Contains(diagnostics.String(), "done, after waiting out something the handler left holding a pipe") {
		t.Fatalf("diagnostics = %q, want the WaitDelay success outcome", diagnostics.String())
	}
}

func TestLaunchArgv(t *testing.T) {
	if runtime.GOOS != "windows" {
		for _, path := range []string{"helper", "helper.bat", "helper.CMD"} {
			argv, attr := launchArgv(path)
			if !reflect.DeepEqual(argv, []string{path}) || attr != nil {
				t.Errorf("launchArgv(%q) = %#v, %#v; want the bare path", path, argv, attr)
			}
		}
		return
	}

	t.Setenv("COMSPEC", `C:\Windows\System32\cmd.exe`)
	for _, path := range []string{`C:\Helpers\search.bat`, `C:\Helpers\search.CMD`} {
		argv, attr := launchArgv(path)
		want := []string{`C:\Windows\System32\cmd.exe`, "/d", "/s", "/c", path}
		if !reflect.DeepEqual(argv, want) {
			t.Errorf("launchArgv(%q) argv = %#v, want %#v", path, argv, want)
		}
		if attr == nil {
			t.Fatalf("launchArgv(%q) returned no process attributes", path)
		}
		cmdLine := reflect.ValueOf(attr).Elem().FieldByName("CmdLine").String()
		if !strings.Contains(cmdLine, `/d /s /c ""`+path+`""`) {
			t.Errorf("launchArgv(%q) command line = %q", path, cmdLine)
		}
	}
	path := `C:\Helpers\search.exe`
	argv, attr := launchArgv(path)
	if !reflect.DeepEqual(argv, []string{path}) || attr != nil {
		t.Errorf("launchArgv(%q) = %#v, %#v; want the bare path", path, argv, attr)
	}
}

func TestHelperReadRawLineTruncatesAndKeepsDraining(t *testing.T) {
	long := strings.Repeat("x", 200<<10)
	r := bufio.NewReaderSize(strings.NewReader(long+"\nafter\n"), 4<<10)
	first, err := helperReadRawLine(r, 64)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 64 {
		t.Fatalf("first line is %d bytes, want 64", len(first))
	}
	second, err := helperReadRawLine(r, 64)
	if err != nil {
		t.Fatal(err)
	}
	if second != "after" {
		t.Fatalf("second line = %q, want the line after the oversized one", second)
	}
}

func TestRunnerHelperProcess(t *testing.T) {
	mode := os.Getenv(runnerTestMode)
	if mode == "" {
		return
	}
	switch mode {
	case "relative":
		data, err := os.ReadFile("relative.txt")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Println(string(data))
	case "silent":
		duration, err := time.ParseDuration(os.Getenv("HTMLCLAY_RUNNER_TEST_DURATION"))
		if err != nil {
			os.Exit(2)
		}
		time.Sleep(duration)
	case "input":
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			os.Exit(2)
		}
		fmt.Printf("%s:%s\n", os.Getenv("HTMLCLAY_RUNNER_TEST_VALUE"), data)
	case "orphan":
		child := exec.Command(os.Args[0], "-test.run=TestRunnerHelperProcess")
		child.Env = append(os.Environ(), runnerTestMode+"=silent", "HTMLCLAY_RUNNER_TEST_DURATION=20s")
		child.Stdout = os.Stdout
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		if pidFile := os.Getenv("HTMLCLAY_RUNNER_TEST_PIDFILE"); pidFile != "" {
			_ = os.WriteFile(pidFile, []byte(strconv.Itoa(child.Process.Pid)), 0644)
		}
		fmt.Println("working")
	case "orphan-stderr":
		child := exec.Command(os.Args[0], "-test.run=TestRunnerHelperProcess")
		child.Env = append(os.Environ(), runnerTestMode+"=silent", "HTMLCLAY_RUNNER_TEST_DURATION=20s")
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		if pidFile := os.Getenv("HTMLCLAY_RUNNER_TEST_PIDFILE"); pidFile != "" {
			_ = os.WriteFile(pidFile, []byte(strconv.Itoa(child.Process.Pid)), 0644)
		}
		fmt.Println("answered")
	}
	os.Exit(0)
}
