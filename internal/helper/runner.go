package helper

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	childDrain = 2 * time.Second
	waitDelay  = 5 * time.Second
)

// Spec is everything needed to run one helper invocation.
type Spec struct {
	Argv       []string
	Dir        string
	Env        []string
	Deadline   time.Duration
	Structured bool
	LoginPath  bool
	Stdin      []byte
	Stderr     io.Writer
	Start      func(*exec.Cmd) error
}

// Event is one thing the runner produced. Exactly one terminal event is emitted
// per run, always last.
type Event struct {
	Kind       string
	Text       string
	Progress   json.RawMessage
	Value      json.RawMessage
	Code       string
	Details    json.RawMessage
	Source     string
	diagnostic string
}

// Run executes one invocation. LoginPath requests use the cached login shell
// PATH and retry once with a fresh value after a start or interpreter failure.
func Run(ctx context.Context, spec Spec, emit func(Event)) {
	if spec.Deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.Deadline)
		defer cancel()
	}

	originalEnv := spec.Env
	refreshPath := spec.LoginPath && runtime.GOOS != "windows"
	if refreshPath {
		spec.Env = withPath(originalEnv, loginPath())
	}
	terminal := runOnce(ctx, spec, emit)
	if refreshPath && ctx.Err() == nil && retryablePathFailure(terminal) {
		spec.Env = withPath(originalEnv, refreshLoginPath())
		terminal = runOnce(ctx, spec, emit)
	}
	emit(terminal)
	if spec.Stderr != nil {
		diagnostic := "done"
		if terminal.diagnostic != "" {
			diagnostic = terminal.diagnostic
		} else if terminal.Kind == "error" {
			diagnostic = "error: " + terminal.Text
		}
		fmt.Fprintln(spec.Stderr, diagnostic)
	}
}

func runOnce(ctx context.Context, spec Spec, emit func(Event)) Event {
	if spec.Structured {
		return runStructured(ctx, spec, emit)
	}
	return runRaw(ctx, spec, emit)
}

func runRaw(ctx context.Context, spec Spec, emit func(Event)) Event {
	if len(spec.Argv) == 0 {
		return Event{Kind: "error", Text: "no helper command"}
	}
	argv, sys := launchArgv(spec.Argv[0])
	argv = append(argv, spec.Argv[1:]...)
	cmd := command(ctx, spec, argv, sys)

	pr, pw, err := os.Pipe()
	if err != nil {
		return Event{Kind: "error", Text: err.Error()}
	}
	cmd.Stdout = pw
	if err := startCommand(spec, cmd); err != nil {
		pw.Close()
		pr.Close()
		if !spec.LoginPath {
			return Event{Kind: "error", Text: err.Error()}
		}
		return startFailure(ctx, spec.Argv[0], err)
	}
	pw.Close()

	read := make(chan struct{})
	go func() {
		defer close(read)
		r := bufio.NewReaderSize(pr, 64<<10)
		for {
			line, readErr := readRawLine(r, maxStatusText)
			if line != "" {
				emit(Event{Kind: "status", Text: line})
			}
			if readErr != nil {
				return
			}
		}
	}()

	waitErr := cmd.Wait()
	flush(spec.Stderr)
	select {
	case <-read:
	case <-time.After(childDrain):
		pr.Close()
		<-read
	}
	pr.Close()

	switch {
	case ctx.Err() != nil:
		if !spec.LoginPath {
			return Event{Kind: "error", Text: "cancelled", diagnostic: "cancelled"}
		}
		return contextError(ctx.Err())
	case waitErr == nil:
		return Event{Kind: "result"}
	case errors.Is(waitErr, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 0:
		return Event{Kind: "result", diagnostic: "done, after waiting out something the handler left holding a pipe"}
	default:
		return Event{Kind: "error", Text: waitErr.Error()}
	}
}

type stopLatch struct {
	mu        sync.Mutex
	event     *Event
	committed bool
}

func (l *stopLatch) stop(event Event) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.event != nil || l.committed {
		return false
	}
	copy := event
	l.event = &copy
	return true
}

func (l *stopLatch) commit(event Event) Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.event != nil {
		l.committed = true
		return *l.event
	}
	l.committed = true
	return event
}

type readResult struct {
	terminal *Event
	cleanEOF bool
}

func runStructured(ctx context.Context, spec Spec, emit func(Event)) Event {
	if len(spec.Argv) == 0 {
		return structuredStartFailure(ctx, spec, "", errors.New("no helper command"))
	}
	argv, err := resolveArgv(spec.Argv)
	if err != nil {
		return structuredStartFailure(ctx, spec, spec.Argv[0], err)
	}

	childCtx, stopChild := context.WithCancel(ctx)
	defer stopChild()
	launch, sys := launchArgv(argv[0])
	launch = append(launch, argv[1:]...)
	childSpec := spec
	childSpec.Argv = argv
	cmd := command(childCtx, childSpec, launch, sys)

	pr, pw, err := os.Pipe()
	if err != nil {
		return structuredStartFailure(ctx, spec, argv[0], err)
	}
	cmd.Stdout = pw
	if err := startCommand(spec, cmd); err != nil {
		pw.Close()
		pr.Close()
		return structuredStartFailure(ctx, spec, argv[0], err)
	}
	pw.Close()

	var stops stopLatch
	watchDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			if stops.stop(contextError(ctx.Err())) {
				stopChild()
				pr.Close()
			}
		case <-watchDone:
		}
	}()

	readDone := make(chan readResult, 1)
	go func() {
		readDone <- readStructuredOutput(pr, emit, func(event Event) {
			if stops.stop(event) {
				stopChild()
				pr.Close()
			}
		})
	}()

	waitErr := cmd.Wait()
	flush(spec.Stderr)
	if ctx.Err() != nil {
		stops.stop(contextError(ctx.Err()))
	}
	exitZero := waitErr == nil || errors.Is(waitErr, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 0
	if !exitZero && ctx.Err() == nil {
		if spec.LoginPath && exitCode(waitErr) == 127 {
			if interpreter := missingEnvInterpreter(argv[0], spec.Env, spec.Dir); interpreter != "" {
				stops.stop(interpreterFailure(argv[0], interpreter))
			} else {
				stops.stop(hostError("helper_crashed", waitErr.Error(), nil))
			}
		} else {
			stops.stop(hostError("helper_crashed", waitErr.Error(), nil))
		}
	}

	var read readResult
	select {
	case read = <-readDone:
	case <-time.After(childDrain):
		stops.stop(hostError("helper_bad_output", "helper stdout did not reach clean EOF", failureDetails("stdout", childDrain.String())))
		pr.Close()
		read = <-readDone
	}
	pr.Close()

	var terminal Event
	switch {
	case read.terminal != nil && read.cleanEOF && exitZero:
		terminal = *read.terminal
	case exitZero:
		terminal = hostError("helper_no_result", "helper exited without a terminal record", nil)
	default:
		terminal = hostError("helper_crashed", waitErr.Error(), nil)
	}
	if ctx.Err() != nil {
		stops.stop(contextError(ctx.Err()))
	}
	terminal = stops.commit(terminal)
	close(watchDone)
	return terminal
}

func structuredStartFailure(ctx context.Context, spec Spec, path string, err error) Event {
	if !spec.LoginPath {
		if ctx.Err() != nil {
			return contextError(ctx.Err())
		}
		return hostError("helper_start_failed", err.Error(), nil)
	}
	return startFailure(ctx, path, err)
}

func command(ctx context.Context, spec Spec, argv []string, sys *syscall.SysProcAttr) *exec.Cmd {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.SysProcAttr = sys
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	cmd.Stdin = bytes.NewReader(spec.Stdin)
	cmd.Stderr = spec.Stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(childStop) }
	cmd.WaitDelay = waitDelay
	return cmd
}

func startCommand(spec Spec, cmd *exec.Cmd) error {
	if spec.Start != nil {
		return spec.Start(cmd)
	}
	return cmd.Start()
}

func resolveArgv(argv []string) ([]string, error) {
	resolved := append([]string(nil), argv...)
	path := resolved[0]
	if !filepath.IsAbs(path) {
		var err error
		path, err = exec.LookPath(path)
		if err != nil {
			return nil, err
		}
		path, err = filepath.Abs(path)
		if err != nil {
			return nil, err
		}
	}
	resolved[0] = path
	return resolved, nil
}

func readStructuredOutput(r io.Reader, emit func(Event), stop func(Event)) readResult {
	reader := bufio.NewReaderSize(r, 64<<10)
	var terminal *Event
	for {
		record, err := readRecord(reader, MaxRecord)
		if errors.Is(err, io.EOF) {
			return readResult{terminal: terminal, cleanEOF: true}
		}
		if err != nil {
			details := failureDetails("record", err.Error())
			if errors.Is(err, errRecordTooLarge) {
				details = limitDetails("record", MaxRecord)
			}
			stop(hostError("helper_bad_output", "invalid helper output: "+err.Error(), details))
			return readResult{}
		}
		if terminal != nil {
			stop(hostError("helper_bad_output", "helper wrote a record after its terminal record", failureDetails("record", record.typeName)))
			return readResult{}
		}
		switch record.typeName {
		case "status":
			emit(Event{Kind: "status", Text: record.text, Progress: record.progress})
		case "result":
			event := Event{Kind: "result", Value: record.value}
			terminal = &event
		case "error":
			event := Event{Kind: "error", Text: record.message, Code: record.code, Details: record.details, Source: "application"}
			terminal = &event
		}
	}
}

func startFailure(ctx context.Context, path string, err error) Event {
	if ctx.Err() != nil {
		return contextError(ctx.Err())
	}
	if errors.Is(err, context.Canceled) {
		return contextError(err)
	}
	if path == "" {
		return hostError("helper_start_failed", err.Error(), nil)
	}
	info, statErr := os.Stat(path)
	switch {
	case errors.Is(statErr, os.ErrNotExist):
		return hostError("helper_start_failed", fmt.Sprintf("helper program does not exist: %s", path), nil)
	case statErr == nil && runtime.GOOS != "windows" && info.Mode().Perm()&0111 == 0:
		return hostError("helper_start_failed", fmt.Sprintf("helper program is not executable: %s", path), nil)
	case statErr == nil && errors.Is(err, syscall.ENOENT):
		if interpreter := interpreterName(path); interpreter != "" {
			return interpreterFailure(path, interpreter)
		}
	}
	return hostError("helper_start_failed", fmt.Sprintf("could not start helper %s: %v", path, err), nil)
}

func interpreterFailure(path, interpreter string) Event {
	return hostError("helper_interpreter_missing", fmt.Sprintf("helper interpreter %q is unavailable for %s", interpreter, path), nil)
}

func interpreterName(path string) string {
	fields := shebangFields(path)
	if len(fields) == 0 {
		return ""
	}
	if filepath.Base(fields[0]) != "env" {
		return filepath.Base(fields[0])
	}
	return envInterpreter(fields)
}

func missingEnvInterpreter(path string, env []string, dir string) string {
	fields := shebangFields(path)
	if len(fields) == 0 || filepath.Base(fields[0]) != "env" {
		return ""
	}
	name := envInterpreter(fields)
	if name == "" || executableInEnv(name, env, dir) {
		return ""
	}
	return name
}

func shebangFields(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	line, _ := bufio.NewReader(io.LimitReader(f, 4096)).ReadString('\n')
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "#!") {
		return nil
	}
	return strings.Fields(strings.TrimSpace(strings.TrimPrefix(line, "#!")))
}

func envInterpreter(fields []string) string {
	for _, field := range fields[1:] {
		if strings.HasPrefix(field, "-") || strings.Contains(field, "=") {
			continue
		}
		return filepath.Base(field)
	}
	return ""
}

func executableInEnv(name string, env []string, dir string) bool {
	path := ""
	for _, entry := range env {
		key, value, found := strings.Cut(entry, "=")
		if found && key == "PATH" {
			path = value
			break
		}
	}
	for _, base := range filepath.SplitList(path) {
		if !filepath.IsAbs(base) && dir != "" {
			base = filepath.Join(dir, base)
		}
		info, err := os.Stat(filepath.Join(base, name))
		if err == nil && !info.IsDir() && info.Mode().Perm()&0111 != 0 {
			return true
		}
	}
	return false
}

func exitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// retryablePathFailure reports whether this terminal is a spawn failure worth
// one retry against a freshly resolved login PATH.
//
// The source is part of the test, not decoration. Both codes are valid
// APPLICATION codes as well, and the protocol distinguishes the two only by
// source, so a helper that exits 0 having emitted
// {"type":"error","code":"helper_start_failed"} would be run a SECOND time and
// repeat every side effect it had already performed.
func retryablePathFailure(event Event) bool {
	return event.Kind == "error" && event.Source == "host" &&
		(event.Code == "helper_start_failed" || event.Code == "helper_interpreter_missing")
}

func contextError(err error) Event {
	if errors.Is(err, context.DeadlineExceeded) {
		return hostError("helper_timeout", "helper timed out", nil)
	}
	return hostError("helper_cancelled", "helper cancelled", nil)
}

func hostError(code, message string, details json.RawMessage) Event {
	return Event{Kind: "error", Text: message, Code: code, Details: details, Source: "host"}
}

func flush(w io.Writer) {
	if flusher, ok := w.(interface{ Flush() }); ok {
		flusher.Flush()
	}
}

func readRawLine(r *bufio.Reader, max int) (string, error) {
	var b strings.Builder
	for {
		chunk, err := r.ReadSlice('\n')
		if room := max - b.Len(); room > 0 {
			if len(chunk) > room {
				chunk = chunk[:room]
			}
			b.Write(chunk)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return strings.TrimRight(b.String(), "\r\n"), err
	}
}
