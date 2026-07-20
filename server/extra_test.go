package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestExtractFilePath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"test.htmlclay", "test.htmlclay"},
		{"a/b/test.htmlclay", "a/b/test.htmlclay"},
		{"test.htmlclay/sub/path", "test.htmlclay"},
		{"test.html", "test.html"},
		{"test.html/x", "test.html"},
		{"App.HTMLClay/route", "App.HTMLClay"},
		{"Page.HTML/x", "Page.HTML"},
		{"plain", "plain"},
	}
	for _, c := range cases {
		if got := extractFilePath(c.in); got != c.want {
			t.Errorf("extractFilePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestAtomicWriteFilePreservesMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.htmlclay")
	if err := os.WriteFile(path, []byte("old"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}

	if err := atomicWriteFile(path, []byte("new content")); err != nil {
		t.Fatalf("atomicWriteFile error: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0640 {
		t.Errorf("mode not preserved: got %v, want 0640", info.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	if string(data) != "new content" {
		t.Errorf("content = %q", data)
	}
}

func TestAtomicWriteFileConcurrentNoTorn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("atomicWriteFile always runs under the per-file lock in the app; lock-free concurrent rename-over-open is a POSIX-only guarantee")
	}
	path := filepath.Join(t.TempDir(), "f.htmlclay")
	if err := os.WriteFile(path, []byte("init"), 0644); err != nil {
		t.Fatal(err)
	}

	const n = 24
	contents := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		contents[fmt.Sprintf("content-%02d-%s", i, strings.Repeat("x", 4096))] = true
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for c := range contents {
		wg.Add(1)
		go func(body string) {
			defer wg.Done()
			if err := atomicWriteFile(path, []byte(body)); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		}(c)
	}
	wg.Wait()

	if firstErr != nil {
		t.Fatalf("concurrent atomicWriteFile error: %v", firstErr)
	}

	final, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !contents[string(final)] {
		t.Errorf("final file is torn or unexpected: %q", string(final)[:32])
	}

	// No leftover temp files in the directory.
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".htmlclay-save-") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

func TestSaveEmptyBodyRejected(t *testing.T) {
	srv, f, content := setupHandlerTest(t)

	req := httptest.NewRequest("POST", "/_/save/"+f.Token, strings.NewReader(""))
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.SetPathValue("token", f.Token)

	w := httptest.NewRecorder()
	srv.handleSave(w, req)

	if w.Code != 400 {
		t.Errorf("expected 400 for empty body, got %d", w.Code)
	}
	saved, _ := os.ReadFile(f.AbsPath)
	if string(saved) != content {
		t.Error("file should be unchanged after a rejected empty save")
	}
}

func TestSaveNonHTMLBodyRejected(t *testing.T) {
	srv, f, content := setupHandlerTest(t)

	req := httptest.NewRequest("POST", "/_/save/"+f.Token, strings.NewReader("<p>Hello</p>"))
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.SetPathValue("token", f.Token)

	w := httptest.NewRecorder()
	srv.handleSave(w, req)

	if w.Code != 400 {
		t.Errorf("expected 400 for non-HTML body, got %d", w.Code)
	}
	if string(mustRead(t, f.AbsPath)) != content {
		t.Error("file should be unchanged after a rejected non-HTML save")
	}
}

func TestCrossSiteRequestRejected(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	req := httptest.NewRequest("POST", "/_/save/"+f.Token, strings.NewReader("<html></html>"))
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.Header.Set("Sec-Fetch-Site", "cross-site")

	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != 403 {
		t.Errorf("expected 403 for cross-site request, got %d", w.Code)
	}
}

// TestServeFileThroughMux verifies content is served at the top level (no /f/
// prefix) through the real mux, exercising the /{path...} catch-all route.
func TestServeFileThroughMux(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	req := httptest.NewRequest("GET", "/"+f.RelPath, nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)

	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200 serving file at top level, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `htmlclaytoken="`+f.Token+`"`) {
		t.Error("served file missing token attribute")
	}
}

// TestReadThroughMux verifies GET /_/read/{token} routes to the read handler
// and is not swallowed by the same-method /{path...} catch-all (the one case
// where two GET patterns overlap and mux precedence must pick the literal one).
func TestReadThroughMux(t *testing.T) {
	srv, f, content := setupHandlerTest(t)

	req := httptest.NewRequest("GET", "/_/read/"+f.Token, nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)

	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != content {
		t.Error("read through mux did not return raw file content")
	}
}

// TestSaveThroughMux verifies POST /_/save/{token} routes to the save handler
// through the real mux and is not swallowed by the /{path...} catch-all.
func TestSaveThroughMux(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	body := `<!DOCTYPE html><html htmlclaytoken="x"><body>Mux Save</body></html>`
	req := httptest.NewRequest("POST", "/_/save/"+f.Token, strings.NewReader(body))
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)

	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"msgType":"success"`) {
		t.Errorf("response missing msgType, got %s", w.Body.String())
	}
	saved, _ := os.ReadFile(f.AbsPath)
	if !strings.Contains(string(saved), "Mux Save") {
		t.Error("save through mux did not write content")
	}
	if strings.Contains(string(saved), "htmlclaytoken") {
		t.Error("token should be stripped on save")
	}
}

// TestSaveJSONBody verifies that an application/json body {content, snapshotHtml}
// persists only content, mirroring hyperclayjs's live-sync save shape.
func TestSaveJSONBody(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	jsonBody := `{"content":"<!DOCTYPE html><html htmlclaytoken=\"x\"><body>From JSON</body></html>","snapshotHtml":"<html>snap</html>"}`
	req := httptest.NewRequest("POST", "/_/save/"+f.Token, strings.NewReader(jsonBody))
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("token", f.Token)

	w := httptest.NewRecorder()
	srv.handleSave(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	saved := string(mustRead(t, f.AbsPath))
	if !strings.Contains(saved, "From JSON") {
		t.Error("content from JSON body not persisted")
	}
	if strings.Contains(saved, "snap") {
		t.Error("snapshotHtml wrapper leaked to disk")
	}
	if strings.Contains(saved, "htmlclaytoken") {
		t.Error("token should be stripped from JSON content")
	}
}

// TestSaveInvalidJSONBody rejects a malformed application/json body without
// touching the file.
func TestSaveInvalidJSONBody(t *testing.T) {
	srv, f, content := setupHandlerTest(t)

	req := httptest.NewRequest("POST", "/_/save/"+f.Token, strings.NewReader("{not json"))
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("token", f.Token)

	w := httptest.NewRecorder()
	srv.handleSave(w, req)

	if w.Code != 400 {
		t.Errorf("expected 400 for invalid json, got %d", w.Code)
	}
	if string(mustRead(t, f.AbsPath)) != content {
		t.Error("file should be unchanged after a rejected invalid-json save")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

// B0: edit mode via cookie, matching hyperclay-local. Both clients fall back to
// exactly this cookie, read synchronously from document.cookie, and the response
// cookie arrives before scripts execute.
func TestServeFileSetsEditModeCookie(t *testing.T) {
	fx := setupFileTest(t, "notes.htmlclay", page("hi"))
	w := fx.serve(t, "notes.htmlclay")

	if w.Code != 200 {
		t.Fatalf("serve: %d", w.Code)
	}

	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "isAdminOfCurrentResource" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("the edit-mode cookie was not set, so savePageCore bails with Not in edit mode")
	}
	if cookie.Value != "true" {
		t.Fatalf("cookie value = %q", cookie.Value)
	}
	if cookie.Path != "/" {
		t.Fatalf("cookie path = %q, want /", cookie.Path)
	}
	if cookie.Domain != "" {
		t.Fatalf("cookie is not host-only, Domain = %q", cookie.Domain)
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie SameSite = %v, want Lax", cookie.SameSite)
	}
	if cookie.HttpOnly {
		t.Fatal("cookie is HttpOnly, so document.cookie cannot read it")
	}
	if cookie.Secure {
		t.Fatal("cookie is Secure, which a plain-http localhost origin cannot satisfy")
	}
}

// B6: tokens are per-process, so any cache validator on the document means a 304
// after a restart hands back a dead token and every save 401s silently.
func TestTokenBearingDocumentIsNoStore(t *testing.T) {
	fx := setupFileTest(t, "notes.htmlclay", page("hi"))
	w := fx.serve(t, "notes.htmlclay")

	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("document Cache-Control = %q, want no-store", got)
	}
	if w.Header().Get("ETag") != "" {
		t.Fatal("the token-bearing document carries an ETag")
	}
	if w.Header().Get("Last-Modified") != "" {
		t.Fatal("the token-bearing document carries a Last-Modified validator")
	}
}

func serveAssetRequest(t *testing.T, fx *fileFixture, rel string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/"+rel, nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", fx.srv.port)
	req.SetPathValue("path", rel)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	fx.srv.handleServeFile(w, req)
	return w
}

func setupAssetTest(t *testing.T, name string, body []byte) *fileFixture {
	t.Helper()
	fx := setupFileTest(t, "index.htmlclay", page("app"))
	assetDir := filepath.Join(fx.home, "assets")
	if err := os.MkdirAll(assetDir, 0755); err != nil {
		t.Fatal(err)
	}
	pagePath := filepath.Join(assetDir, "page.htmlclay")
	if err := os.WriteFile(pagePath, []byte(page("sub")), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.srv.sessions.Register(pagePath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assetDir, name), body, 0644); err != nil {
		t.Fatal(err)
	}
	return fx
}

// B7: the bug that started the thread. htmlclay served a .br sidecar without
// Content-Encoding, and the client read compressed bytes as a mesh header.
func TestBrotliSidecarCarriesContentEncoding(t *testing.T) {
	compressed := []byte{0x1b, 0x2e, 0x00, 0xf8, 0x25, 0x14}
	fx := setupAssetTest(t, "mesh.glb.br", compressed)

	w := serveAssetRequest(t, fx, "assets/mesh.glb.br", nil)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if got := w.Header().Get("Content-Encoding"); got != "br" {
		t.Fatalf("Content-Encoding = %q, want br", got)
	}
	if got := w.Body.Bytes(); string(got) != string(compressed) {
		t.Fatalf("body was altered: %v", got)
	}
	// Content-Type comes from the inner extension, not from sniffing the
	// compressed bytes.
	if ct := w.Header().Get("Content-Type"); strings.Contains(ct, "text/plain") {
		t.Fatalf("Content-Type %q was sniffed from the compressed bytes", ct)
	}
}

func TestGzipSidecarCarriesContentEncoding(t *testing.T) {
	compressed := []byte{0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00}
	fx := setupAssetTest(t, "bundle.js.gz", compressed)

	w := serveAssetRequest(t, fx, "assets/bundle.js.gz", nil)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if got := w.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Fatalf("Content-Type = %q, want it derived from the inner .js", ct)
	}
}

// A Range header on an encoded sidecar is declined rather than honored. Accept-
// Ranges is never advertised for these, so the request is unsolicited, and the
// full representation is returned with its encoding intact. Dropping
// Content-Encoding to satisfy a Range would reintroduce the original bug.
func TestEncodedSidecarDeclinesRange(t *testing.T) {
	compressed := []byte{0x1b, 0x2e, 0x00, 0xf8, 0x25, 0x14}
	fx := setupAssetTest(t, "mesh.glb.br", compressed)

	w := serveAssetRequest(t, fx, "assets/mesh.glb.br", map[string]string{"Range": "bytes=0-2"})

	if w.Code == http.StatusPartialContent {
		t.Fatal("a byte range was served for an encoded representation")
	}
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if got := w.Header().Get("Accept-Ranges"); got != "none" {
		t.Fatalf("Accept-Ranges = %q, want none", got)
	}
	if got := w.Header().Get("Content-Encoding"); got != "br" {
		t.Fatalf("Content-Encoding was dropped to satisfy a Range: %q", got)
	}
	if w.Body.Len() != len(compressed) {
		t.Fatalf("body length %d, want the full %d", w.Body.Len(), len(compressed))
	}
}

// A plain asset gets no sidecar treatment: no generic negotiation.
func TestPlainAssetHasNoContentEncoding(t *testing.T) {
	fx := setupAssetTest(t, "style.css", []byte("body{color:red}"))

	w := serveAssetRequest(t, fx, "assets/style.css", nil)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if got := w.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("a plain asset was labelled %q", got)
	}
}

// B8: assets revalidate rather than being served from cache blindly.
func TestAssetsCarryNoCacheAndETag(t *testing.T) {
	fx := setupAssetTest(t, "style.css", []byte("body{color:red}"))

	w := serveAssetRequest(t, fx, "assets/style.css", nil)
	if got := w.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("asset Cache-Control = %q, want no-cache", got)
	}
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("asset has no ETag to revalidate against")
	}

	again := serveAssetRequest(t, fx, "assets/style.css", map[string]string{"If-None-Match": etag})
	if again.Code != http.StatusNotModified {
		t.Fatalf("revalidation returned %d, want 304", again.Code)
	}
}

func TestEncodedSidecarRevalidatesWithETag(t *testing.T) {
	fx := setupAssetTest(t, "bundle.js.gz", []byte{0x1f, 0x8b, 0x08})

	w := serveAssetRequest(t, fx, "assets/bundle.js.gz", nil)
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("encoded sidecar has no ETag")
	}

	again := serveAssetRequest(t, fx, "assets/bundle.js.gz", map[string]string{"If-None-Match": etag})
	if again.Code != http.StatusNotModified {
		t.Fatalf("revalidation returned %d, want 304", again.Code)
	}
}

func TestEtagMatches(t *testing.T) {
	cases := []struct {
		header, etag string
		want         bool
	}{
		{`"abc-1"`, `"abc-1"`, true},
		{`W/"abc-1"`, `"abc-1"`, true},
		{`"x", "abc-1"`, `"abc-1"`, true},
		{`*`, `"abc-1"`, true},
		{`"other"`, `"abc-1"`, false},
		{``, `"abc-1"`, false},
	}
	for _, c := range cases {
		if got := etagMatches(c.header, c.etag); got != c.want {
			t.Errorf("etagMatches(%q, %q) = %v, want %v", c.header, c.etag, got, c.want)
		}
	}
}

func TestSidecarEncoding(t *testing.T) {
	cases := []struct {
		name, encoding, inner string
		ok                    bool
	}{
		{"mesh.glb.br", "br", "mesh.glb", true},
		{"bundle.js.gz", "gzip", "bundle.js", true},
		{"style.css", "", "", false},
		{"archive.tar", "", "", false},
		{"notes.brotli", "", "", false},
	}
	for _, c := range cases {
		enc, inner, ok := sidecarEncoding(c.name)
		if ok != c.ok || enc != c.encoding || inner != c.inner {
			t.Errorf("sidecarEncoding(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.name, enc, inner, ok, c.encoding, c.inner, c.ok)
		}
	}
}

// The six-connection HTTP/1.1 cap is a real, documented constraint of the
// transport: an SSE stream holds one connection for the life of the page, so a
// seventh request queues behind six open tabs.
//
// Deliberately NOT exercised with Go's http.Client, which has no per-host
// connection cap and therefore cannot reproduce the limit at all. Driving real
// tabs with a browser is the only honest way to observe it.
func TestTabLimitIsDocumented(t *testing.T) {
	if maxUsefulTabs != 6 {
		t.Fatalf("documented tab limit = %d, want the browser's 6", maxUsefulTabs)
	}
}
