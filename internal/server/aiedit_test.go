package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func aiEditSend(t *testing.T, srv *Server, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/_/wire/send", strings.NewReader(body))
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.Header.Set("Content-Type", "application/json")
	req = sameOriginHeaders(req)
	req.Header.Set("Document-URL", helperPageURL(srv))
	if token != "" {
		req.Header.Set(helperTokenHeader, token)
	}
	w := httptest.NewRecorder()
	srv.wireMux().ServeHTTP(w, req)
	return w
}

func aiEditBody(id, comment string) string {
	return fmt.Sprintf(`{"type":"wire/request","id":%q,"helper":"ai-edit","payload":{"elementHTML":"<p>Old</p>","tag":"p","comment":%q,"editId":"p"}}`, id, comment)
}

func aiEditDelivered(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", w.Code, w.Body.String())
	}
	var reply struct {
		Delivered int `json:"delivered"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Delivered != 1 {
		t.Fatalf("delivered = %d, want 1: %s", reply.Delivered, w.Body.String())
	}
}

func aiEditEnvelopeType(t *testing.T, sub *wireSub, id string, frames int) wireEnvelope {
	t.Helper()
	for i := 0; i < frames; i++ {
		env := receiveHelperEnvelope(t, sub)
		if env.Type == "wire/done" || env.Type == "wire/error" {
			if env.ID != id {
				t.Fatalf("terminal frame for %q, want %q", env.ID, id)
			}
			return env
		}
	}
	t.Fatalf("no terminal frame for %s", id)
	return wireEnvelope{}
}

func aiEditSwitch(on *atomic.Bool) Hooks {
	return Hooks{AIEditEnabled: func() bool { return on.Load() }}
}

func aiEditDiscoveryMeta(t *testing.T, srv *Server, file string) []helperMeta {
	t.Helper()
	_, meta := srv.helperDiscovery(file)
	return meta
}

func TestAIEditDiscovery(t *testing.T) {
	srv, f := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })
	t.Cleanup(srv.CancelAIEdits)

	for _, entry := range aiEditDiscoveryMeta(t, srv, f.AbsPath) {
		if entry.Name == "ai-edit" {
			t.Fatal("a server with no AI editing hooks offered ai-edit")
		}
	}

	var on atomic.Bool
	on.Store(true)
	srv.SetHooks(aiEditSwitch(&on))

	meta := aiEditDiscoveryMeta(t, srv, f.AbsPath)
	if len(meta) != 1 || meta[0] != (helperMeta{Name: "ai-edit", State: "ready"}) {
		t.Fatalf("enabled discovery = %+v", meta)
	}

	on.Store(false)
	meta = aiEditDiscoveryMeta(t, srv, f.AbsPath)
	if len(meta) != 1 || meta[0] != (helperMeta{Name: "ai-edit", State: "unavailable"}) {
		t.Fatalf("disabled discovery = %+v", meta)
	}

	on.Store(true)
	srv.helperMu.Lock()
	if srv.helperOffline == nil {
		srv.helperOffline = make(map[string][]string)
	}
	srv.helperOffline[f.AbsPath] = []string{"search", "ai-edit"}
	srv.helperMu.Unlock()

	meta = aiEditDiscoveryMeta(t, srv, f.AbsPath)
	want := []helperMeta{{Name: "search", State: "unavailable"}, {Name: "ai-edit", State: "ready"}}
	if len(meta) != len(want) || meta[0] != want[0] || meta[1] != want[1] {
		t.Fatalf("declared-plus-built-in discovery = %+v, want %+v", meta, want)
	}
}

func TestAIEditAnswersWithTheMock(t *testing.T) {
	t.Setenv("MOCK_MODEL", "1")
	srv, f := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })
	t.Cleanup(srv.CancelAIEdits)
	var on atomic.Bool
	on.Store(true)
	srv.SetHooks(aiEditSwitch(&on))

	sub := helperObserver(t, srv, f.AbsPath)
	aiEditDelivered(t, aiEditSend(t, srv, f.Token, aiEditBody("e1", "make it [mock:end_turn]")))

	ack := receiveHelperType(t, sub, "wire/ack", "e1")
	var ackPayload struct {
		Mode     string `json:"mode"`
		BudgetMs int64  `json:"budgetMs"`
	}
	if err := json.Unmarshal(ack.Payload, &ackPayload); err != nil {
		t.Fatal(err)
	}
	if ackPayload.Mode != "jsonl" || ackPayload.BudgetMs != 300000 {
		t.Fatalf("ack payload = %+v", ackPayload)
	}

	done := receiveHelperType(t, sub, "wire/done", "e1")
	var result struct {
		HTML       string `json:"html"`
		Model      string `json:"model"`
		StopReason string `json:"stopReason"`
	}
	if err := json.Unmarshal(done.Payload, &result); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.HTML, "mock edit: make it") {
		t.Fatalf("done html = %q", result.HTML)
	}
	if result.Model != "mock(claude-opus-5-5)" {
		t.Fatalf("model = %q, want mock(claude-opus-5-5)", result.Model)
	}
}

func TestAIEditRequiresTheSaveToken(t *testing.T) {
	t.Setenv("MOCK_MODEL", "1")
	srv, f := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })
	t.Cleanup(srv.CancelAIEdits)
	var on atomic.Bool
	on.Store(true)
	srv.SetHooks(aiEditSwitch(&on))

	sub := helperObserver(t, srv, f.AbsPath)
	if w := aiEditSend(t, srv, "", aiEditBody("e1", "make it")); w.Code != http.StatusForbidden {
		t.Fatalf("tokenless status %d, want 403: %s", w.Code, w.Body.String())
	}
	select {
	case raw := <-sub.ch:
		t.Fatalf("tokenless send published a frame: %s", raw)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestAIEditOffIsRefused(t *testing.T) {
	t.Setenv("MOCK_MODEL", "1")
	srv, f := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })
	t.Cleanup(srv.CancelAIEdits)
	var on atomic.Bool
	srv.SetHooks(aiEditSwitch(&on))

	sub := helperObserver(t, srv, f.AbsPath)
	aiEditDelivered(t, aiEditSend(t, srv, f.Token, aiEditBody("e1", "make it")))

	env := receiveHelperType(t, sub, "wire/error", "e1")
	if code := helperErrorCode(t, env); code != "helper_not_granted" {
		t.Fatalf("error code = %q, want helper_not_granted", code)
	}
}

func TestAIEditOneAtATime(t *testing.T) {
	t.Setenv("MOCK_MODEL", "1")
	srv, f := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })
	t.Cleanup(srv.CancelAIEdits)
	var on atomic.Bool
	on.Store(true)
	srv.SetHooks(aiEditSwitch(&on))

	sub := helperObserver(t, srv, f.AbsPath)
	long := "make it " + strings.Repeat("x", 3000)
	aiEditDelivered(t, aiEditSend(t, srv, f.Token, aiEditBody("e1", long)))
	receiveHelperType(t, sub, "wire/ack", "e1")

	aiEditDelivered(t, aiEditSend(t, srv, f.Token, aiEditBody("e2", long)))
	if env := aiEditEnvelopeType(t, sub, "e2", 8); helperErrorCode(t, env) != "helper_busy" {
		t.Fatalf("busy error = %s", env.Payload)
	}

	aiEditDelivered(t, aiEditSend(t, srv, f.Token, aiEditBody("e1", long)))
	if env := aiEditEnvelopeType(t, sub, "e1", 8); helperErrorCode(t, env) != "duplicate_request" {
		t.Fatalf("duplicate error = %s", env.Payload)
	}

	aiEditDelivered(t, aiEditSend(t, srv, f.Token, `{"type":"wire/cancel","id":"e1"}`))
	if env := aiEditEnvelopeType(t, sub, "e1", 8); env.Type != "wire/error" || helperErrorCode(t, env) != "helper_cancelled" {
		t.Fatalf("cancel answer = %s %s", env.Type, env.Payload)
	}

	aiEditDelivered(t, aiEditSend(t, srv, f.Token, aiEditBody("e3", "short [mock:end_turn]")))
	if env := aiEditEnvelopeType(t, sub, "e3", 8); env.Type != "wire/done" {
		t.Fatalf("next request = %s %s", env.Type, env.Payload)
	}
}

func TestCancelAIEditsStopsRunningEdits(t *testing.T) {
	t.Setenv("MOCK_MODEL", "1")
	srv, f := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })
	t.Cleanup(srv.CancelAIEdits)
	var on atomic.Bool
	on.Store(true)
	srv.SetHooks(aiEditSwitch(&on))

	sub := helperObserver(t, srv, f.AbsPath)
	aiEditDelivered(t, aiEditSend(t, srv, f.Token, aiEditBody("e1", "make it "+strings.Repeat("x", 3000))))
	receiveHelperType(t, sub, "wire/ack", "e1")

	srv.CancelAIEdits()
	if env := aiEditEnvelopeType(t, sub, "e1", 8); env.Type != "wire/error" || helperErrorCode(t, env) != "helper_cancelled" {
		t.Fatalf("cancel answer = %s %s", env.Type, env.Payload)
	}
}
