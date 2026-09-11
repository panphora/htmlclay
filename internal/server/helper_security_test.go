package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/panphora/htmlclay/internal/logging"
)

func helperPageURL(srv *Server) string {
	return fmt.Sprintf("http://127.0.0.1:%d/page.htmlclay", srv.port)
}

func helperRelayRequest(t *testing.T, srv *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Document-URL", helperPageURL(srv))
	w := httptest.NewRecorder()
	srv.handleLiveSyncSave(w, req)
	return w
}

func TestHelperBoundRelayRefusesBothAliasesAndLanes(t *testing.T) {
	srv, f := setupLiveSyncTest(t)
	srv.BindHelpers(f.AbsPath)

	cases := []struct {
		name string
		path string
		body string
	}{
		{"sync live lane", "/_/sync", `{"snapshot":"<html><body>peer</body></html>"}`},
		{"sync saved lane", "/_/sync", `{"document":"<html><body>peer</body></html>"}`},
		{"legacy live lane", "/_/live-sync/save", `{"html":"<div>peer</div>"}`},
		{"legacy saved lane", "/_/live-sync/save", `{"document":"<div>peer</div>"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := helperRelayRequest(t, srv, tc.path, tc.body)
			if w.Code != http.StatusForbidden {
				t.Fatalf("status %d, want 403: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestUnboundRelayAliasesAndLanesKeepTheirExistingBehavior(t *testing.T) {
	srv, _ := setupLiveSyncTest(t)
	cases := []struct {
		name string
		path string
		body string
	}{
		{"sync live lane", "/_/sync", `{"snapshot":"<html><body>peer</body></html>"}`},
		{"sync saved lane", "/_/sync", `{"document":"<html><body>peer</body></html>"}`},
		{"legacy live lane", "/_/live-sync/save", `{"html":"<div>peer</div>"}`},
		{"legacy saved lane", "/_/live-sync/save", `{"document":"<div>peer</div>"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := helperRelayRequest(t, srv, tc.path, tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("status %d, want 200: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestHelperSubscriptionsRequireTheDocumentToken(t *testing.T) {
	srv, f := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })
	srv.BindHelpers(f.AbsPath)

	cases := []struct {
		name    string
		target  string
		browser bool
	}{
		{
			name:    "browser observer",
			target:  "/_/wire/subscribe?document-url=" + url.QueryEscape(helperPageURL(srv)),
			browser: true,
		},
		{
			name:   "header-free local handler",
			target: "/_/wire/subscribe?role=handler&file=" + url.QueryEscape(f.AbsPath),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tc.target, nil)
			req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
			if tc.browser {
				req = sameOriginHeaders(req)
			}
			w := httptest.NewRecorder()
			srv.wireMux().ServeHTTP(w, req)
			if w.Code != http.StatusForbidden {
				t.Fatalf("status %d, want 403: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestHelperSubscriptionAcceptsTheDocumentTokenInTheQuery(t *testing.T) {
	srv, f := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })
	srv.BindHelpers(f.AbsPath)
	ts := httptest.NewServer(srv.wireMux())
	t.Cleanup(ts.Close)

	q := url.Values{
		"document-url":   {ts.URL + "/page.htmlclay"},
		helperTokenQuery: {f.Token},
	}
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", ts.URL+"/_/wire/subscribe?"+q.Encode(), nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
}

func TestHelperSendsRequireTheDocumentToken(t *testing.T) {
	srv, f := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })
	srv.BindHelpers(f.AbsPath)

	cases := []struct {
		name    string
		body    string
		browser bool
	}{
		{
			name: "header-free local process",
			body: fmt.Sprintf(`{"type":"wire/request","id":"local","file":%q}`, f.AbsPath),
		},
		{
			name:    "browser page",
			body:    `{"type":"wire/request","id":"page"}`,
			browser: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := func(token string) *httptest.ResponseRecorder {
				req := httptest.NewRequest("POST", "/_/wire/send", strings.NewReader(tc.body))
				req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
				req.Header.Set("Content-Type", "application/json")
				if tc.browser {
					req = sameOriginHeaders(req)
					req.Header.Set("Document-URL", helperPageURL(srv))
				}
				if token != "" {
					req.Header.Set(helperTokenHeader, token)
				}
				w := httptest.NewRecorder()
				srv.wireMux().ServeHTTP(w, req)
				return w
			}

			if w := request(""); w.Code != http.StatusForbidden {
				t.Fatalf("tokenless status %d, want 403: %s", w.Code, w.Body.String())
			}
			if w := request(f.Token); w.Code != http.StatusOK {
				t.Fatalf("credentialed status %d, want 200: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestHelperBindingTransitionsAdvanceGenerationAndAreIdempotent(t *testing.T) {
	srv, f := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })

	if srv.HelpersBound(f.AbsPath) || srv.HelperGeneration(f.AbsPath) != 0 {
		t.Fatal("a new server started with a helper binding")
	}
	bound := srv.BindHelpers(f.AbsPath)
	if !srv.HelpersBound(f.AbsPath) || bound == 0 || srv.HelperGeneration(f.AbsPath) != bound {
		t.Fatalf("bind state = bound %v, returned %d, stored %d", srv.HelpersBound(f.AbsPath), bound, srv.HelperGeneration(f.AbsPath))
	}
	if again := srv.BindHelpers(f.AbsPath); again != bound {
		t.Fatalf("idempotent bind advanced generation from %d to %d", bound, again)
	}
	unbound := srv.UnbindHelpers(f.AbsPath)
	if srv.HelpersBound(f.AbsPath) || unbound <= bound || srv.HelperGeneration(f.AbsPath) != unbound {
		t.Fatalf("unbind state = bound %v, returned %d, stored %d", srv.HelpersBound(f.AbsPath), unbound, srv.HelperGeneration(f.AbsPath))
	}
	if again := srv.UnbindHelpers(f.AbsPath); again != unbound {
		t.Fatalf("idempotent unbind advanced generation from %d to %d", unbound, again)
	}
}

func TestHelperBindingTransitionsEvictQueuesAndDropReplay(t *testing.T) {
	srv, f := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })

	handler := &wireSub{key: f.AbsPath, handler: true, ch: make(chan []byte, 4), done: make(chan struct{})}
	observer := &wireSub{key: f.AbsPath, ch: make(chan []byte, 4), done: make(chan struct{})}
	if _, _, err := srv.wire.add(handler, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := srv.wire.add(observer, 0); err != nil {
		t.Fatal(err)
	}
	srv.wire.publish(f.AbsPath, wireEnvelope{Type: "wire/done", ID: "before-bind", Text: "secret"})

	srv.BindHelpers(f.AbsPath)
	select {
	case <-observer.done:
	default:
		t.Fatal("binding left a pre-bind observer attached")
	}
	select {
	case frame := <-handler.ch:
		t.Fatalf("binding left a pre-bind request queued: %q", frame)
	default:
	}
	select {
	case <-handler.done:
		t.Fatal("binding displaced the handler whose slot the dispatcher won")
	default:
	}

	srv.wire.publish(f.AbsPath, wireEnvelope{Type: "wire/done", ID: "bound", Text: "secret"})
	srv.UnbindHelpers(f.AbsPath)
	select {
	case <-handler.done:
	default:
		t.Fatal("unbinding left the helper handler attached")
	}

	resumed := &wireSub{key: f.AbsPath, ch: make(chan []byte, 4), done: make(chan struct{})}
	_, replay, err := srv.wire.add(resumed, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(replay) != 0 {
		t.Fatalf("revocation replayed %d retained helper frames", len(replay))
	}
	srv.wire.mu.Lock()
	retainedBytes := srv.wire.terminalBytes
	srv.wire.mu.Unlock()
	if retainedBytes != 0 {
		t.Fatalf("revocation retained %d helper frame bytes", retainedBytes)
	}
}

func TestHelperSubscriptionTokenDoesNotReachTheAccessLog(t *testing.T) {
	srv, f := setupLiveSyncTest(t)
	logPath := fmt.Sprintf("%s/access.log", t.TempDir())
	logger, err := logging.New(logPath)
	if err != nil {
		t.Fatal(err)
	}
	srv.logger = logger

	h := srv.loggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest("GET", "/_/wire/subscribe?"+helperTokenQuery+"="+url.QueryEscape(f.Token), nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	logger.Close()

	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logged), f.Token) {
		t.Fatalf("subscription token reached the access log: %s", logged)
	}
}
