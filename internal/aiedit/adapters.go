package aiedit

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	maxStdout      = 4 << 20
	stderrTail     = 400
	childWaitDelay = 2 * time.Second
)

type adapterFunc func(ctx context.Context, e engine, bin, userPrompt string, o Options, env []string, report func(string)) (Result, error)

var adapters = map[string]adapterFunc{
	"claude":  claudeAdapter,
	"codex":   codexAdapter,
	"generic": genericAdapter,
}

// claudeArgs is the headless Claude Code invocation: no tools, no MCP servers
// from the person's config, one turn, streaming output.
func claudeArgs(model string) []string {
	return []string{
		"-p",
		"--model", model,
		"--append-system-prompt", systemPrompt,
		"--tools", "",
		"--strict-mcp-config",
		"--no-session-persistence",
		"--max-turns", "1",
		"--output-format", "stream-json",
		"--include-partial-messages",
		"--verbose",
	}
}

// A read-only sandbox still lets codex run shell commands; these features are
// what give it tools.
var codexDisabledFeatures = []string{
	"shell_tool", "unified_exec", "apps", "browser_use", "computer_use",
	"multi_agent", "plugins", "image_generation", "view_image",
}

func codexArgs(dir, outFile string) []string {
	argv := []string{
		"exec",
		"--ephemeral", "--ignore-user-config", "--ignore-rules",
		"--skip-git-repo-check",
		"--sandbox", "read-only",
	}
	for _, feature := range codexDisabledFeatures {
		argv = append(argv, "--disable", feature)
	}
	return append(argv, "-C", dir, "-o", outFile, "-")
}

type claudeEvent struct {
	Type       string `json:"type"`
	Subtype    string `json:"subtype"`
	Model      string `json:"model"`
	Result     string `json:"result"`
	StopReason string `json:"stop_reason"`
	IsError    bool   `json:"is_error"`
	Event      *struct {
		Delta *struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	} `json:"event"`
}

func claudeAdapter(ctx context.Context, e engine, bin, userPrompt string, _ Options, env []string, report func(string)) (Result, error) {
	scratch, err := os.MkdirTemp("", "ai-edit-claude-")
	if err != nil {
		return Result{}, engineFailure(err)
	}
	defer os.RemoveAll(scratch)

	argv := append([]string{bin}, claudeArgs(e.model)...)
	cmd := childCommand(ctx, argv, scratch, env)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, engineFailure(err)
	}
	stderr := &tailBuffer{limit: stderrTail}
	cmd.Stderr = stderr
	if err := startChild(cmd, userPrompt); err != nil {
		return Result{}, engineFailure(err)
	}

	var (
		result    *claudeEvent
		modelSeen = e.model
		// One byte past the cap is enough to know the reply is too large, so no
		// line, however long, ever buffers more than that.
		reader = bufio.NewReaderSize(io.LimitReader(stdout, maxStdout+1), 64<<10)
		read   int
	)
	for {
		line, readErr := reader.ReadString('\n')
		read += len(line)
		if read > maxStdout {
			killChild(cmd)
			cmd.Wait()
			return Result{}, &Error{Code: "engine_failed", Message: "reply too large"}
		}
		if text := strings.TrimSpace(line); text != "" {
			var event claudeEvent
			ok := json.Unmarshal([]byte(text), &event) == nil
			if ok {
				switch {
				case event.Type == "stream_event":
					if event.Event != nil && event.Event.Delta != nil && event.Event.Delta.Type == "text_delta" {
						report(event.Event.Delta.Text)
					}
				case event.Type == "system" && event.Subtype == "init":
					modelSeen = event.Model
				case event.Type == "result":
					seen := event
					result = &seen
				}
			}
		}
		if readErr != nil {
			break
		}
	}
	cmd.Wait()
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	if result == nil {
		message := fmt.Sprintf("claude exited (%d) without a result", exitCode(cmd))
		if tail := stderr.String(); tail != "" {
			message += ": " + tail
		}
		return Result{}, &Error{Code: "engine_failed", Message: message}
	}
	if result.IsError {
		text := result.Result
		if text == "" {
			text = result.Subtype
		}
		return Result{}, &Error{Code: "engine_failed", Message: text}
	}
	stop := result.StopReason
	if stop == "" {
		stop = result.Subtype
		if stop == "success" {
			stop = "end_turn"
		}
	}
	return Result{HTML: stripFences(result.Result), Model: modelSeen, StopReason: stop}, nil
}

func codexAdapter(ctx context.Context, e engine, bin, userPrompt string, _ Options, env []string, _ func(string)) (Result, error) {
	scratch, err := os.MkdirTemp("", "ai-edit-codex-")
	if err != nil {
		return Result{}, engineFailure(err)
	}
	defer os.RemoveAll(scratch)
	outFile := filepath.Join(scratch, "last-message.txt")

	argv := append([]string{bin}, codexArgs(scratch, outFile)...)
	cmd := childCommand(ctx, argv, scratch, env)
	stderr := &tailBuffer{limit: stderrTail}
	cmd.Stderr = stderr
	if err := startChild(cmd, systemPrompt+"\n\n"+userPrompt); err != nil {
		return Result{}, engineFailure(err)
	}
	cmd.Wait()
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	code := exitCode(cmd)
	if code != 0 {
		message := fmt.Sprintf("codex exited (%d)", code)
		if tail := stderr.String(); tail != "" {
			message += ": " + tail
		}
		return Result{}, &Error{Code: "engine_failed", Message: message}
	}
	text, err := readCapped(outFile)
	if err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(string(text)) == "" {
		return Result{}, &Error{Code: "engine_failed", Message: "codex produced no reply"}
	}
	return Result{HTML: stripFences(string(text)), Model: "codex", StopReason: "end_turn"}, nil
}

// readCapped reads the reply file without ever holding more than the cap plus one
// byte. A missing file reads as empty, which the caller reports as no reply.
func readCapped(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil
	}
	defer f.Close()
	text, err := io.ReadAll(io.LimitReader(f, maxStdout+1))
	if err != nil {
		return nil, engineFailure(err)
	}
	if len(text) > maxStdout {
		return nil, &Error{Code: "engine_failed", Message: "reply too large"}
	}
	return text, nil
}

func genericAdapter(ctx context.Context, e engine, bin, userPrompt string, o Options, env []string, report func(string)) (Result, error) {
	prompt := systemPrompt + "\n\n" + userPrompt
	argv := append([]string{bin}, e.command[1:]...)
	viaStdin := true
	for i, arg := range argv {
		if strings.Contains(arg, "{prompt}") {
			viaStdin = false
			argv[i] = strings.ReplaceAll(arg, "{prompt}", prompt)
		}
	}
	cmd := childCommand(ctx, argv, o.BaseDir, env)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, engineFailure(err)
	}
	stderr := &tailBuffer{limit: stderrTail}
	cmd.Stderr = stderr
	stdin := ""
	if viaStdin {
		stdin = prompt
	}
	if err := startChild(cmd, stdin); err != nil {
		return Result{}, engineFailure(err)
	}

	var out []byte
	buf := make([]byte, 32<<10)
	for {
		n, readErr := stdout.Read(buf)
		if n > 0 {
			out = append(out, buf[:n]...)
			if len(out) > maxStdout {
				killChild(cmd)
				cmd.Wait()
				return Result{}, &Error{Code: "engine_failed", Message: "reply too large"}
			}
			report(string(buf[:n]))
		}
		if readErr != nil {
			break
		}
	}
	cmd.Wait()
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	if code := exitCode(cmd); code != 0 {
		message := fmt.Sprintf("@%s (%s) exited (%d)", e.name, e.command[0], code)
		if tail := stderr.String(); tail != "" {
			message += ": " + tail
		}
		return Result{}, &Error{Code: "engine_failed", Message: message}
	}
	if strings.TrimSpace(string(out)) == "" {
		return Result{}, &Error{Code: "engine_failed", Message: fmt.Sprintf("@%s produced no reply", e.name)}
	}
	return Result{HTML: stripFences(string(out)), Model: e.name, StopReason: "end_turn"}, nil
}

func childCommand(ctx context.Context, argv []string, dir string, env []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.WaitDelay = childWaitDelay
	prepareProcess(cmd)
	return cmd
}

// startChild feeds stdin through a pipe of our own, so a program that exits
// without reading it cannot turn the write into a process failure.
func startChild(cmd *exec.Cmd, stdin string) error {
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd.Stdin = pr
	go func() {
		io.WriteString(pw, stdin)
		pw.Close()
	}()
	if err := cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		return err
	}
	pr.Close()
	return nil
}

func exitCode(cmd *exec.Cmd) int {
	if cmd.ProcessState == nil {
		return -1
	}
	return cmd.ProcessState.ExitCode()
}

func engineFailure(err error) *Error {
	return &Error{Code: "engine_failed", Message: err.Error()}
}

var (
	fenceStart = regexp.MustCompile("^```[a-zA-Z]*\\s*")
	fenceEnd   = regexp.MustCompile("\\s*```$")
)

// stripFences removes a markdown fence a model wrapped its reply in.
func stripFences(text string) string {
	trimmed := strings.TrimSpace(text)
	trimmed = fenceStart.ReplaceAllString(trimmed, "")
	trimmed = fenceEnd.ReplaceAllString(trimmed, "")
	return strings.TrimSpace(trimmed)
}

type tailBuffer struct {
	mu    sync.Mutex
	data  []byte
	limit int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if len(b.data) > b.limit {
		b.data = b.data[len(b.data)-b.limit:]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.data))
}
