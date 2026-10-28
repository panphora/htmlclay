package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/panphora/htmlclay/internal/config"
)

func writeHelperDocument(t *testing.T, path string, names ...string) {
	t.Helper()
	var metas strings.Builder
	for _, name := range names {
		fmt.Fprintf(&metas, "<meta name=\"htmlclay-helper\" content=\"%s\">", name)
	}
	if err := os.WriteFile(path, []byte("<!doctype html><head>"+metas.String()+"</head><body></body>"), 0644); err != nil {
		t.Fatal(err)
	}
}

func helperObserver(t *testing.T, srv *Server, file string) *wireSub {
	t.Helper()
	sub := &wireSub{key: file, ch: make(chan []byte, 64), done: make(chan struct{})}
	if _, _, err := srv.wire.add(sub, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.wire.remove(sub) })
	return sub
}

func receiveHelperEnvelope(t *testing.T, sub *wireSub) wireEnvelope {
	t.Helper()
	select {
	case raw := <-sub.ch:
		env, err := decodeWireEnvelope(raw)
		if err != nil {
			t.Fatal(err)
		}
		return env
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for helper frame")
		return wireEnvelope{}
	}
}

func receiveHelperType(t *testing.T, sub *wireSub, typ, id string) wireEnvelope {
	t.Helper()
	for i := 0; i < 8; i++ {
		env := receiveHelperEnvelope(t, sub)
		if env.Type == typ && env.ID == id {
			return env
		}
	}
	t.Fatalf("did not receive %s for %s", typ, id)
	return wireEnvelope{}
}

func helperErrorCode(t *testing.T, env wireEnvelope) string {
	t.Helper()
	var payload struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	return payload.Code
}

func helperProgram(name, path string) config.HelperProgram {
	return config.HelperProgram{ID: "program-" + name, Name: name, Path: path}
}

func TestAttachHelpersDispatchesAndReusesCompletedID(t *testing.T) {
	srv, file := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })
	writeHelperDocument(t, file.AbsPath, "search", "ocr")
	t.Setenv(serverHelperArg, filepath.Join(t.TempDir(), "count"))
	program := helperProgram("search", structuredHelper(t, "count"))
	allowed := map[string]config.HelperProgram{"search": program}
	denied := []string{"ocr"}
	if err := srv.AttachHelpers(file.AbsPath, allowed, denied); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.DetachHelpers(file.AbsPath) })

	srv.helperMu.Lock()
	first := srv.helpers[file.AbsPath]
	srv.helperMu.Unlock()
	generation := srv.HelperGeneration(file.AbsPath)
	if err := srv.AttachHelpers(file.AbsPath, allowed, denied); err != nil {
		t.Fatal(err)
	}
	srv.helperMu.Lock()
	second := srv.helpers[file.AbsPath]
	srv.helperMu.Unlock()
	if first != second || srv.HelperGeneration(file.AbsPath) != generation {
		t.Fatal("idempotent attach replaced the dispatcher or advanced its generation")
	}

	mode, helpers := srv.helperDiscovery(file.AbsPath)
	if mode != "jsonl" {
		t.Fatalf("mode = %q, want jsonl", mode)
	}
	if len(helpers) != 2 || helpers[0] != (helperMeta{Name: "search", State: "ready"}) || helpers[1] != (helperMeta{Name: "ocr", State: "denied"}) {
		t.Fatalf("helper discovery = %+v", helpers)
	}
	meta := metaOf(t, srv, file)
	document, ok := meta["document"].(map[string]any)
	if !ok || document["wireMode"] != "jsonl" || document["wireBudgetMs"] != float64(300000) {
		t.Fatalf("wire discovery = %+v", document)
	}
	if listed, ok := document["helpers"].([]any); !ok || len(listed) != 2 {
		t.Fatalf("serialized helper discovery = %+v", document["helpers"])
	}

	observer := helperObserver(t, srv, file.AbsPath)
	for want := 1; want <= 2; want++ {
		srv.wire.publish(file.AbsPath, wireEnvelope{
			V: 1, Type: "wire/request", ID: "reusable", File: file.AbsPath,
			Helper: "search", Document: "none", Payload: json.RawMessage(`{"query":"clay"}`),
		})
		ack := receiveHelperType(t, observer, "wire/ack", "reusable")
		var accepted struct {
			Mode     string `json:"mode"`
			BudgetMS int    `json:"budgetMs"`
		}
		if err := json.Unmarshal(ack.Payload, &accepted); err != nil {
			t.Fatal(err)
		}
		if accepted.Mode != "jsonl" || accepted.BudgetMS != 300000 {
			t.Fatalf("ack = %+v", accepted)
		}
		done := receiveHelperType(t, observer, "wire/done", "reusable")
		var result struct {
			Count int    `json:"count"`
			Cwd   string `json:"cwd"`
		}
		if err := json.Unmarshal(done.Payload, &result); err != nil {
			t.Fatal(err)
		}
		if result.Count != want || result.Cwd != filepath.Dir(file.AbsPath) {
			t.Fatalf("result = %+v, want count %d and cwd %s", result, want, filepath.Dir(file.AbsPath))
		}
		waitFor(t, time.Second, "the completed request to leave the live set", func() bool {
			first.mu.Lock()
			defer first.mu.Unlock()
			_, live := first.live["reusable"]
			return !live
		})
	}

	srv.wire.mu.Lock()
	retained := append([]byte(nil), srv.wire.chans[file.AbsPath].terminal["reusable"].frame...)
	srv.wire.mu.Unlock()
	env, err := decodeWireEnvelope(retained)
	if err != nil {
		t.Fatal(err)
	}
	var retainedResult struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(env.Payload, &retainedResult); err != nil {
		t.Fatal(err)
	}
	if retainedResult.Count != 2 {
		t.Fatalf("retained result count = %d, want the reused request's result", retainedResult.Count)
	}
}

func TestDispatcherRefusesNamesWithoutPrompting(t *testing.T) {
	srv, file := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })
	writeHelperDocument(t, file.AbsPath, "search", "ocr")
	program := helperProgram("search", structuredHelper(t, "result-null"))
	if err := srv.AttachHelpers(file.AbsPath, map[string]config.HelperProgram{"search": program}, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.DetachHelpers(file.AbsPath) })
	observer := helperObserver(t, srv, file.AbsPath)

	cases := []struct {
		id     string
		helper string
		code   string
	}{
		{"missing", "", "helper_name_required"},
		{"denied", "ocr", "helper_not_granted"},
		{"undeclared", "other", "helper_not_declared"},
	}
	for _, tc := range cases {
		srv.wire.publish(file.AbsPath, wireEnvelope{V: 1, Type: "wire/request", ID: tc.id, File: file.AbsPath, Helper: tc.helper})
		env := receiveHelperType(t, observer, "wire/error", tc.id)
		if code := helperErrorCode(t, env); code != tc.code {
			t.Fatalf("%s code = %q, want %q", tc.id, code, tc.code)
		}
	}
}

func TestExternalRawHandlerStaysAttachedAndRejectsNamedCalls(t *testing.T) {
	srv, file := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })
	writeHelperDocument(t, file.AbsPath, "search")
	handler := &wireSub{key: file.AbsPath, handler: true, ch: make(chan []byte, 4), done: make(chan struct{})}
	if _, _, err := srv.wire.add(handler, 0); err != nil {
		t.Fatal(err)
	}
	program := helperProgram("search", structuredHelper(t, "result-null"))
	if err := srv.AttachHelpers(file.AbsPath, map[string]config.HelperProgram{"search": program}, nil); !errors.Is(err, errHelperHandlerTaken) {
		t.Fatalf("AttachHelpers error = %v, want external handler conflict", err)
	}
	if err := srv.AttachHelpers(file.AbsPath, map[string]config.HelperProgram{"search": program}, nil); !errors.Is(err, errHelperHandlerTaken) {
		t.Fatalf("repeated AttachHelpers error = %v, want remembered external handler conflict", err)
	}
	mode, helpers := srv.helperDiscovery(file.AbsPath)
	if mode != "raw" || len(helpers) != 1 || helpers[0] != (helperMeta{Name: "search", State: "unavailable"}) {
		t.Fatalf("discovery = mode %q, helpers %+v", mode, helpers)
	}
	observer := helperObserver(t, srv, file.AbsPath)
	body := fmt.Sprintf(`{"type":"wire/request","id":"named","file":%q,"helper":"search"}`, file.AbsPath)
	req := httptest.NewRequest(http.MethodPost, "/_/wire/send", strings.NewReader(body))
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.wireMux().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var dispatch struct {
		Delivered int `json:"delivered"`
		Observers int `json:"observers"`
		Refused   struct {
			Source  string `json:"source"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"refused"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &dispatch); err != nil {
		t.Fatal(err)
	}
	if dispatch.Delivered != 0 || dispatch.Observers != 1 {
		t.Fatalf("dispatch = %+v", dispatch)
	}
	// The reply carries the refusal too. It also goes down the stream, but the
	// two are separate connections with no ordering between them, and a client
	// that settles on "delivered":0 alone reports "nobody is attached" for a
	// handler that was attached and refused for a specific reason.
	if dispatch.Refused.Source != "host" || dispatch.Refused.Code != "helper_protocol_unsupported" || dispatch.Refused.Message == "" {
		t.Fatalf("the POST reply did not carry the typed refusal: %+v", dispatch.Refused)
	}
	if code := helperErrorCode(t, receiveHelperType(t, observer, "wire/error", "named")); code != "helper_protocol_unsupported" {
		t.Fatalf("error code = %q", code)
	}
	select {
	case frame := <-handler.ch:
		t.Fatalf("raw handler received named request: %q", frame)
	default:
	}
}

func TestRevocationCancelsAcceptedEditAndReleasesWatcher(t *testing.T) {
	srv, file := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })
	writeHelperDocument(t, file.AbsPath, "search")
	program := helperProgram("search", structuredHelper(t, "hold"))
	if err := srv.AttachHelpers(file.AbsPath, map[string]config.HelperProgram{"search": program}, nil); err != nil {
		t.Fatal(err)
	}
	observer := helperObserver(t, srv, file.AbsPath)
	srv.wire.publish(file.AbsPath, wireEnvelope{
		V: 1, Type: "wire/request", ID: "active", File: file.AbsPath,
		Helper: "search", Document: "edit", Payload: json.RawMessage(`{}`),
	})
	receiveHelperType(t, observer, "wire/status", "active")
	waitFor(t, 2*time.Second, "the editing helper watcher lease", func() bool { return watchEntries(srv) == 1 })

	srv.wire.publish(file.AbsPath, wireEnvelope{
		V: 1, Type: "wire/request", ID: "active", File: file.AbsPath,
		Helper: "search", Document: "edit", Payload: json.RawMessage(`{}`),
	})
	if code := helperErrorCode(t, receiveHelperType(t, observer, "wire/error", "active")); code != "duplicate_request" {
		t.Fatalf("duplicate code = %q", code)
	}

	srv.DetachHelpers(file.AbsPath)
	waitFor(t, 2*time.Second, "revoked helper cleanup", func() bool { return watchEntries(srv) == 0 })
	if srv.HelpersBound(file.AbsPath) {
		t.Fatal("revoked helper remained bound")
	}
}

func TestEvictedDispatcherCannotPublishIntoAReplacementHandler(t *testing.T) {
	srv, file := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })
	writeHelperDocument(t, file.AbsPath, "search")
	program := helperProgram("search", structuredHelper(t, "result-null"))
	if err := srv.AttachHelpers(file.AbsPath, map[string]config.HelperProgram{"search": program}, nil); err != nil {
		t.Fatal(err)
	}
	srv.helperMu.Lock()
	dispatcher := srv.helpers[file.AbsPath]
	generation := dispatcher.generation
	srv.wire.remove(dispatcher.sub)
	replacement := &wireSub{
		key: file.AbsPath, handler: true, mode: "raw",
		ch: make(chan []byte, 4), done: make(chan struct{}),
	}
	if _, _, err := srv.wire.add(replacement, 0); err != nil {
		srv.helperMu.Unlock()
		t.Fatal(err)
	}
	_, _, published := srv.wire.publishFromHandler(file.AbsPath, dispatcher.sub, wireEnvelope{
		Type: "wire/done", ID: "late", File: file.AbsPath, Payload: json.RawMessage(`{"late":true}`),
	})
	if srv.HelperGeneration(file.AbsPath) != generation {
		srv.helperMu.Unlock()
		t.Fatal("generation changed before dispatcher cleanup reached the binding")
	}
	srv.helperMu.Unlock()
	if published {
		t.Fatal("evicted dispatcher published after losing its handler slot")
	}
	select {
	case frame := <-replacement.ch:
		t.Fatalf("replacement handler received the evicted dispatcher's frame: %q", frame)
	default:
	}
}

func TestDispatcherStopsWhenWireShutsDown(t *testing.T) {
	srv, file := setupLiveSyncTest(t)
	writeHelperDocument(t, file.AbsPath, "search")
	program := helperProgram("search", structuredHelper(t, "result-null"))
	if err := srv.AttachHelpers(file.AbsPath, map[string]config.HelperProgram{"search": program}, nil); err != nil {
		t.Fatal(err)
	}
	srv.wire.shutdown()
	waitFor(t, time.Second, "dispatcher shutdown", func() bool {
		srv.helperMu.Lock()
		_, attached := srv.helpers[file.AbsPath]
		srv.helperMu.Unlock()
		return !attached && !srv.HelpersBound(file.AbsPath)
	})
}

func TestWireHandlerModes(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  string
		ok    bool
	}{
		{"", "raw", true},
		{"raw", "raw", true},
		{"jsonl", "jsonl", true},
		{"auto", "", false},
	} {
		got, ok := wireHandlerMode(tc.input)
		if got != tc.want || ok != tc.ok {
			t.Errorf("wireHandlerMode(%q) = %q, %v; want %q, %v", tc.input, got, ok, tc.want, tc.ok)
		}
	}
}

// A handler's own output must not come back into its own queue. It used to: the
// owner argument gated authorization but not the fanout, so a helper reporting
// progress faster than the dispatcher drained the echo overflowed its 32-entry
// input queue, was evicted from its own handler slot, and lost the terminal
// frame for the request it was answering. Valid status alone was enough.
func TestAHandlerDoesNotReceiveItsOwnOutput(t *testing.T) {
	wh := newWireHub()
	defer wh.shutdown()
	owner := &wireSub{key: "/doc", handler: true, ch: make(chan []byte, wireQueueSize), done: make(chan struct{})}
	if _, _, err := wh.add(owner, 0); err != nil {
		t.Fatal(err)
	}
	observer := &wireSub{key: "/doc", ch: make(chan []byte, wireQueueSize*4), done: make(chan struct{})}
	if _, _, err := wh.add(observer, 0); err != nil {
		t.Fatal(err)
	}

	const burst = wireQueueSize + 8
	for i := 0; i < burst; i++ {
		if _, _, published := wh.publishFromHandler("/doc", owner, wireEnvelope{
			V: 1, Type: "wire/status", ID: "r1", File: "/doc", Text: "tick",
		}); !published {
			t.Fatalf("publish %d was refused, so the handler had already been evicted", i)
		}
	}

	if got := len(owner.ch); got != 0 {
		t.Errorf("the handler received %d of its own frames, want 0", got)
	}
	if owner.removed {
		t.Error("a status burst evicted the handler from its own slot")
	}
	// The observers it was publishing FOR still get every frame.
	if got := len(observer.ch); got != burst {
		t.Errorf("observer received %d frames, want %d", got, burst)
	}
}

func TestHelpersComeBackAfterTheExternalHandlerLeaves(t *testing.T) {
	srv, file := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })
	writeHelperDocument(t, file.AbsPath, "search")
	handler := &wireSub{key: file.AbsPath, handler: true, ch: make(chan []byte, 4), done: make(chan struct{})}
	if _, _, err := srv.wire.add(handler, 0); err != nil {
		t.Fatal(err)
	}
	program := helperProgram("search", structuredHelper(t, "result-null"))
	allowed := map[string]config.HelperProgram{"search": program}
	if err := srv.AttachHelpers(file.AbsPath, allowed, nil); !errors.Is(err, errHelperHandlerTaken) {
		t.Fatalf("AttachHelpers error = %v, want external handler conflict", err)
	}
	// The terminal running `htmlclay wire serve` is quit, and the document is
	// opened again. Nothing about the remembered conflict is still true.
	srv.wire.remove(handler)
	if err := srv.AttachHelpers(file.AbsPath, allowed, nil); err != nil {
		t.Fatalf("AttachHelpers after the external handler left: %v", err)
	}
	mode, helpers := srv.helperDiscovery(file.AbsPath)
	if mode != "jsonl" || len(helpers) != 1 || helpers[0] != (helperMeta{Name: "search", State: "ready"}) {
		t.Fatalf("discovery = mode %q, helpers %+v", mode, helpers)
	}
}

func TestDiscoveryTellsARefusalApartFromAnUndecidedHelper(t *testing.T) {
	srv, file := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })
	t.Cleanup(func() { srv.DetachHelpers(file.AbsPath) })
	writeHelperDocument(t, file.AbsPath, "search", "ocr", "translate", "index")
	program := helperProgram("search", structuredHelper(t, "result-null"))
	gone := helperProgram("index", filepath.Join(t.TempDir(), "unregistered"))
	allowed := map[string]config.HelperProgram{"search": program, "index": gone}
	// ocr was refused. translate was never decided: the user closed the picker,
	// or the program behind an older allow has since been unregistered.
	if err := srv.AttachHelpers(file.AbsPath, allowed, []string{"ocr"}); err != nil {
		t.Fatal(err)
	}
	_, helpers := srv.helperDiscovery(file.AbsPath)
	want := []helperMeta{
		{Name: "search", State: "ready"},
		{Name: "ocr", State: "denied"},
		{Name: "translate", State: "unavailable"},
		{Name: "index", State: "unavailable"},
	}
	if !reflect.DeepEqual(helpers, want) {
		t.Fatalf("discovery = %+v, want %+v", helpers, want)
	}

	// A later refusal of a name that was only undecided has to reach discovery,
	// even though the allow set did not move.
	if err := srv.AttachHelpers(file.AbsPath, allowed, []string{"ocr", "translate"}); err != nil {
		t.Fatal(err)
	}
	if _, helpers := srv.helperDiscovery(file.AbsPath); helpers[2].State != "denied" {
		t.Fatalf("a new refusal did not reach discovery: %+v", helpers)
	}
}

func TestADuplicateRefusalDoesNotBecomeTheLiveRequestsOutcome(t *testing.T) {
	srv, file := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })
	t.Cleanup(func() { srv.DetachHelpers(file.AbsPath) })
	writeHelperDocument(t, file.AbsPath, "search")
	release := filepath.Join(t.TempDir(), "release")
	t.Setenv(serverHelperArg, release)
	program := helperProgram("search", structuredHelper(t, "release"))
	if err := srv.AttachHelpers(file.AbsPath, map[string]config.HelperProgram{"search": program}, nil); err != nil {
		t.Fatal(err)
	}

	observer := &wireSub{key: file.AbsPath, ch: make(chan []byte, 64), done: make(chan struct{})}
	cursor, _, err := srv.wire.add(observer, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.wire.remove(observer) })

	request := wireEnvelope{V: 1, Type: "wire/request", ID: "live", File: file.AbsPath, Helper: "search", Payload: json.RawMessage(`{}`)}
	srv.wire.publish(file.AbsPath, request)
	receiveHelperType(t, observer, "wire/status", "live")

	srv.wire.publish(file.AbsPath, request)
	if code := helperErrorCode(t, receiveHelperType(t, observer, "wire/error", "live")); code != "duplicate_request" {
		t.Fatalf("the second request was not refused as a duplicate")
	}

	if err := os.WriteFile(release, nil, 0644); err != nil {
		t.Fatal(err)
	}
	receiveHelperType(t, observer, "wire/done", "live")

	// A client that reconnects with a cursor from before the request replays the
	// one remembered outcome for that id. It has to be the answer the program
	// produced, not the refusal handed to whoever reused the id.
	replayed := &wireSub{key: file.AbsPath, ch: make(chan []byte, 64), done: make(chan struct{})}
	_, replay, err := srv.wire.add(replayed, cursor)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.wire.remove(replayed) })
	if len(replay) != 1 {
		t.Fatalf("replay = %d frames, want the single retained outcome", len(replay))
	}
	env, err := decodeWireEnvelope(replay[0])
	if err != nil {
		t.Fatal(err)
	}
	if env.Type != "wire/done" || env.ID != "live" {
		t.Fatalf("replayed terminal = %s for %q, want wire/done for live", env.Type, env.ID)
	}
}

func TestAChildReadsTheDocumentModeTheHostResolved(t *testing.T) {
	srv, file := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })
	t.Cleanup(func() { srv.DetachHelpers(file.AbsPath) })
	writeHelperDocument(t, file.AbsPath, "search")
	program := helperProgram("search", structuredHelper(t, "document-mode"))
	if err := srv.AttachHelpers(file.AbsPath, map[string]config.HelperProgram{"search": program}, nil); err != nil {
		t.Fatal(err)
	}
	observer := helperObserver(t, srv, file.AbsPath)

	// The page omits document, which the host reads as "none" and enforces. The
	// child has to be told the same thing rather than left to infer it from a
	// field that is not there.
	srv.wire.publish(file.AbsPath, wireEnvelope{
		V: 1, Type: "wire/request", ID: "unstated", File: file.AbsPath,
		Helper: "search", Payload: json.RawMessage(`{}`),
	})
	env := receiveHelperEnvelope(t, observer)
	for !env.isTerminal() {
		env = receiveHelperEnvelope(t, observer)
	}
	if env.Type != "wire/done" {
		t.Fatalf("terminal = %s: %s", env.Type, env.Text)
	}
}

func TestAnUndecidedHelperAsksOnFirstUseAndThenRuns(t *testing.T) {
	srv, file := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })
	writeHelperDocument(t, file.AbsPath, "search")
	t.Setenv(serverHelperArg, filepath.Join(t.TempDir(), "count"))
	program := helperProgram("search", structuredHelper(t, "count"))
	var calls atomic.Int32
	entered := make(chan struct{}, 1)
	proceed := make(chan struct{})
	ready := make(chan *wireSub, 1)
	srv.SetHooks(Hooks{AskHelper: func(path, name string) bool {
		calls.Add(1)
		entered <- struct{}{}
		<-proceed
		if err := srv.AttachHelpers(path, map[string]config.HelperProgram{name: program}, nil); err != nil {
			t.Errorf("reattach after approval: %v", err)
			return false
		}
		sub := &wireSub{key: path, ch: make(chan []byte, 64), done: make(chan struct{})}
		if _, _, err := srv.wire.add(sub, 0); err != nil {
			t.Errorf("observe after approval: %v", err)
			return false
		}
		ready <- sub
		return true
	}})
	if err := srv.AttachHelpers(file.AbsPath, nil, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.DetachHelpers(file.AbsPath) })
	observer := helperObserver(t, srv, file.AbsPath)

	srv.wire.publish(file.AbsPath, wireEnvelope{
		V: 1, Type: "wire/request", ID: "first", File: file.AbsPath,
		Helper: "search", Document: "none", Payload: json.RawMessage(`{"query":"clay"}`),
	})
	ack := receiveHelperType(t, observer, "wire/ack", "first")
	var accepted struct {
		Mode     string `json:"mode"`
		BudgetMS int    `json:"budgetMs"`
	}
	if err := json.Unmarshal(ack.Payload, &accepted); err != nil {
		t.Fatal(err)
	}
	if accepted.Mode != "jsonl" || accepted.BudgetMS != 900000 {
		t.Fatalf("ask ack = %+v, want jsonl with a 900000ms budget", accepted)
	}
	status := receiveHelperType(t, observer, "wire/status", "first")
	if status.Text != "Waiting for you to allow search in HTML Clay" {
		t.Fatalf("status text = %q", status.Text)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the approval dialog was never raised")
	}
	close(proceed)
	var doneSub *wireSub
	select {
	case doneSub = <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("no wire subscription after the approval reattached the helpers")
	}
	t.Cleanup(func() { srv.wire.remove(doneSub) })
	receiveHelperType(t, doneSub, "wire/done", "first")
	if calls.Load() != 1 {
		t.Fatalf("AskHelper calls = %d, want 1", calls.Load())
	}
}

func TestOneDialogServesEveryRequestForTheSameHelper(t *testing.T) {
	srv, file := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })
	writeHelperDocument(t, file.AbsPath, "search")
	program := helperProgram("search", structuredHelper(t, "result-null"))
	release := make(chan struct{})
	ready := make(chan *wireSub, 1)
	var calls atomic.Int32
	srv.SetHooks(Hooks{AskHelper: func(path, name string) bool {
		calls.Add(1)
		<-release
		if err := srv.AttachHelpers(path, map[string]config.HelperProgram{name: program}, nil); err != nil {
			t.Errorf("reattach after approval: %v", err)
			return false
		}
		sub := &wireSub{key: path, ch: make(chan []byte, 64), done: make(chan struct{})}
		if _, _, err := srv.wire.add(sub, 0); err != nil {
			t.Errorf("observe after approval: %v", err)
			return false
		}
		ready <- sub
		return true
	}})
	if err := srv.AttachHelpers(file.AbsPath, nil, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.DetachHelpers(file.AbsPath) })
	observer := helperObserver(t, srv, file.AbsPath)

	srv.wire.publish(file.AbsPath, wireEnvelope{
		V: 1, Type: "wire/request", ID: "one", File: file.AbsPath,
		Helper: "search", Payload: json.RawMessage(`{}`),
	})
	receiveHelperType(t, observer, "wire/ack", "one")
	srv.wire.publish(file.AbsPath, wireEnvelope{
		V: 1, Type: "wire/request", ID: "two", File: file.AbsPath,
		Helper: "search", Payload: json.RawMessage(`{}`),
	})
	receiveHelperType(t, observer, "wire/ack", "two")
	close(release)
	var doneSub *wireSub
	select {
	case doneSub = <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("no wire subscription after the approval reattached the helpers")
	}
	t.Cleanup(func() { srv.wire.remove(doneSub) })
	seen := make(map[string]bool)
	for len(seen) < 2 {
		env := receiveHelperEnvelope(t, doneSub)
		if env.Type == "wire/done" {
			seen[env.ID] = true
		}
	}
	if !seen["one"] || !seen["two"] {
		t.Fatalf("completed requests = %+v, want both one and two", seen)
	}
	if calls.Load() != 1 {
		t.Fatalf("AskHelper calls = %d, want one dialog for both requests", calls.Load())
	}
}

func TestADeniedForThisOpenHelperIsNeverAskedAbout(t *testing.T) {
	srv, file := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })
	writeHelperDocument(t, file.AbsPath, "search")
	structuredHelper(t, "result-null")
	var calls atomic.Int32
	srv.SetHooks(Hooks{AskHelper: func(string, string) bool {
		calls.Add(1)
		return true
	}})
	if err := srv.AttachHelpers(file.AbsPath, nil, []string{"search"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.DetachHelpers(file.AbsPath) })
	observer := helperObserver(t, srv, file.AbsPath)

	srv.wire.publish(file.AbsPath, wireEnvelope{
		V: 1, Type: "wire/request", ID: "denied", File: file.AbsPath,
		Helper: "search", Payload: json.RawMessage(`{}`),
	})
	if code := helperErrorCode(t, receiveHelperType(t, observer, "wire/error", "denied")); code != "helper_not_granted" {
		t.Fatalf("code = %q, want helper_not_granted", code)
	}
	if calls.Load() != 0 {
		t.Fatalf("AskHelper calls = %d, want none for a name refused this open", calls.Load())
	}
}

func TestADialogThatGrantsNothingQuietsThePageForAMinute(t *testing.T) {
	srv, file := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })
	writeHelperDocument(t, file.AbsPath, "search")
	structuredHelper(t, "result-null")
	var calls atomic.Int32
	srv.SetHooks(Hooks{AskHelper: func(string, string) bool {
		calls.Add(1)
		return false
	}})
	if err := srv.AttachHelpers(file.AbsPath, nil, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.DetachHelpers(file.AbsPath) })
	observer := helperObserver(t, srv, file.AbsPath)

	srv.wire.publish(file.AbsPath, wireEnvelope{
		V: 1, Type: "wire/request", ID: "first", File: file.AbsPath,
		Helper: "search", Payload: json.RawMessage(`{}`),
	})
	if code := helperErrorCode(t, receiveHelperType(t, observer, "wire/error", "first")); code != "helper_not_granted" {
		t.Fatalf("first code = %q, want helper_not_granted", code)
	}
	srv.wire.publish(file.AbsPath, wireEnvelope{
		V: 1, Type: "wire/request", ID: "second", File: file.AbsPath,
		Helper: "search", Payload: json.RawMessage(`{}`),
	})
	if code := helperErrorCode(t, receiveHelperType(t, observer, "wire/error", "second")); code != "helper_not_granted" {
		t.Fatalf("second code = %q, want helper_not_granted", code)
	}
	if calls.Load() != 1 {
		t.Fatalf("AskHelper calls = %d, want the page to wait after a dialog that granted nothing", calls.Load())
	}
}

func TestLoadingThePageNeverAsksAboutAnUndecidedHelper(t *testing.T) {
	srv, file := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })
	writeHelperDocument(t, file.AbsPath, "search")
	structuredHelper(t, "result-null")
	var calls atomic.Int32
	srv.SetHooks(Hooks{AskHelper: func(string, string) bool {
		calls.Add(1)
		return true
	}})
	if err := srv.AttachHelpers(file.AbsPath, nil, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.DetachHelpers(file.AbsPath) })
	observer := helperObserver(t, srv, file.AbsPath)

	srv.wire.publish(file.AbsPath, wireEnvelope{
		V: 1, Type: "wire/describe", ID: "describe", File: file.AbsPath,
		Helper: "search", Payload: json.RawMessage(`{}`),
	})
	if code := helperErrorCode(t, receiveHelperType(t, observer, "wire/error", "describe")); code != "helper_not_granted" {
		t.Fatalf("describe code = %q, want helper_not_granted", code)
	}
	if calls.Load() != 0 {
		t.Fatalf("AskHelper calls = %d, want none for wire/describe", calls.Load())
	}
}

func TestARequestCancelledWhileParkedNeverRuns(t *testing.T) {
	srv, file := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })
	writeHelperDocument(t, file.AbsPath, "search")
	countPath := filepath.Join(t.TempDir(), "count")
	t.Setenv(serverHelperArg, countPath)
	program := helperProgram("search", structuredHelper(t, "count"))
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	srv.SetHooks(Hooks{AskHelper: func(path, name string) bool {
		entered <- struct{}{}
		<-release
		if err := srv.AttachHelpers(path, map[string]config.HelperProgram{name: program}, nil); err != nil {
			t.Errorf("reattach after approval: %v", err)
			return false
		}
		return true
	}})
	if err := srv.AttachHelpers(file.AbsPath, nil, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.DetachHelpers(file.AbsPath) })
	observer := helperObserver(t, srv, file.AbsPath)

	srv.wire.publish(file.AbsPath, wireEnvelope{
		V: 1, Type: "wire/request", ID: "parked", File: file.AbsPath,
		Helper: "search", Payload: json.RawMessage(`{}`),
	})
	receiveHelperType(t, observer, "wire/ack", "parked")
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the approval dialog was never raised")
	}
	srv.wire.publish(file.AbsPath, wireEnvelope{
		V: 1, Type: "wire/cancel", ID: "parked", File: file.AbsPath,
	})
	close(release)
	waitFor(t, time.Second, "the parked request to be released", func() bool {
		srv.helperMu.Lock()
		defer srv.helperMu.Unlock()
		_, asking := srv.helperAsking[helperAskKey(file.AbsPath, "search")]
		return !asking
	})
	if _, err := os.Stat(countPath); err == nil {
		t.Fatal("a request cancelled while parked ran the helper program")
	}
}

func TestRegrantKeepsARunningRequestAlive(t *testing.T) {
	srv, file := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })
	writeHelperDocument(t, file.AbsPath, "a", "b")
	releasePath := filepath.Join(t.TempDir(), "release")
	t.Setenv(serverHelperArg, releasePath)
	exe := structuredHelper(t, "release")
	aProgram := helperProgram("a", exe)
	bProgram := helperProgram("b", exe)
	if err := srv.AttachHelpers(file.AbsPath, map[string]config.HelperProgram{"a": aProgram}, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.DetachHelpers(file.AbsPath) })
	observer := helperObserver(t, srv, file.AbsPath)

	srv.wire.publish(file.AbsPath, wireEnvelope{
		V: 1, Type: "wire/request", ID: "run", File: file.AbsPath,
		Helper: "a", Document: "none", Payload: json.RawMessage(`{}`),
	})
	receiveHelperType(t, observer, "wire/ack", "run")

	srv.helperMu.Lock()
	before := srv.helpers[file.AbsPath]
	srv.helperMu.Unlock()
	if err := srv.AttachHelpers(file.AbsPath, map[string]config.HelperProgram{"a": aProgram, "b": bProgram}, nil); err != nil {
		t.Fatal(err)
	}
	srv.helperMu.Lock()
	after := srv.helpers[file.AbsPath]
	srv.helperMu.Unlock()
	if after != before {
		t.Fatal("regranting an unchanged declaration replaced the dispatcher")
	}
	if err := os.WriteFile(releasePath, []byte("go\n"), 0644); err != nil {
		t.Fatal(err)
	}
	receiveHelperType(t, observer, "wire/done", "run")
}

func TestRegrantCancelsARunningRequestThatLostItsGrant(t *testing.T) {
	srv, file := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })
	writeHelperDocument(t, file.AbsPath, "a", "b")
	t.Setenv(serverHelperArg, filepath.Join(t.TempDir(), "release"))
	exe := structuredHelper(t, "release")
	aProgram := helperProgram("a", exe)
	bProgram := helperProgram("b", exe)
	if err := srv.AttachHelpers(file.AbsPath, map[string]config.HelperProgram{"a": aProgram, "b": bProgram}, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.DetachHelpers(file.AbsPath) })
	observer := helperObserver(t, srv, file.AbsPath)

	srv.wire.publish(file.AbsPath, wireEnvelope{
		V: 1, Type: "wire/request", ID: "run", File: file.AbsPath,
		Helper: "a", Document: "none", Payload: json.RawMessage(`{}`),
	})
	receiveHelperType(t, observer, "wire/ack", "run")

	if err := srv.AttachHelpers(file.AbsPath, map[string]config.HelperProgram{"b": bProgram}, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	env := receiveHelperType(t, observer, "wire/error", "run")
	if code := helperErrorCode(t, env); code != "helper_cancelled" {
		t.Fatalf("terminal code = %q, want helper_cancelled", code)
	}
}

func TestAQuietFileRefusesAnotherUndecidedNameWithoutAsking(t *testing.T) {
	srv, file := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })
	writeHelperDocument(t, file.AbsPath, "a", "b")
	structuredHelper(t, "result-null")
	var calls atomic.Int32
	srv.SetHooks(Hooks{AskHelper: func(string, string) bool {
		calls.Add(1)
		return false
	}})
	if err := srv.AttachHelpers(file.AbsPath, nil, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.DetachHelpers(file.AbsPath) })
	observer := helperObserver(t, srv, file.AbsPath)

	srv.wire.publish(file.AbsPath, wireEnvelope{
		V: 1, Type: "wire/request", ID: "first", File: file.AbsPath,
		Helper: "a", Payload: json.RawMessage(`{}`),
	})
	if code := helperErrorCode(t, receiveHelperType(t, observer, "wire/error", "first")); code != "helper_not_granted" {
		t.Fatalf("first code = %q, want helper_not_granted", code)
	}
	srv.wire.publish(file.AbsPath, wireEnvelope{
		V: 1, Type: "wire/request", ID: "second", File: file.AbsPath,
		Helper: "b", Payload: json.RawMessage(`{}`),
	})
	if code := helperErrorCode(t, receiveHelperType(t, observer, "wire/error", "second")); code != "helper_not_granted" {
		t.Fatalf("second code = %q, want helper_not_granted", code)
	}
	if calls.Load() != 1 {
		t.Fatalf("AskHelper calls = %d, want the whole file to be quiet after one refusal", calls.Load())
	}
}

func TestAParkedRequestIsAnsweredWhenTheDispatcherIsGone(t *testing.T) {
	srv, file := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })
	writeHelperDocument(t, file.AbsPath, "search")
	structuredHelper(t, "result-null")
	reconnected := make(chan *wireSub, 1)
	srv.SetHooks(Hooks{AskHelper: func(path, name string) bool {
		srv.DetachHelpers(path)
		sub := &wireSub{key: path, ch: make(chan []byte, 64), done: make(chan struct{})}
		if _, _, err := srv.wire.add(sub, 0); err != nil {
			t.Errorf("reconnect after detach: %v", err)
		}
		reconnected <- sub
		return false
	}})
	if err := srv.AttachHelpers(file.AbsPath, nil, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.DetachHelpers(file.AbsPath) })
	observer := helperObserver(t, srv, file.AbsPath)

	srv.wire.publish(file.AbsPath, wireEnvelope{
		V: 1, Type: "wire/request", ID: "parked", File: file.AbsPath,
		Helper: "search", Payload: json.RawMessage(`{}`),
	})
	receiveHelperType(t, observer, "wire/ack", "parked")
	var page *wireSub
	select {
	case page = <-reconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("the dialog never detached the dispatcher")
	}
	t.Cleanup(func() { srv.wire.remove(page) })
	env := receiveHelperType(t, page, "wire/error", "parked")
	if code := helperErrorCode(t, env); code != "helper_not_granted" {
		t.Fatalf("terminal code = %q, want helper_not_granted", code)
	}
}

func TestAMalformedRequestIsRefusedBeforeItCanAsk(t *testing.T) {
	srv, file := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })
	writeHelperDocument(t, file.AbsPath, "search")
	structuredHelper(t, "result-null")
	var calls atomic.Int32
	srv.SetHooks(Hooks{AskHelper: func(string, string) bool {
		calls.Add(1)
		return false
	}})
	if err := srv.AttachHelpers(file.AbsPath, nil, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.DetachHelpers(file.AbsPath) })
	observer := helperObserver(t, srv, file.AbsPath)

	srv.wire.publish(file.AbsPath, wireEnvelope{
		V: 1, Type: "wire/request", ID: "bad", File: file.AbsPath,
		Helper: "search", Document: "bogus", Payload: json.RawMessage(`{}`),
	})
	if code := helperErrorCode(t, receiveHelperType(t, observer, "wire/error", "bad")); code != "invalid_document" {
		t.Fatalf("code = %q, want invalid_document", code)
	}
	if calls.Load() != 0 {
		t.Fatalf("AskHelper calls = %d, want none for a malformed request", calls.Load())
	}
}
