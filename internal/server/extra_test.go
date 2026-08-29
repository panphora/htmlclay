package server

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/panphora/htmlclay/internal/logging"
	"github.com/panphora/htmlclay/internal/platform"
	"github.com/panphora/htmlclay/internal/session"
	"github.com/panphora/htmlclay/internal/testutil"
	"github.com/panphora/htmlclay/internal/versions"
	"net"
	"sync/atomic"
	"time"
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

// atomicWriteFile must never let a reader see a half-written file. Staging is what
// guarantees it: the new bytes land in a temp file, and the target changes only by
// rename, which is atomic on POSIX.
//
// This holds a writer at the one instant a torn read would be possible, with every
// replacement byte already on disk and the rename not yet run, and reads the target
// there. The concurrent test below cannot prove this: racing writers that all
// finish still leave one writer's bytes intact at the end, so it passes on a build
// that writes the target directly.
func TestAtomicWriteFilePublishesOnlyByRename(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("rename-over-open is a POSIX guarantee; Windows takes the per-file lock instead")
	}
	path := filepath.Join(t.TempDir(), "f.htmlclay")
	before := []byte("the original contents")
	after := []byte(strings.Repeat("replacement ", 4096))
	if err := os.WriteFile(path, before, 0644); err != nil {
		t.Fatal(err)
	}

	// Rename replaces the directory entry, so a descriptor opened before the swap
	// keeps reading the original inode. An in-place write to the same inode changes
	// what this descriptor sees. That is the difference between publishing the new
	// bytes and overwriting the file a reader already has open, and it is the half
	// that reading the path cannot tell apart.
	held, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	staged := make(chan struct{})
	release := make(chan struct{})
	// Once, because a second concurrent entry would close a closed channel and take
	// the whole package down rather than fail this test.
	var stagedOnce sync.Once
	beforeAtomicReplace = func() {
		stagedOnce.Do(func() { close(staged) })
		<-release
	}
	t.Cleanup(func() { beforeAtomicReplace = nil })

	done := make(chan error, 1)
	go func() { done <- atomicWriteFile(path, after) }()

	testutil.Receive(t, 10*time.Second, "the write to reach the rename", staged)
	mid, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mid, before) {
		if bytes.Equal(mid, after) {
			t.Fatal("the target already held the replacement before the rename, so the write is not staged")
		}
		t.Fatalf("a reader at the rename boundary saw neither the old file nor the new one: %d bytes, want the %d-byte original", len(mid), len(before))
	}
	close(release)

	if err := testutil.Receive(t, 10*time.Second, "the staged write to finish", done); err != nil {
		t.Fatal(err)
	}
	final, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(final, after) {
		t.Fatalf("after the rename the target holds %d bytes, want the %d-byte replacement", len(final), len(after))
	}

	viaHeld, err := io.ReadAll(held)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(viaHeld, before) {
		t.Fatalf("a descriptor opened before the write now reads %d bytes, want the %d-byte original: "+
			"the target was written in place rather than replaced, so a reader holding it open saw the change under them",
			len(viaHeld), len(before))
	}
}

// Concurrent writers must all succeed, leave the target holding exactly one of
// their bodies, and leave no temp files behind. This is a stress test, and it is
// named for what it proves rather than for the torn read, which
// TestAtomicWriteFilePublishesOnlyByRename establishes on its own.
func TestAtomicWriteFileConcurrentWritersLeaveNoTempFiles(t *testing.T) {
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

	// A start barrier, so the writers overlap instead of trickling out behind
	// however long the loop took to spawn them.
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	var completed int
	for c := range contents {
		wg.Add(1)
		go func(body string) {
			defer wg.Done()
			<-start
			err := atomicWriteFile(path, []byte(body))
			mu.Lock()
			defer mu.Unlock()
			if err != nil && firstErr == nil {
				firstErr = err
			}
			if err == nil {
				completed++
			}
		}(c)
	}
	close(start)
	wg.Wait()

	if firstErr != nil {
		t.Fatalf("concurrent atomicWriteFile error: %v", firstErr)
	}
	if completed != n {
		t.Fatalf("%d of %d writers completed", completed, n)
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
	if !strings.Contains(w.Body.String(), `savetoken="`+f.Token+`"`) {
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

	body := `<!DOCTYPE html><html savetoken="x"><body>Mux Save</body></html>`
	req := httptest.NewRequest("POST", "/_/save/"+f.Token, strings.NewReader(body))
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	sameOriginHeaders(req)

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
	if strings.Contains(string(saved), "savetoken") {
		t.Error("token should be stripped on save")
	}
}

// TestSaveJSONBodyRefused verifies spec §3: /_/save has exactly one body shape,
// so a JSON envelope is refused with 415 and nothing reaches the file. Writing
// such an envelope's `content` would be inventing a second shape for the one
// route the spec says has only one.
func TestSaveJSONBodyRefused(t *testing.T) {
	srv, f, content := setupHandlerTest(t)

	jsonBody := `{"content":"<!DOCTYPE html><html><body>From JSON</body></html>","snapshotHtml":"<html>snap</html>"}`
	req := httptest.NewRequest("POST", "/_/save/"+f.Token, strings.NewReader(jsonBody))
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("token", f.Token)

	w := httptest.NewRecorder()
	srv.handleSave(w, req)

	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("expected 415, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "unsupported-media-type") {
		t.Errorf("expected the spec error code in the body, got %s", w.Body.String())
	}
	if string(mustRead(t, f.AbsPath)) != content {
		t.Error("a refused save must leave the file untouched")
	}
}

// TestSaveJSONVariantsRefused covers the two content types the old matcher missed.
// It compared for exactly "application/json" and treated a mime.ParseMediaType
// failure as "not JSON", so a `+json` suffix or a malformed parameter list walked
// straight past the refusal — and then past HasHTMLTag, because the escaped
// document inside the envelope contains the characters "<html". The raw envelope
// was written to the file and answered 200.
func TestSaveJSONVariantsRefused(t *testing.T) {
	for _, ct := range []string{
		"application/hal+json",
		"application/vnd.api+json",
		"APPLICATION/JSON",
		"application/json;;",
		"  application/json ; charset=utf-8",
	} {
		t.Run(ct, func(t *testing.T) {
			srv, f, content := setupHandlerTest(t)

			jsonBody := `{"content":"<!DOCTYPE html><html><body>From JSON</body></html>"}`
			req := httptest.NewRequest("POST", "/_/save/"+f.Token, strings.NewReader(jsonBody))
			req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
			req.Header.Set("Content-Type", ct)
			req.SetPathValue("token", f.Token)

			w := httptest.NewRecorder()
			srv.handleSave(w, req)

			if w.Code != http.StatusUnsupportedMediaType {
				t.Errorf("expected 415, got %d", w.Code)
			}
			if string(mustRead(t, f.AbsPath)) != content {
				t.Error("a refused save must leave the file untouched")
			}
		})
	}
}

// TestSaveTextIsStillAccepted guards the refusal against overreach: text/plain,
// an absent Content-Type, and text/html are all the one body shape this route
// takes, and none of them may be caught by the JSON matcher.
func TestSaveTextIsStillAccepted(t *testing.T) {
	for _, ct := range []string{"text/plain", "text/plain;charset=utf-8", "text/html", ""} {
		t.Run("ct="+ct, func(t *testing.T) {
			srv, f, _ := setupHandlerTest(t)

			doc := "<!DOCTYPE html><html><body>Written</body></html>"
			req := httptest.NewRequest("POST", "/_/save/"+f.Token, strings.NewReader(doc))
			req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
			if ct != "" {
				req.Header.Set("Content-Type", ct)
			}
			req.SetPathValue("token", f.Token)

			w := httptest.NewRecorder()
			srv.handleSave(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
			}
			if !strings.Contains(string(mustRead(t, f.AbsPath)), "Written") {
				t.Error("the document should have reached the file")
			}
		})
	}
}

// TestSaveMalformedJSONBodyRefused pins the refusal to the declared content type
// rather than to whether the body happens to parse. Both are the same answer,
// which is what makes the rule one a client can rely on.
func TestSaveMalformedJSONBodyRefused(t *testing.T) {
	srv, f, content := setupHandlerTest(t)

	req := httptest.NewRequest("POST", "/_/save/"+f.Token, strings.NewReader("{not json"))
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("token", f.Token)

	w := httptest.NewRecorder()
	srv.handleSave(w, req)

	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("expected 415, got %d", w.Code)
	}
	if string(mustRead(t, f.AbsPath)) != content {
		t.Error("file should be unchanged after a refused save")
	}
}

// TestSaveTextBodyAccepted is the other half: the one shape the route does take,
// with the ephemeral token stripped on the way to disk.
func TestSaveTextBodyAccepted(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	body := `<!DOCTYPE html><html savetoken="x"><body>From text</body></html>`
	req := httptest.NewRequest("POST", "/_/save/"+f.Token, strings.NewReader(body))
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.Header.Set("Content-Type", "text/plain")
	req.SetPathValue("token", f.Token)

	w := httptest.NewRecorder()
	srv.handleSave(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	saved := string(mustRead(t, f.AbsPath))
	if !strings.Contains(saved, "From text") {
		t.Error("text body not persisted")
	}
	if strings.Contains(saved, "savetoken") {
		t.Error("token should be stripped on save")
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
	if _, err := fx.srv.sessions.Register(pagePath, session.ViaOsOpen); err != nil {
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

// os.Root follows relative in-root symlinks, so a directory component swapped
// for such a symlink BETWEEN the serve path being resolved and the capability
// open that acts on it can redirect the read into the internal tree while the
// pre-checks saw an entirely benign path. Only the descriptor-bound RealPath
// check can catch that, because it reports where the held inode lives rather
// than what a name currently points at.
//
// The swap is staged, not raced. The old form flipped a directory in a loop
// while firing 400 requests and hoped one of them landed inside the window; a
// run where none did passed with the RealPath check deleted, which is the whole
// thing this test exists to protect.
func TestServeAssetSymlinkSwapIsCaughtByTheDescriptorCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require privileges on windows")
	}
	fx := setupAssetTest(t, "unused.txt", []byte("x"))

	// Inside the opened root, which is what makes the relative symlink followable
	// and the attack worth defending against.
	internal := filepath.Join(fx.home, "assets", "internal")
	if err := os.MkdirAll(internal, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(internal, "config.json"), []byte(`{"secret":"SECRETVALUE"}`), 0644); err != nil {
		t.Fatal(err)
	}
	fx.srv.SetInternalDir(internal)

	swap := filepath.Join(fx.home, "assets", "swap")
	if err := os.MkdirAll(swap, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(swap, "config.json"), []byte("benign"), 0644); err != nil {
		t.Fatal(err)
	}
	// An ordinary asset outside the swapped directory, used as the control below.
	if err := os.WriteFile(filepath.Join(fx.home, "assets", "control.js"), []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}

	swaps := 0
	fx.srv.beforeAssetCapabilityOpen = func() {
		// Every resolution check has now passed, against the benign directory.
		swaps++
		if err := os.RemoveAll(swap); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("internal", swap); err != nil {
			t.Fatal(err)
		}
	}

	w := serveAssetRequest(t, fx, "assets/swap/config.json", nil)

	if swaps != 1 {
		t.Fatalf("the swap ran %d times, want exactly 1: the request did not reach the window", swaps)
	}
	if strings.Contains(w.Body.String(), "SECRETVALUE") {
		t.Fatal("the serve path leaked the internal tree through a swapped directory component")
	}
	if w.Code != 404 {
		t.Fatalf("swapped asset = %d, want 404", w.Code)
	}

	// A control, without which a serve path that refused EVERY asset would satisfy
	// everything above. The refusal has to be the descriptor check noticing the
	// swap, not the handler having stopped serving assets at all.
	fx.srv.beforeAssetCapabilityOpen = nil
	if c := serveAssetRequest(t, fx, "assets/control.js", nil); c.Code != 200 {
		t.Fatalf("the unswapped control asset = %d, want 200: this test proves nothing if every asset is refused", c.Code)
	}
}

// setupBatchAssetTest builds a server whose only opened root is one page's own
// folder, so every asset under the sibling shared/ directory is out of scope and
// has to park for permission. Returns the shared dir and the request-relative
// paths of the assets in it.
func setupBatchAssetTest(t *testing.T, confirm brokerConfirm) (*Server, string, []string) {
	t.Helper()
	home, _ := filepath.EvalSymlinks(t.TempDir())
	pageDir := filepath.Join(home, "work", "review", "fable")
	if err := os.MkdirAll(pageDir, 0755); err != nil {
		t.Fatal(err)
	}
	page := filepath.Join(pageDir, "index.html")
	if err := os.WriteFile(page, []byte("<html><body>fable</body></html>"), 0644); err != nil {
		t.Fatal(err)
	}

	shared := filepath.Join(home, "work", "review", "shared")
	if err := os.MkdirAll(shared, 0755); err != nil {
		t.Fatal(err)
	}
	var rels []string
	for i := 0; i < 8; i++ {
		name := fmt.Sprintf("a%d.js", i)
		if err := os.WriteFile(filepath.Join(shared, name), []byte(fmt.Sprintf("console.log(%d)", i)), 0644); err != nil {
			t.Fatal(err)
		}
		rels = append(rels, filepath.Join("work", "review", "shared", name))
	}

	mgr := newTestManager(t, home)
	if _, err := mgr.Register(page, session.ViaOsOpen); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	srv := New(ln, mgr, logging.NewStdout(), versions.New(t.TempDir()))
	srv.broker.confirm = confirm
	t.Cleanup(func() { srv.broker.shutdown(); srv.hub.shutdown(); srv.watcher.shutdown() })
	return srv, shared, rels
}

type assetResult struct {
	code     int
	hasToken bool
}

// fireAssets issues every request concurrently and returns a channel of results.
// The goroutines never touch t: a failed assertion belongs to the test goroutine.
func fireAssets(srv *Server, rels []string) <-chan assetResult {
	out := make(chan assetResult, len(rels))
	for _, rel := range rels {
		go func(rel string) {
			req := httptest.NewRequest("GET", "/"+rel, nil)
			req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
			req.SetPathValue("path", rel)
			w := httptest.NewRecorder()
			srv.handleServeFile(w, req)
			out <- assetResult{code: w.Code, hasToken: strings.Contains(w.Body.String(), "savetoken")}
		}(rel)
	}
	return out
}

// A burst of out-of-scope asset requests under one common directory produces
// exactly one prompt, and the single Allow resolves all of them with a token-free
// 200.
//
// The batch is held open until all eight have parked, which is the state the
// invariant is about. The old form slept 150ms after the prompt opened and hoped;
// when a request arrived late it found the grant already installed, returned 200
// without ever parking, and every assertion here still passed. That run proved
// nothing about batching.
func TestConcurrentOutOfScopeAssetsResumeTogetherOnAllow(t *testing.T) {
	var prompts atomic.Int32
	srv, shared, rels := setupBatchAssetTest(t, func(string, string, bool) (platform.ConfirmChoice, error) {
		prompts.Add(1)
		return platform.ConfirmAllowOnce, nil
	})
	holdBatchOpen(srv.broker)

	results := fireAssets(srv, rels)
	waitParked(t, srv.broker, len(rels))
	go srv.broker.flush()

	for range rels {
		r := testutil.Receive(t, 10*time.Second, "a parked asset to resume", results)
		if r.code != 200 {
			t.Errorf("a resumed asset returned %d, want 200", r.code)
		}
		if r.hasToken {
			t.Error("a granted asset must be served without a save token")
		}
	}
	if got := prompts.Load(); got != 1 {
		t.Errorf("a burst under one common dir must prompt exactly once, got %d", got)
	}
	// One prompt installs one root, at the batch's common dir and no broader.
	root, _, ok := srv.sessions.AssetRoot(filepath.Join(shared, "a0.js"))
	if !ok {
		t.Fatal("the allow installed no read root")
	}
	if root != shared {
		t.Errorf("installed root = %q, want the batch's common dir %q", root, shared)
	}
}

// The same batch, refused: one prompt, and every parked request is answered 403
// by that single Deny rather than raising its own dialog.
func TestConcurrentOutOfScopeAssetsRefuseTogetherOnDeny(t *testing.T) {
	var prompts atomic.Int32
	srv, shared, rels := setupBatchAssetTest(t, func(string, string, bool) (platform.ConfirmChoice, error) {
		prompts.Add(1)
		return platform.ConfirmDeny, nil
	})
	holdBatchOpen(srv.broker)

	results := fireAssets(srv, rels)
	waitParked(t, srv.broker, len(rels))
	go srv.broker.flush()

	for range rels {
		r := testutil.Receive(t, 10*time.Second, "a parked asset to be refused", results)
		if r.code != 403 {
			t.Errorf("a refused asset returned %d, want 403", r.code)
		}
	}
	if got := prompts.Load(); got != 1 {
		t.Errorf("a denied burst must prompt exactly once, got %d", got)
	}
	if _, _, ok := srv.sessions.AssetRoot(filepath.Join(shared, "a0.js")); ok {
		t.Error("a deny must install no read root")
	}
}
