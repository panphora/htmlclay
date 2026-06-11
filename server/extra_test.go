package server

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	if info.Mode().Perm() != 0640 {
		t.Errorf("mode not preserved: got %v, want 0640", info.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	if string(data) != "new content" {
		t.Errorf("content = %q", data)
	}
}

func TestAtomicWriteFileConcurrentNoTorn(t *testing.T) {
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
