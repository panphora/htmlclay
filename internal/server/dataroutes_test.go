package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/panphora/htmlclay/internal/session"
)

// get drives the handler the same way the mux would, but directly, so a test names the path value
// explicitly rather than depending on pattern matching. Routing itself is tested through the real
// mux in TestDataAPIRouting.
func get(t *testing.T, srv *Server, target, pathValue string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", target, nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.SetPathValue("path", pathValue)
	w := httptest.NewRecorder()
	srv.handleServeFile(w, req)
	return w
}

func getAPI(t *testing.T, srv *Server, pathValue string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/_/api/"+pathValue, nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.SetPathValue("path", pathValue)
	w := httptest.NewRecorder()
	srv.handleDataAPI(w, req)
	return w
}

func writeHome(t *testing.T, srv *Server, name, body string) string {
	t.Helper()
	p := filepath.Join(srv.sessions.HomeDir(), name)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

func decodeError(t *testing.T, w *httptest.ResponseRecorder) dataError {
	t.Helper()
	var body dataError
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body is not JSON (%d): %s", w.Code, w.Body.String())
	}
	return body
}

func TestDataQueryExtracts(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)

	w := get(t, srv, `/test.htmlclay?data={t:"title",p:"p"}`, "test.htmlclay")
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != `{"t":"Test","p":"Hello"}` {
		t.Errorf("body = %s", got)
	}

	if ct := w.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	if x := w.Header().Get("X-Content-Type-Options"); x != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", x)
	}

	// No CORS, deliberately: every http://localhost:* origin is SAME-site with htmlclay's, so
	// security.go's cross-site check does not stop another local dev server from issuing this
	// request. The absence of these headers is the only thing keeping it from reading the JSON.
	for _, h := range []string{"Access-Control-Allow-Origin", "Access-Control-Allow-Credentials"} {
		if v := w.Header().Get(h); v != "" {
			t.Errorf("%s = %q; the data faces must not be CORS-enabled", h, v)
		}
	}
}

// The whole point of the mode flag rather than a second pipeline: a data request is the same read,
// with a different terminal write. It must not create the editing state a navigation creates.
func TestDataQueryTouchesNoPerFileState(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	w := get(t, srv, `/test.htmlclay?data={t:"title"}`, "test.htmlclay")
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	if strings.Contains(w.Body.String(), "savetoken") {
		t.Error("a data response carried a save token")
	}
	if len(w.Result().Cookies()) != 0 {
		t.Errorf("a data response set %d cookie(s); it must not flip the client into edit mode",
			len(w.Result().Cookies()))
	}
	if f.Observed() {
		t.Error("a data request marked the file observed; a JSON read is not an editing session")
	}
	if key := f.HistoryKey(); key != "" {
		t.Errorf("a data request resolved a history key (%q)", key)
	}

	// ...and the plain GET that follows still does all of it, so nothing was permanently skipped.
	plain := get(t, srv, "/test.htmlclay", "test.htmlclay")
	if !strings.Contains(plain.Body.String(), "savetoken") {
		t.Error("the plain GET after a data request lost its token")
	}
	if !f.Observed() {
		t.Error("the plain GET after a data request did not mark the file observed")
	}
}

// The token is injected on the way OUT and is normally absent from disk, but "normally" is not an
// invariant: an external writer or a saved copy of a served page can put one back. A capability
// must not become extractable because disk happens to contain it.
func TestDataQueryCannotReadATokenPresentOnDisk(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)
	writeHome(t, srv, "test.htmlclay",
		`<html savetoken="`+f.Token+`"><head><title>T</title></head><body><p>x</p></body></html>`)

	w := get(t, srv, `/test.htmlclay?data={tok:"html@savetoken",t:"title"}`, "test.htmlclay")
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), f.Token) {
		t.Fatalf("the save token was extractable from disk bytes: %s", w.Body.String())
	}
	if got := w.Body.String(); got != `{"tok":null,"t":"T"}` {
		t.Errorf("body = %s, want the token read as null", got)
	}
}

func TestDataQueryErrors(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)

	cases := []struct {
		name, target, wantError string
		wantStatus              int
	}{
		{"empty parameter", `/test.htmlclay?data=`, "Missing data parameter", 400},
		{"malformed rules", `/test.htmlclay?data={t:`, "Invalid extraction rules", 400},
		{"rejected selector", `/test.htmlclay?data={t:"p:matches(x)"}`, "Invalid CSS selector", 400},
		{"broken selector", `/test.htmlclay?data={t:"p["}`, "Invalid CSS selector", 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := get(t, srv, c.target, "test.htmlclay")
			if w.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d: %s", w.Code, c.wantStatus, w.Body.String())
			}
			if got := decodeError(t, w).Error; got != c.wantError {
				t.Errorf("error = %q, want %q", got, c.wantError)
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
				t.Errorf("error Content-Type = %q", ct)
			}
		})
	}
}

// r.URL.Query() discards its parse error and returns a map with no "data" key, so this request
// would silently serve the HTML with a 200 — the caller asking for JSON would parse a web page.
func TestDataQueryUndecodableIsNotServedAsHTML(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)

	w := get(t, srv, "/test.htmlclay?data=%zz", "test.htmlclay")
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "<html") {
		t.Fatal("an undecodable data query served HTML")
	}
	if got := decodeError(t, w).Error; got != "Invalid extraction rules" {
		t.Errorf("error = %q", got)
	}

	// The mirror, and the reason this cannot just 400 on any bad query: a page with an undecodable
	// query that is NOT a data request must be served exactly as it is today.
	plain := get(t, srv, "/test.htmlclay?other=%zz", "test.htmlclay")
	if plain.Code != 200 || !strings.Contains(plain.Body.String(), "<html") {
		t.Errorf("a non-data request with a bad query = %d, want the page: %s", plain.Code, plain.Body.String())
	}
}

func TestDataQueryIgnoredOnNonHTMLPath(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	registerSubdirPage(t, srv, "sub")
	writeHome(t, srv, "sub/style.css", "body{color:red}")

	w := get(t, srv, `/sub/style.css?data={t:"h1"}`, "sub/style.css")
	if w.Code != 200 {
		t.Fatalf("status = %d, want the file served: %s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != "body{color:red}" {
		t.Errorf("body = %q, want the css served unchanged", got)
	}
}

// extractFilePath truncates at the first .html/.htmlclay segment boundary, so a client-side route
// under a page resolves to the page. Both faces must agree about which file a URL names.
func TestDataFacesAgreeOnPathTruncation(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	writeHome(t, srv, "test.htmlclay",
		`<html><head><title>T</title></head><body><script data-rules-name="api" data-rules-version="1">{t:"title"}</script></body></html>`)

	q := get(t, srv, `/test.htmlclay/spa/deep?data={t:"title"}`, "test.htmlclay/spa/deep")
	if q.Code != 200 || q.Body.String() != `{"t":"T"}` {
		t.Errorf("?data= on a sub-path = %d %s", q.Code, q.Body.String())
	}

	a := getAPI(t, srv, "test.htmlclay/spa/deep")
	if a.Code != 200 || a.Body.String() != `{"t":"T"}` {
		t.Errorf("/_/api on a sub-path = %d %s", a.Code, a.Body.String())
	}
}

func TestDataAPIUsesTheDocumentsRulesTag(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	writeHome(t, srv, "test.htmlclay",
		`<html><head><title>T</title></head><body><h1>Head</h1>`+
			`<script data-rules-name="api" data-rules-version="1">{heading:"h1"}</script></body></html>`)

	w := getAPI(t, srv, "test.htmlclay")
	if w.Code != 200 {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != `{"heading":"Head"}` {
		t.Errorf("body = %s", got)
	}

	// ?data= is ignored on this face: it takes its rules from the document.
	req := httptest.NewRequest("GET", `/_/api/test.htmlclay?data={t:"title"}`, nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.SetPathValue("path", "test.htmlclay")
	ignored := httptest.NewRecorder()
	srv.handleDataAPI(ignored, req)
	if got := ignored.Body.String(); got != `{"heading":"Head"}` {
		t.Errorf("?data= on the /_/api face changed the answer: %s", got)
	}
}

func TestDataAPIErrors(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)

	cases := []struct {
		name, page, path, wantError string
		wantStatus                  int
	}{
		{"no rules tag", `<html><body><h1>x</h1></body></html>`,
			"test.htmlclay", "No api rules tag", 400},
		{"unsupported version", `<html><body><script data-rules-name="api" data-rules-version="2">{a:"h1"}</script></body></html>`,
			"test.htmlclay", "Unsupported rules version", 400},
		{"malformed tag body", `<html><body><script data-rules-name="api" data-rules-version="1">{a:</script></body></html>`,
			"test.htmlclay", "Malformed api rules tag", 400},
		{"rejected selector in tag", `<html><body><script data-rules-name="api" data-rules-version="1">{a:"p:matches(x)"}</script></body></html>`,
			"test.htmlclay", "Invalid selector in api rules tag", 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			writeHome(t, srv, "test.htmlclay", c.page)
			w := getAPI(t, srv, c.path)
			if w.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d: %s", w.Code, c.wantStatus, w.Body.String())
			}
			if got := decodeError(t, w).Error; got != c.wantError {
				t.Errorf("error = %q, want %q", got, c.wantError)
			}
		})
	}
}

func TestDataAPIRejectsBarePathAndUnsupportedExtension(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	writeHome(t, srv, "style.css", "body{}")

	for _, p := range []string{"", "/"} {
		w := getAPI(t, srv, p)
		if w.Code != 400 {
			t.Errorf("/_/api/%q = %d, want 400", p, w.Code)
		}
		if got := decodeError(t, w).Error; got != "Missing path" {
			t.Errorf("/_/api/%q error = %q", p, got)
		}
	}

	w := getAPI(t, srv, "style.css")
	if w.Code != 404 {
		t.Errorf("/_/api/style.css = %d, want 404", w.Code)
	}
	if got := decodeError(t, w).Error; got != "Unsupported file type" {
		t.Errorf("error = %q", got)
	}
}

// Routing, through the real mux. "/_/api/{path...}" does not match bare "/_/api" — ServeMux
// 307-redirects it — which is why the bare route is registered explicitly.
func TestDataAPIRouting(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	registerSubdirPage(t, srv, "sub")
	writeHome(t, srv, "sub/page.html",
		`<html><body><script data-rules-name="api" data-rules-version="1">{a:"h1"}</script><h1>Y</h1></body></html>`)

	cases := []struct{ target, wantBody string }{
		{"/_/api", "Missing path"},
		{"/_/api/", "Missing path"},
		{"/_/api/sub/page.html", `{"a":"Y"}`},
		{"/_/apix", ""}, // falls to the catch-all, which 404s: no such file
	}
	for _, c := range cases {
		t.Run(c.target, func(t *testing.T) {
			req := httptest.NewRequest("GET", c.target, nil)
			req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
			w := httptest.NewRecorder()
			srv.httpServer.Handler.ServeHTTP(w, req)

			if c.wantBody == "" {
				if w.Code == 200 {
					t.Errorf("%s = 200 %s, want the catch-all", c.target, w.Body.String())
				}
				if strings.Contains(w.Body.String(), "Missing path") {
					t.Errorf("%s reached the data API handler", c.target)
				}
				return
			}
			if !strings.Contains(w.Body.String(), c.wantBody) {
				t.Errorf("%s = %d %s, want %q", c.target, w.Code, w.Body.String(), c.wantBody)
			}
		})
	}
}

// The anti-oracle guarantee, stated the way the design actually delivers it: a data request answers
// EXACTLY what the same plain GET answers. That is stronger than any hand-written table of statuses,
// and it holds structurally rather than by inspection, because a data mode replaces only the
// terminal write. If a future change made a data face decide anything about a path on its own, the
// two columns would drift apart here.
func TestDataFaceAnswersExactlyWhatAPlainGetAnswers(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	home := srv.sessions.HomeDir()
	registerSubdirPage(t, srv, "sub")

	internal := filepath.Join(home, "sub", "internal")
	os.MkdirAll(internal, 0755)
	srv.SetInternalDir(internal)
	os.WriteFile(filepath.Join(internal, "secret.html"), []byte("<h1>secret</h1>"), 0644)

	os.MkdirAll(filepath.Join(home, "sub", ".hidden"), 0755)
	os.WriteFile(filepath.Join(home, "sub", ".hidden", "secret.html"), []byte("<h1>secret</h1>"), 0644)
	os.MkdirAll(filepath.Join(home, "sub", "dir.html"), 0755)

	for _, p := range []string{
		"sub/missing.html",
		"sub/.hidden/secret.html",
		"sub/internal/secret.html",
		"sub/dir.html",
		"outside/missing.html",
		"../escape.html",
	} {
		t.Run(p, func(t *testing.T) {
			plain := get(t, srv, "/"+p, p)
			data := get(t, srv, "/"+p+`?data={t:"h1"}`, p)

			if data.Code != plain.Code {
				t.Errorf("?data= answered %d, the plain GET answered %d", data.Code, plain.Code)
			}
			if data.Body.String() != plain.Body.String() {
				t.Errorf("?data= body %q, plain GET body %q", data.Body.String(), plain.Body.String())
			}
			if strings.Contains(data.Body.String(), "secret") {
				t.Errorf("the data face leaked content: %s", data.Body.String())
			}
		})
	}
}

// A data request never auto-registers: registration is per-file state created on a caller's say-so,
// and it can redirect to another site's origin. The request falls through to the stricter asset
// path instead, so a data face can only ever read less than a navigation would.
func TestDataRequestDoesNotAutoRegister(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	ws := filepath.Join(srv.sessions.HomeDir(), "ws")
	if err := os.MkdirAll(ws, 0755); err != nil {
		t.Fatal(err)
	}
	sibling := writeHome(t, srv, "ws/sibling.htmlclay", "<html><body><h1>S</h1></body></html>")

	// Trusted scope and the routing seam are both wired, so the only thing left
	// to stop the registration is the data-request branch itself.
	srv.SetHooks(Hooks{
		TrustedCovers: func(absPath string) bool { return session.EqualOrUnder(absPath, ws) },
		Route: func(string) (string, bool) {
			t.Error("a data request reached the auto-registration seam")
			return "", false
		},
	})
	if err := srv.sessions.InstallTrustedRoot(ws); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", `/ws/sibling.htmlclay?data={t:"h1"}`, nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.SetPathValue("path", "ws/sibling.htmlclay")
	w := httptest.NewRecorder()
	srv.handleServeFile(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != `{"t":"S"}` {
		t.Errorf("body = %s", got)
	}
	if _, registered := srv.sessions.LookupByPath(sibling); registered {
		t.Error("a data request registered the file")
	}
}
