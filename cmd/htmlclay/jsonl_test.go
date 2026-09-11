package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The record grammar, field validation and limits are pinned in
// internal/helper, which owns the parser both the CLI and the dispatcher run.
// What remains here covers the CLI's own transport: frame encoding and the
// status queue.

const structuredRunnerTestMode = "HTMLCLAY_STRUCTURED_RUNNER_TEST_MODE"

func TestEncodeFrameKeepsA512KiBAngleBracketResultUnderTheWireLimit(t *testing.T) {
	value := make([]byte, 0, (512<<10)+2)
	value = append(value, '"')
	value = append(value, bytes.Repeat([]byte("<"), 512<<10)...)
	value = append(value, '"')
	_, body, err := encodeHelperEventFrame("request", "/tmp/page.htmlclay", HelperEvent{
		Kind:  "result",
		Value: json.RawMessage(value),
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) >= helperMaxWireEnvelope {
		t.Fatalf("encoded envelope is %d bytes, want less than %d", len(body), helperMaxWireEnvelope)
	}
	if bytes.Contains(body, []byte(`\u003c`)) {
		t.Fatal("encoded envelope HTML-escaped the result")
	}

	var delivered []byte
	fakeTransport := func(frame []byte) { delivered = append(delivered, frame...) }
	fakeTransport(body)
	var frame wireFrame
	if err := json.Unmarshal(delivered, &frame); err != nil {
		t.Fatal(err)
	}
	var result string
	if err := json.Unmarshal(frame.Payload, &result); err != nil {
		t.Fatal(err)
	}
	if len(result) != 512<<10 || strings.Trim(result, "<") != "" {
		t.Fatalf("fake transport delivered %d result bytes, want %d literal angle brackets", len(result), 512<<10)
	}
}

func TestEncodeHelperEventFrameMappings(t *testing.T) {
	status, _, err := encodeHelperEventFrame("id", "/tmp/page.htmlclay", HelperEvent{
		Kind:     "status",
		Text:     "Scanning",
		Progress: json.RawMessage(`{"completed":2,"total":4}`),
	}, helperMaxTerminalRecord)
	if err != nil {
		t.Fatal(err)
	}
	var statusPayload struct {
		Progress struct {
			Completed int `json:"completed"`
			Total     int `json:"total"`
		} `json:"progress"`
	}
	if err := json.Unmarshal(status.Payload, &statusPayload); err != nil {
		t.Fatal(err)
	}
	if status.Type != "wire/status" || status.Text != "Scanning" || statusPayload.Progress.Completed != 2 || statusPayload.Progress.Total != 4 {
		t.Fatalf("status frame = %+v, payload = %+v", status, statusPayload)
	}

	application, _, err := encodeHelperEventFrame("id", "/tmp/page.htmlclay", HelperEvent{
		Kind:    "error",
		Text:    "bad query",
		Code:    "invalid_request",
		Details: json.RawMessage(`{"field":"query"}`),
		Source:  "application",
	}, helperMaxTerminalRecord)
	if err != nil {
		t.Fatal(err)
	}
	var errorPayload struct {
		Source  string          `json:"source"`
		Code    string          `json:"code"`
		Details json.RawMessage `json:"details"`
	}
	if err := json.Unmarshal(application.Payload, &errorPayload); err != nil {
		t.Fatal(err)
	}
	if application.Type != "wire/error" || errorPayload.Source != "application" || errorPayload.Code != "invalid_request" || string(errorPayload.Details) != `{"field":"query"}` {
		t.Fatalf("application error frame = %+v, payload = %+v", application, errorPayload)
	}

	tooLarge, _, err := encodeHelperEventFrame("id", "/tmp/page.htmlclay", HelperEvent{
		Kind:  "result",
		Value: json.RawMessage(strings.Repeat("0", helperDescribeResultLimit+1)),
	}, helperDescribeResultLimit)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(tooLarge.Payload, &errorPayload); err != nil {
		t.Fatal(err)
	}
	if tooLarge.Type != "wire/error" || errorPayload.Source != "host" || errorPayload.Code != "helper_result_too_large" {
		t.Fatalf("oversized result frame = %+v, payload = %+v", tooLarge, errorPayload)
	}
}

func TestStructuredEventQueueParsesPastBlockedProgressAndRetainsTerminal(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var delivered []HelperEvent
	queue := newStructuredEventQueue(func(event HelperEvent) {
		if len(delivered) == 0 {
			close(started)
			<-release
		}
		delivered = append(delivered, event)
	})
	queue.emit(HelperEvent{Kind: "status", Text: "first"})
	<-started

	for i := 0; i < 100; i++ {
		queue.emit(HelperEvent{Kind: "status", Text: fmt.Sprintf("status %d", i)})
	}
	terminalDone := make(chan struct{})
	go func() {
		queue.emit(HelperEvent{Kind: "result", Value: json.RawMessage("null")})
		close(terminalDone)
	}()
	select {
	case <-terminalDone:
		t.Fatal("terminal was reported delivered while the transport was blocked")
	default:
	}
	close(release)
	select {
	case <-terminalDone:
	case <-time.After(time.Second):
		t.Fatal("terminal was not retained after the transport resumed")
	}
	if got := delivered[len(delivered)-1]; got.Kind != "result" {
		t.Fatalf("last delivered event = %+v, want terminal result", got)
	}
	if len(delivered) >= 101 {
		t.Fatalf("delivered %d events, want progress to be coalesced", len(delivered))
	}
}

func structuredRunnerCommand(t *testing.T, mode string) HelperSpec {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return HelperSpec{
		Argv:       []string{exe, "-test.run=^TestStructuredRunnerHelperProcess$"},
		Env:        append(os.Environ(), structuredRunnerTestMode+"="+mode),
		Structured: true,
		Stderr:     io.Discard,
	}
}

func requireStructuredTerminal(t *testing.T, events []HelperEvent, code string) HelperEvent {
	t.Helper()
	if len(events) == 0 {
		t.Fatal("runner emitted no events")
	}
	terminal := events[len(events)-1]
	if terminal.Kind != "error" || terminal.Code != code || terminal.Source != "host" {
		t.Fatalf("terminal = %+v, want host error %q; all events: %+v", terminal, code, events)
	}
	return terminal
}

func TestRunStructuredStopsAChildAfterAnOversizedRecord(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "stopped")
	spec := structuredRunnerCommand(t, "oversized")
	spec.Env = append(spec.Env, "HTMLCLAY_STRUCTURED_STOPPED="+marker)
	events := runnerEvents(context.Background(), spec)
	requireStructuredTerminal(t, events, "helper_bad_output")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("child did not record the cleanup signal: %v", err)
	}
}

func TestRunStructuredKeepsProtocolFailureAcrossCleanupCancellation(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "stopped")
	spec := structuredRunnerCommand(t, "invalid")
	spec.Env = append(spec.Env, "HTMLCLAY_STRUCTURED_STOPPED="+marker)
	events := runnerEvents(context.Background(), spec)
	terminal := requireStructuredTerminal(t, events, "helper_bad_output")
	if terminal.Code == "helper_cancelled" {
		t.Fatalf("cleanup replaced the protocol failure: %+v", terminal)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("child did not record the cleanup signal: %v", err)
	}
}

func TestRunStructuredCompletionState(t *testing.T) {
	tests := []struct {
		name     string
		mode     string
		wantKind string
		wantCode string
	}{
		{name: "explicit null result", mode: "null", wantKind: "result"},
		{name: "application error", mode: "application-error", wantKind: "error", wantCode: "invalid_request"},
		{name: "crash after result", mode: "crash-after-result", wantKind: "error", wantCode: "helper_crashed"},
		{name: "zero exit without result", mode: "no-result", wantKind: "error", wantCode: "helper_no_result"},
		{name: "status after terminal", mode: "status-after-result", wantKind: "error", wantCode: "helper_bad_output"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events := runnerEvents(context.Background(), structuredRunnerCommand(t, tt.mode))
			if len(events) == 0 {
				t.Fatal("runner emitted no events")
			}
			terminal := events[len(events)-1]
			if terminal.Kind != tt.wantKind || terminal.Code != tt.wantCode {
				t.Fatalf("terminal = %+v, want kind %q code %q", terminal, tt.wantKind, tt.wantCode)
			}
			if tt.mode == "null" && string(terminal.Value) != "null" {
				t.Fatalf("null result = %q", terminal.Value)
			}
			if tt.mode == "application-error" && terminal.Source != "application" {
				t.Fatalf("application error source = %q", terminal.Source)
			}
		})
	}
}

func TestRunStructuredUsesDocumentDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "relative.txt"), []byte("document directory"), 0644); err != nil {
		t.Fatal(err)
	}
	spec := structuredRunnerCommand(t, "cwd")
	spec.Dir = dir
	events := runnerEvents(context.Background(), spec)
	if len(events) != 1 || events[0].Kind != "result" || string(events[0].Value) != `"document directory"` {
		t.Fatalf("events = %+v, want result read from document directory", events)
	}
}

func TestRunStructuredExternalCancellationBeatsAHeldResult(t *testing.T) {
	started := filepath.Join(t.TempDir(), "started")
	spec := structuredRunnerCommand(t, "held-result")
	spec.Env = append(spec.Env, "HTMLCLAY_STRUCTURED_STARTED="+started)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan []HelperEvent, 1)
	go func() { done <- runnerEvents(ctx, spec) }()
	waitForFile(t, started)
	cancel()
	select {
	case events := <-done:
		requireStructuredTerminal(t, events, "helper_cancelled")
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled structured runner did not stop")
	}
}

func TestRunStructuredDeadlineHasItsOwnHostCode(t *testing.T) {
	spec := structuredRunnerCommand(t, "wait")
	spec.Deadline = 50 * time.Millisecond
	events := runnerEvents(context.Background(), spec)
	requireStructuredTerminal(t, events, "helper_timeout")
}

func TestWireServeStructuredProtocolAndDescribe(t *testing.T) {
	file, _, cfgBase := openTestSite(t)
	if err := os.WriteFile(filepath.Join(filepath.Dir(file), "structured-relative.txt"), []byte("from document cwd"), 0644); err != nil {
		t.Fatal(err)
	}
	inputPath := filepath.Join(t.TempDir(), "input.json")
	t.Setenv("HTMLCLAY_JSONL_HELPER", "1")
	t.Setenv("HTMLCLAY_JSONL_INPUT", inputPath)

	server := newWireHarness(t, cfgBase)
	serving := server.background(append([]string{"serve", file, "--protocol=jsonl", "--"}, wireJSONLHelperCommand()...)...)
	waitForText(t, server.errs, "attached as handler")

	watcher := newWireHarness(t, cfgBase)
	watching := watcher.background("listen", file)
	waitForText(t, watcher.errs, "attached as observer")

	sender := newWireHarness(t, cfgBase)
	if code := sender.run("send", file, "--type", "wire/request", "--id", "structured-request"); code != wireExitOK {
		t.Fatalf("structured request exited %d; stderr:\n%s", code, sender.errs.String())
	}
	waitForText(t, watcher.out, `"cwdFile":"from document cwd"`)

	frames := wireFrames(t, watcher.out)
	ack, ok := findWireFrame(frames, "wire/ack", "structured-request")
	if !ok {
		t.Fatal("structured request received no acknowledgement")
	}
	var ackPayload struct {
		Mode     string `json:"mode"`
		BudgetMS int    `json:"budgetMs"`
	}
	if err := json.Unmarshal(ack.Payload, &ackPayload); err != nil {
		t.Fatal(err)
	}
	if ackPayload.Mode != "jsonl" || ackPayload.BudgetMS != helperStructuredDeadline {
		t.Fatalf("ack payload = %+v", ackPayload)
	}
	done, ok := findWireFrame(frames, "wire/done", "structured-request")
	if !ok {
		t.Fatal("structured request received no result")
	}
	var result struct {
		CWDFile        string `json:"cwdFile"`
		HelperProtocol int    `json:"helperProtocol"`
	}
	if err := json.Unmarshal(done.Payload, &result); err != nil {
		t.Fatal(err)
	}
	if result.CWDFile != "from document cwd" || result.HelperProtocol != helperProtocolVersion {
		t.Fatalf("result = %+v", result)
	}
	input, err := os.ReadFile(inputPath)
	if err != nil {
		t.Fatal(err)
	}
	var stamped map[string]json.RawMessage
	if err := json.Unmarshal(input, &stamped); err != nil {
		t.Fatal(err)
	}
	if string(stamped["helperProtocol"]) != "1" {
		t.Fatalf("helperProtocol in child stdin = %s", stamped["helperProtocol"])
	}

	if err := os.Remove(filepath.Join(filepath.Dir(file), "structured-relative.txt")); err != nil {
		t.Fatal(err)
	}
	describer := newWireHarness(t, cfgBase)
	if code := describer.run("send", file, "--type", "wire/describe", "--id", "structured-describe"); code != wireExitOK {
		t.Fatalf("describe exited %d; stderr:\n%s", code, describer.errs.String())
	}
	waitForText(t, watcher.out, `"contract":"test-helper/1"`)
	frames = wireFrames(t, watcher.out)
	describeAck, ok := findWireFrame(frames, "wire/ack", "structured-describe")
	if !ok {
		t.Fatal("describe received no acknowledgement")
	}
	if err := json.Unmarshal(describeAck.Payload, &ackPayload); err != nil {
		t.Fatal(err)
	}
	if ackPayload.BudgetMS != helperDescribeDeadline {
		t.Fatalf("describe budget = %d, want %d", ackPayload.BudgetMS, helperDescribeDeadline)
	}

	server.cancel()
	watcher.cancel()
	waitForExit(t, serving)
	waitForExit(t, watching)
}

func TestWireServeRequiresJSONLFlagForStructuredBehavior(t *testing.T) {
	file, _, cfgBase := openTestSite(t)
	t.Setenv("HTMLCLAY_JSONL_HELPER", "1")
	t.Setenv("HTMLCLAY_JSONL_RAW", "1")

	server := newWireHarness(t, cfgBase)
	serving := server.background(append([]string{"serve", file, "--"}, wireJSONLHelperCommand()...)...)
	waitForText(t, server.errs, "attached as handler")

	watcher := newWireHarness(t, cfgBase)
	watching := watcher.background("listen", file)
	waitForText(t, watcher.errs, "attached as observer")

	sender := newWireHarness(t, cfgBase)
	if code := sender.run("send", file, "--type", "wire/request", "--id", "raw-json-line"); code != wireExitOK {
		t.Fatalf("raw request exited %d; stderr:\n%s", code, sender.errs.String())
	}
	waitForText(t, watcher.out, `"wire/done"`)
	frames := wireFrames(t, watcher.out)
	status, ok := findWireFrame(frames, "wire/status", "raw-json-line")
	if !ok || status.Text != `{"type":"result","value":{"structured":true}}` {
		t.Fatalf("raw status = %+v, want the JSON record as plain text", status)
	}
	done, ok := findWireFrame(frames, "wire/done", "raw-json-line")
	if !ok || done.Payload != nil {
		t.Fatalf("raw terminal = %+v, want done with no result payload", done)
	}
	ack, ok := findWireFrame(frames, "wire/ack", "raw-json-line")
	if !ok || ack.Payload != nil {
		t.Fatalf("raw acknowledgement = %+v, want no structured payload", ack)
	}

	server.cancel()
	watcher.cancel()
	waitForExit(t, serving)
	waitForExit(t, watching)
}

func TestWireServeProtocolFlagAndSubscriptionMode(t *testing.T) {
	if url := wireSubscribeURL(4321, "/tmp/page.htmlclay", true, "jsonl"); !strings.Contains(url, "mode=jsonl") {
		t.Fatalf("structured handler URL = %q, want mode=jsonl", url)
	}
	if url := wireSubscribeURL(4321, "/tmp/page.htmlclay", false, "jsonl"); strings.Contains(url, "mode=") {
		t.Fatalf("observer URL = %q, want no helper mode", url)
	}

	h := newWireHarness(t, t.TempDir())
	if code := h.run("serve", "--protocol=yaml", "/tmp/page.htmlclay", "--", "helper"); code != wireExitUsage {
		t.Fatalf("unsupported protocol exited %d, want %d", code, wireExitUsage)
	}
}

func findWireFrame(frames []wireFrame, typ, id string) (wireFrame, bool) {
	for _, frame := range frames {
		if frame.Type == typ && frame.ID == id {
			return frame, true
		}
	}
	return wireFrame{}, false
}

func wireJSONLHelperCommand() []string {
	return []string{os.Args[0], "-test.run=^TestWireJSONLHelperProcess$"}
}

func TestWireJSONLHelperProcess(t *testing.T) {
	if os.Getenv("HTMLCLAY_JSONL_HELPER") != "1" {
		return
	}
	if os.Getenv("HTMLCLAY_JSONL_RAW") == "1" {
		fmt.Fprintln(os.Stdout, `{"type":"result","value":{"structured":true}}`)
		os.Exit(0)
	}
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(2)
	}
	if path := os.Getenv("HTMLCLAY_JSONL_INPUT"); path != "" {
		if err := os.WriteFile(path, input, 0644); err != nil {
			os.Exit(2)
		}
	}
	var request struct {
		Type           string `json:"type"`
		HelperProtocol int    `json:"helperProtocol"`
	}
	if err := json.Unmarshal(input, &request); err != nil {
		os.Exit(2)
	}
	if request.Type == "wire/describe" {
		json.NewEncoder(os.Stdout).Encode(map[string]any{
			"type": "result",
			"value": map[string]any{
				"helperProtocol": 1,
				"contract":       "test-helper/1",
				"operations":     []string{"test"},
			},
		})
		os.Exit(0)
	}
	data, err := os.ReadFile("structured-relative.txt")
	if err != nil {
		os.Exit(2)
	}
	json.NewEncoder(os.Stdout).Encode(map[string]any{
		"type":     "status",
		"text":     "working",
		"progress": map[string]any{"completed": 1, "total": 1, "unit": "steps"},
	})
	json.NewEncoder(os.Stdout).Encode(map[string]any{
		"type": "result",
		"value": map[string]any{
			"cwdFile":        string(data),
			"helperProtocol": request.HelperProtocol,
		},
	})
	os.Exit(0)
}

func TestStructuredRunnerHelperProcess(t *testing.T) {
	mode := os.Getenv(structuredRunnerTestMode)
	if mode == "" {
		return
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, wireStopSignals...)
	switch mode {
	case "oversized":
		go func() {
			prefix := `{"type":"status","text":"ok","padding":"`
			suffix := `"}`
			line := prefix + strings.Repeat("x", helperMaxRecord+1-len(prefix)-len(suffix)) + suffix
			fmt.Fprintln(os.Stdout, line)
		}()
		<-stop
		writeStructuredStopMarker()
	case "invalid":
		fmt.Fprintln(os.Stdout, "{not JSON")
		<-stop
		writeStructuredStopMarker()
	case "null":
		fmt.Fprintln(os.Stdout, `{"type":"result","value":null}`)
	case "application-error":
		fmt.Fprintln(os.Stdout, `{"type":"error","code":"invalid_request","message":"bad request","details":{"field":"query"}}`)
	case "crash-after-result":
		fmt.Fprintln(os.Stdout, `{"type":"result","value":true}`)
		os.Exit(3)
	case "no-result":
	case "status-after-result":
		fmt.Fprintln(os.Stdout, `{"type":"result","value":true}`)
		fmt.Fprintln(os.Stdout, `{"type":"status","text":"late"}`)
	case "cwd":
		data, err := os.ReadFile("relative.txt")
		if err != nil {
			os.Exit(2)
		}
		json.NewEncoder(os.Stdout).Encode(map[string]any{"type": "result", "value": string(data)})
	case "held-result":
		fmt.Fprintln(os.Stdout, `{"type":"result","value":true}`)
		os.WriteFile(os.Getenv("HTMLCLAY_STRUCTURED_STARTED"), []byte("started"), 0644)
		<-stop
	case "wait":
		<-stop
	}
	os.Exit(0)
}

func writeStructuredStopMarker() {
	if path := os.Getenv("HTMLCLAY_STRUCTURED_STOPPED"); path != "" {
		os.WriteFile(path, []byte("stopped"), 0644)
	}
}
