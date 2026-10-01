package aiedit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeClaude is a stand-in for the Claude Code CLI: it records its argv and
// stdin, then answers with the stream-json lines the real one prints.
const fakeClaude = `#!/bin/sh
[ -n "$FAKE_ARGV" ] && printf '%s\n' "$@" > "$FAKE_ARGV"
[ -n "$FAKE_STDIN" ] && cat > "$FAKE_STDIN"
printf '%s\n' '{"type":"system","subtype":"init","model":"claude-opus-5-5"}'
printf '%s\n' '{"type":"stream_event","event":{"delta":{"type":"text_delta","text":"Hello "}}}'
printf '%s\n' '{"type":"result","subtype":"success","result":"<p>New</p>","stop_reason":"end_turn"}'
`

const fakeCodex = `#!/bin/sh
[ -n "$FAKE_ARGV" ] && printf '%s\n' "$@" > "$FAKE_ARGV"
[ -n "$FAKE_STDIN" ] && cat > "$FAKE_STDIN"
out=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-o" ]; then out="$arg"; fi
  prev="$arg"
done
printf '%s' '<p>From codex</p>' > "$out"
`

func requireUnix(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake agents are shell scripts")
	}
}

func writeAgent(t *testing.T, dir, name, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
}

// testEnv is this process's environment with PATH led by the fake agents,
// plus whatever extra variables the fake needs.
func testEnv(binDir string, extra ...string) []string {
	env := make([]string, 0, len(os.Environ())+len(extra)+1)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "PATH=") {
			continue
		}
		env = append(env, entry)
	}
	env = append(env, "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return append(env, extra...)
}

func readText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func argvOf(t *testing.T, path string) []string {
	t.Helper()
	return strings.Split(strings.TrimSuffix(readText(t, path), "\n"), "\n")
}

func hasArg(argv []string, want string) bool {
	for _, arg := range argv {
		if arg == want {
			return true
		}
	}
	return false
}

func argAfter(t *testing.T, argv []string, flag string) string {
	t.Helper()
	for i, arg := range argv {
		if arg == flag && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	t.Fatalf("%s missing from argv %q", flag, argv)
	return ""
}

func errorCode(t *testing.T, err error) string {
	t.Helper()
	var failure *Error
	if !errors.As(err, &failure) {
		t.Fatalf("want a coded error, got %v", err)
	}
	return failure.Code
}

func TestClaudeIsTheDefaultEngine(t *testing.T) {
	requireUnix(t)
	binDir := t.TempDir()
	writeAgent(t, binDir, "claude", fakeClaude)
	argvFile := filepath.Join(t.TempDir(), "argv")
	stdinFile := filepath.Join(t.TempDir(), "stdin")

	var streamed strings.Builder
	result, err := Run(context.Background(), Payload{
		Tag:         "p",
		ElementHTML: `<p class="lead">old</p>`,
		Comment:     "tighten",
	}, Options{Env: testEnv(binDir, "FAKE_ARGV="+argvFile, "FAKE_STDIN="+stdinFile)}, func(text string) {
		streamed.WriteString(text)
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.HTML != "<p>New</p>" {
		t.Fatalf("html = %q", result.HTML)
	}
	if result.Model != "claude-opus-5-5" {
		t.Fatalf("model = %q", result.Model)
	}
	if result.StopReason != "end_turn" {
		t.Fatalf("stopReason = %q", result.StopReason)
	}
	if !strings.Contains(streamed.String(), "Hello ") {
		t.Fatalf("progress = %q", streamed.String())
	}
	argv := argvOf(t, argvFile)
	if !hasArg(argv, "--strict-mcp-config") {
		t.Fatalf("argv = %q", argv)
	}
	if got := argAfter(t, argv, "--model"); got != "claude-opus-5-5" {
		t.Fatalf("--model = %q", got)
	}
	if got := argAfter(t, argv, "--tools"); got != "" {
		t.Fatalf("--tools = %q, want an empty argument", got)
	}
	if stdin := readText(t, stdinFile); !strings.Contains(stdin, "Request: tighten") {
		t.Fatalf("stdin = %q", stdin)
	}
}

func TestFableEngine(t *testing.T) {
	requireUnix(t)
	binDir := t.TempDir()
	writeAgent(t, binDir, "claude", fakeClaude)
	argvFile := filepath.Join(t.TempDir(), "argv")
	stdinFile := filepath.Join(t.TempDir(), "stdin")

	_, err := Run(context.Background(), Payload{
		Tag:         "p",
		ElementHTML: `<p class="lead">old</p>`,
		Comment:     "@fable tighten",
	}, Options{Env: testEnv(binDir, "FAKE_ARGV="+argvFile, "FAKE_STDIN="+stdinFile)}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := argAfter(t, argvOf(t, argvFile), "--model"); got != "claude-fable-5-1" {
		t.Fatalf("--model = %q", got)
	}
	stdin := readText(t, stdinFile)
	if !strings.Contains(stdin, "Request: tighten") {
		t.Fatalf("stdin = %q", stdin)
	}
	if strings.Contains(stdin, "@fable") {
		t.Fatalf("stdin still names the engine: %q", stdin)
	}
}

func TestCodexEngine(t *testing.T) {
	requireUnix(t)
	binDir := t.TempDir()
	writeAgent(t, binDir, "codex", fakeCodex)
	argvFile := filepath.Join(t.TempDir(), "argv")
	stdinFile := filepath.Join(t.TempDir(), "stdin")

	result, err := Run(context.Background(), Payload{
		Tag:         "p",
		ElementHTML: `<p class="lead">old</p>`,
		Comment:     "@codex tighten",
	}, Options{Env: testEnv(binDir, "FAKE_ARGV="+argvFile, "FAKE_STDIN="+stdinFile)}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.HTML != "<p>From codex</p>" {
		t.Fatalf("html = %q", result.HTML)
	}
	argv := argvOf(t, argvFile)
	if !hasArg(argv, "--disable") || !hasArg(argv, "shell_tool") {
		t.Fatalf("argv = %q", argv)
	}
	if argv[len(argv)-1] != "-" {
		t.Fatalf("last argument = %q", argv[len(argv)-1])
	}
	if stdin := readText(t, stdinFile); !strings.HasPrefix(stdin, systemPrompt) {
		t.Fatalf("stdin does not start with the system prompt: %q", stdin)
	}
}

func TestAgyIsRefused(t *testing.T) {
	requireUnix(t)
	binDir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "marker")
	writeAgent(t, binDir, "agy", "#!/bin/sh\ntouch \"$FAKE_MARKER\"\n")

	_, err := Run(context.Background(), Payload{
		Tag:         "p",
		ElementHTML: "<p>old</p>",
		Comment:     "@agy x",
	}, Options{Env: testEnv(binDir, "FAKE_MARKER="+marker)}, nil)
	if code := errorCode(t, err); code != "engine_unsupported" {
		t.Fatalf("code = %q", code)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("the agy agent was started")
	}
}

func TestUnknownEngine(t *testing.T) {
	_, err := Run(context.Background(), Payload{
		Tag:         "p",
		ElementHTML: "<p>old</p>",
		Comment:     "@nope x",
	}, Options{Env: []string{"PATH=" + t.TempDir()}}, nil)
	if code := errorCode(t, err); code != "unknown_engine" {
		t.Fatalf("code = %q", code)
	}
	if !strings.Contains(err.Error(), "@claude @fable @codex @agy") {
		t.Fatalf("message = %q", err.Error())
	}
}

func TestMissingBinary(t *testing.T) {
	_, err := Run(context.Background(), Payload{
		Tag:         "p",
		ElementHTML: "<p>old</p>",
		Comment:     "tighten",
	}, Options{Env: []string{"PATH=" + t.TempDir()}}, nil)
	if code := errorCode(t, err); code != "engine_unavailable" {
		t.Fatalf("code = %q", code)
	}
	if !strings.HasPrefix(err.Error(), "@claude isn't available") {
		t.Fatalf("message = %q", err.Error())
	}
}

func TestContextReferences(t *testing.T) {
	requireUnix(t)
	binDir := t.TempDir()
	writeAgent(t, binDir, "claude", fakeClaude)
	stdinFile := filepath.Join(t.TempDir(), "stdin")
	argvFile := filepath.Join(t.TempDir(), "argv")

	root := t.TempDir()
	baseDir := filepath.Join(root, "doc")
	if err := os.Mkdir(baseDir, 0755); err != nil {
		t.Fatal(err)
	}
	run := func(t *testing.T, ref string) (Result, error) {
		t.Helper()
		if err := os.Remove(stdinFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		return Run(context.Background(), Payload{
			Tag:         "p",
			ElementHTML: "<p>old</p>",
			Comment:     "tighten",
			ContextRefs: []string{ref},
		}, Options{
			BaseDir: baseDir,
			Env:     testEnv(binDir, "FAKE_ARGV="+argvFile, "FAKE_STDIN="+stdinFile),
		}, nil)
	}

	t.Run("escape", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(root, "outside.md"), []byte("out"), 0644); err != nil {
			t.Fatal(err)
		}
		_, err := run(t, "../outside.md")
		if code := errorCode(t, err); code != "invalid_context" {
			t.Fatalf("code = %q", code)
		}
		if !strings.Contains(err.Error(), "escapes") {
			t.Fatalf("message = %q", err.Error())
		}
	})

	t.Run("symlink", func(t *testing.T) {
		target := filepath.Join(root, "secret.md")
		if err := os.WriteFile(target, []byte("secret"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(baseDir, "link.md")); err != nil {
			t.Fatal(err)
		}
		_, err := run(t, "link.md")
		if code := errorCode(t, err); code != "invalid_context" {
			t.Fatalf("code = %q", code)
		}
	})

	t.Run("too large", func(t *testing.T) {
		big := strings.Repeat("x", 300<<10)
		if err := os.WriteFile(filepath.Join(baseDir, "big.txt"), []byte(big), 0644); err != nil {
			t.Fatal(err)
		}
		_, err := run(t, "big.txt")
		if code := errorCode(t, err); code != "invalid_context" {
			t.Fatalf("code = %q", code)
		}
		if !strings.Contains(err.Error(), "256 KB") {
			t.Fatalf("message = %q", err.Error())
		}
	})

	t.Run("read into the prompt", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(baseDir, "notes.md"), []byte("hello notes"), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := run(t, "notes.md"); err != nil {
			t.Fatalf("Run: %v", err)
		}
		stdin := readText(t, stdinFile)
		if !strings.Contains(stdin, "Context file @notes.md:") {
			t.Fatalf("stdin = %q", stdin)
		}
		if !strings.Contains(stdin, "hello notes") {
			t.Fatalf("stdin = %q", stdin)
		}
	})
}

func TestGenericEngines(t *testing.T) {
	requireUnix(t)
	element := "<p>old</p>"
	payload := Payload{Tag: "p", ElementHTML: element, Comment: "@echo hi"}

	echoed, err := Run(context.Background(), payload, Options{
		Engines: map[string][]string{"echo": {"sh", "-c", "cat"}},
		Env:     testEnv(t.TempDir()),
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(echoed.HTML, "The element to edit:") || !strings.Contains(echoed.HTML, "Request: hi") {
		t.Fatalf("html = %q", echoed.HTML)
	}

	payload.Comment = "@arg hi"
	substituted, err := Run(context.Background(), payload, Options{
		Engines: map[string][]string{"arg": {"printf", "%s", "{prompt}"}},
		Env:     testEnv(t.TempDir()),
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(substituted.HTML, "Request: hi") {
		t.Fatalf("html = %q", substituted.HTML)
	}
	if strings.Contains(substituted.HTML, "{prompt}") {
		t.Fatalf("the placeholder reached the agent: %q", substituted.HTML)
	}
}

func TestStopReasons(t *testing.T) {
	requireUnix(t)
	cases := map[string]string{
		"refusal":    "declined",
		"max_tokens": "incomplete",
	}
	for stop, want := range cases {
		t.Run(stop, func(t *testing.T) {
			binDir := t.TempDir()
			writeAgent(t, binDir, "claude", "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"result\",\"subtype\":\"success\",\"result\":\"<p>x</p>\",\"stop_reason\":\""+stop+"\"}'\n")
			_, err := Run(context.Background(), Payload{
				Tag:         "p",
				ElementHTML: "<p>old</p>",
				Comment:     "tighten",
			}, Options{Env: testEnv(binDir)}, nil)
			if code := errorCode(t, err); code != want {
				t.Fatalf("code = %q, want %q", code, want)
			}
		})
	}
}

func TestCancelKillsTheAgent(t *testing.T) {
	requireUnix(t)
	binDir := t.TempDir()
	writeAgent(t, binDir, "claude", "#!/bin/sh\nsleep 30\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	started := time.Now()
	_, err := Run(ctx, Payload{
		Tag:         "p",
		ElementHTML: "<p>old</p>",
		Comment:     "tighten",
	}, Options{Env: testEnv(binDir)}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("Run took %s", elapsed)
	}
}

func TestMock(t *testing.T) {
	pieces := 0
	var streamed strings.Builder
	result, err := Run(context.Background(), Payload{
		Tag:         "div",
		ElementHTML: `<div class="hero" data-id="1"><p>Hello there</p></div>`,
		Comment:     "make it [mock:end_turn]",
	}, Options{Mock: true}, func(text string) {
		pieces++
		streamed.WriteString(text)
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(result.HTML, "mock edit: make it") {
		t.Fatalf("html = %q", result.HTML)
	}
	if result.Model != "mock(claude-opus-5-5)" {
		t.Fatalf("model = %q", result.Model)
	}
	if result.StopReason != "end_turn" {
		t.Fatalf("stopReason = %q", result.StopReason)
	}
	if pieces < 2 {
		t.Fatalf("progress called %d times", pieces)
	}
	if streamed.String() != result.HTML {
		t.Fatalf("progress streamed %q", streamed.String())
	}
}

func TestMalformedRequest(t *testing.T) {
	cases := map[string]Payload{
		"comment": {Tag: "p", ElementHTML: "<p>old</p>"},
		"element": {Tag: "p", Comment: "tighten"},
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Run(context.Background(), payload, Options{Mock: true}, nil)
			if code := errorCode(t, err); code != "invalid_request" {
				t.Fatalf("code = %q", code)
			}
		})
	}
}

func TestIsDocument(t *testing.T) {
	for _, path := range []string{"a.html", "B.HTMLCLAY", "/tmp/x/y.Html"} {
		if !IsDocument(path) {
			t.Fatalf("%s is a document", path)
		}
	}
	for _, path := range []string{"a.htm", "a.txt", "html", ""} {
		if IsDocument(path) {
			t.Fatalf("%s is not a document", path)
		}
	}
}
