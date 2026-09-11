package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"testing"
	"time"
)

const contractScenarioEnv = "HTMLCLAY_CONTRACT_SCENARIO"

func contractSpec(t *testing.T, scenario string) HelperSpec {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return HelperSpec{
		Argv:       []string{exe, "-test.run=^TestContractFixtureHelperProcess$"},
		Env:        append(os.Environ(), contractScenarioEnv+"="+scenario),
		Deadline:   time.Duration(helperStructuredDeadline) * time.Millisecond,
		Structured: true,
		Stderr:     io.Discard,
	}
}

func contractEvents(ctx context.Context, spec HelperSpec) []HelperEvent {
	var events []HelperEvent
	Run(ctx, spec, func(event HelperEvent) {
		events = append(events, event)
	})
	return events
}

func contractTerminal(t *testing.T, events []HelperEvent) HelperEvent {
	t.Helper()
	if len(events) == 0 {
		t.Fatal("runner emitted no events")
	}
	return events[len(events)-1]
}

func contractErrorPayload(t *testing.T, frame wireFrame) (string, string) {
	t.Helper()
	var payload struct {
		Source string `json:"source"`
		Code   string `json:"code"`
	}
	if err := json.Unmarshal(frame.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	return payload.Source, payload.Code
}

func TestStructuredContractFixtures(t *testing.T) {
	tests := []struct {
		name       string
		scenario   string
		wantKind   string
		wantSource string
		wantCode   string
		wantValue  string
		resultCap  int
		wireType   string
		wireCode   string
	}{
		{
			name:      "success",
			scenario:  "success",
			wantKind:  "result",
			wantValue: `{"answer":42}`,
			resultCap: helperMaxTerminalRecord,
			wireType:  "wire/done",
		},
		{
			name:       "application error",
			scenario:   "application-error",
			wantKind:   "error",
			wantSource: "application",
			wantCode:   "invalid_request",
			resultCap:  helperMaxTerminalRecord,
			wireType:   "wire/error",
			wireCode:   "invalid_request",
		},
		{
			name:       "crash after result",
			scenario:   "crash-after-result",
			wantKind:   "error",
			wantSource: "host",
			wantCode:   "helper_crashed",
			resultCap:  helperMaxTerminalRecord,
			wireType:   "wire/error",
			wireCode:   "helper_crashed",
		},
		{
			name:       "malformed output",
			scenario:   "malformed",
			wantKind:   "error",
			wantSource: "host",
			wantCode:   "helper_bad_output",
			resultCap:  helperMaxTerminalRecord,
			wireType:   "wire/error",
			wireCode:   "helper_bad_output",
		},
		{
			name:      "oversized output",
			scenario:  "oversized-result",
			wantKind:  "result",
			resultCap: helperDescribeResultLimit,
			wireType:  "wire/error",
			wireCode:  "helper_result_too_large",
		},
		{
			name:      "silent result without progress",
			scenario:  "silent",
			wantKind:  "result",
			wantValue: `{"quiet":true}`,
			resultCap: helperMaxTerminalRecord,
			wireType:  "wire/done",
		},
		{
			name:      "null result",
			scenario:  "null",
			wantKind:  "result",
			wantValue: "null",
			resultCap: helperMaxTerminalRecord,
			wireType:  "wire/done",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events := contractEvents(context.Background(), contractSpec(t, tt.scenario))
			terminal := contractTerminal(t, events)
			if terminal.Kind != tt.wantKind || terminal.Source != tt.wantSource || terminal.Code != tt.wantCode {
				t.Fatalf("terminal = %+v, want kind %q source %q code %q; all events: %+v", terminal, tt.wantKind, tt.wantSource, tt.wantCode, events)
			}
			if tt.wantValue != "" && string(terminal.Value) != tt.wantValue {
				t.Fatalf("result = %s, want %s", terminal.Value, tt.wantValue)
			}
			if tt.scenario == "success" {
				if len(events) != 2 || events[0].Kind != "status" || events[0].Text != "Scanning" || string(events[0].Progress) != `{"completed":1,"total":2}` {
					t.Fatalf("success events = %+v, want progress then result", events)
				}
			}

			frame, body, err := encodeHelperEventFrame("contract", "/tmp/page.htmlclay", terminal, tt.resultCap)
			if err != nil {
				t.Fatal(err)
			}
			if frame.Type != tt.wireType {
				t.Fatalf("wire frame = %+v, want type %q", frame, tt.wireType)
			}
			if len(body) > helperMaxWireEnvelope {
				t.Fatalf("encoded frame is %d bytes, limit is %d", len(body), helperMaxWireEnvelope)
			}
			if tt.wireCode != "" {
				_, code := contractErrorPayload(t, frame)
				if code != tt.wireCode {
					t.Fatalf("wire error code = %q, want %q", code, tt.wireCode)
				}
			} else if string(frame.Payload) != tt.wantValue {
				t.Fatalf("wire result = %s, want %s", frame.Payload, tt.wantValue)
			}
		})
	}
}

func TestStructuredContractCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var events []HelperEvent
	Run(ctx, contractSpec(t, "cancellation"), func(event HelperEvent) {
		events = append(events, event)
		if event.Kind == "status" && event.Text == "ready" {
			cancel()
		}
	})
	terminal := contractTerminal(t, events)
	if terminal.Kind != "error" || terminal.Source != "host" || terminal.Code != "helper_cancelled" {
		t.Fatalf("events = %+v, want host cancellation after ready", events)
	}
}

func TestStructuredContractPreservesEditingAndDataRequests(t *testing.T) {
	for _, documentMode := range []string{"edit", "none"} {
		t.Run(documentMode, func(t *testing.T) {
			raw := []byte(fmt.Sprintf(`{"v":1,"type":"wire/request","id":"%s","helper":"search","document":"%s","payload":{"query":"clay"}}`, documentMode, documentMode))
			stamped, err := stampHelperProtocol(raw)
			if err != nil {
				t.Fatal(err)
			}
			var request struct {
				HelperProtocol int             `json:"helperProtocol"`
				Helper         string          `json:"helper"`
				Document       string          `json:"document"`
				Payload        json.RawMessage `json:"payload"`
			}
			if err := json.Unmarshal(stamped, &request); err != nil {
				t.Fatal(err)
			}
			if request.HelperProtocol != 1 || request.Helper != "search" || request.Document != documentMode || string(request.Payload) != `{"query":"clay"}` {
				t.Fatalf("stamped request = %+v, payload = %s", request, request.Payload)
			}
		})
	}
}

func waitForContractText(t *testing.T, output *safeBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(output.String(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in output:\n%s", want, output.String())
}

func stopContractHarness(t *testing.T, harness *wireHarness, done <-chan int) {
	t.Helper()
	harness.cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("wire harness did not stop")
	}
}

func postFrozenPageFrame(t *testing.T, s *site, file, body string) {
	t.Helper()
	f, ok := s.sessions.LookupByPath(file)
	if !ok {
		t.Fatalf("open file %s has no session", file)
	}
	origin := fmt.Sprintf("http://127.0.0.1:%d", s.port)
	req, err := http.NewRequest(http.MethodPost, origin+"/_/wire/send", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Page-URL", fileURL(s.port, f.RelPath))
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Origin", origin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reply, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("frozen page frame was refused: %s: %s", resp.Status, reply)
	}
}

func TestFrozenClientAndRawHandlerContract(t *testing.T) {
	file, site, cfgBase := openTestSite(t)
	t.Setenv(contractScenarioEnv, "raw-handler")

	server := newWireHarness(t, cfgBase)
	serving := server.background("serve", file, "--", os.Args[0], "-test.run=^TestContractFixtureHelperProcess$")
	waitForContractText(t, server.errs, "attached as handler")

	observer := newWireHarness(t, cfgBase)
	observing := observer.background("listen", file)
	waitForContractText(t, observer.errs, "attached as observer")

	postFrozenPageFrame(t, site, file, `{"type":"wire/request","id":"legacy-success","payload":{"task":"edit"}}`)
	waitForContractText(t, observer.out, `"type":"wire/done","id":"legacy-success"`)
	frames := wireFrames(t, observer.out)
	ack, ok := findWireFrame(frames, "wire/ack", "legacy-success")
	if !ok || ack.Payload != nil {
		t.Fatalf("legacy acknowledgement = %+v, want raw acknowledgement without payload", ack)
	}
	status, ok := findWireFrame(frames, "wire/status", "legacy-success")
	if !ok || status.Text != `{"type":"result","value":{"legacy":true}}` {
		t.Fatalf("legacy status = %+v, want JSON-looking stdout left as raw text", status)
	}
	done, ok := findWireFrame(frames, "wire/done", "legacy-success")
	if !ok || done.Payload != nil {
		t.Fatalf("legacy terminal = %+v, want raw done without payload", done)
	}

	postFrozenPageFrame(t, site, file, `{"type":"wire/request","id":"named-raw","helper":"search","document":"none"}`)
	waitForContractText(t, observer.out, `"type":"wire/error","id":"named-raw"`)
	frames = wireFrames(t, observer.out)
	rawError, ok := findWireFrame(frames, "wire/error", "named-raw")
	if !ok {
		t.Fatal("named request against raw handler received no terminal error")
	}
	_, code := contractErrorPayload(t, rawError)
	if code != "helper_protocol_unsupported" {
		t.Fatalf("raw handler mismatch code = %q, want helper_protocol_unsupported", code)
	}
	if _, ok := findWireFrame(frames, "wire/ack", "named-raw"); ok {
		t.Fatal("named request reached the raw handler")
	}

	postFrozenPageFrame(t, site, file, `{"type":"wire/request","id":"legacy-cancel","payload":{"task":"edit"}}`)
	waitForContractText(t, observer.out, `"text":"working"`)
	postFrozenPageFrame(t, site, file, `{"type":"wire/cancel","id":"legacy-cancel"}`)
	waitForContractText(t, observer.out, `"type":"wire/error","id":"legacy-cancel"`)
	frames = wireFrames(t, observer.out)
	cancelled, ok := findWireFrame(frames, "wire/error", "legacy-cancel")
	if !ok || cancelled.Text != "cancelled" || cancelled.Payload != nil {
		t.Fatalf("legacy cancellation = %+v, want raw text error", cancelled)
	}

	stopContractHarness(t, server, serving)
	stopContractHarness(t, observer, observing)
}

func TestContractFixtureHelperProcess(t *testing.T) {
	scenario := os.Getenv(contractScenarioEnv)
	if scenario == "" {
		return
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, wireStopSignals...)
	switch scenario {
	case "success":
		fmt.Fprintln(os.Stdout, `{"type":"status","text":"Scanning","progress":{"completed":1,"total":2}}`)
		fmt.Fprintln(os.Stdout, `{"type":"result","value":{"answer":42}}`)
	case "application-error":
		fmt.Fprintln(os.Stdout, `{"type":"error","code":"invalid_request","message":"Query is empty","details":{"field":"query"}}`)
	case "crash-after-result":
		fmt.Fprintln(os.Stdout, `{"type":"result","value":{"answer":42}}`)
		os.Exit(3)
	case "malformed":
		fmt.Fprintln(os.Stdout, `{not JSON`)
	case "oversized-result":
		json.NewEncoder(os.Stdout).Encode(map[string]any{
			"type":  "result",
			"value": strings.Repeat("x", helperDescribeResultLimit+1),
		})
	case "silent":
		fmt.Fprintln(os.Stdout, `{"type":"result","value":{"quiet":true}}`)
	case "null":
		fmt.Fprintln(os.Stdout, `{"type":"result","value":null}`)
	case "cancellation":
		fmt.Fprintln(os.Stdout, `{"type":"status","text":"ready"}`)
		<-stop
	case "raw-handler":
		input, err := io.ReadAll(os.Stdin)
		if err != nil {
			os.Exit(2)
		}
		var request struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(input, &request); err != nil {
			os.Exit(2)
		}
		if request.ID == "legacy-cancel" {
			fmt.Fprintln(os.Stdout, "working")
			<-stop
			os.Exit(0)
		}
		fmt.Fprintln(os.Stdout, `{"type":"result","value":{"legacy":true}}`)
	}
	os.Exit(0)
}
