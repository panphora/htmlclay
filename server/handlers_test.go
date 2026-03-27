package server

import (
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/panphora/htmlclay/logging"
	"github.com/panphora/htmlclay/session"
)

func setupHandlerTest(t *testing.T) (*Server, *session.File, string) {
	t.Helper()

	homeDir, _ := filepath.EvalSymlinks(t.TempDir())
	filePath := filepath.Join(homeDir, "test.htmlclay")
	content := "<!DOCTYPE html>\n<html lang=\"en\">\n<head><title>Test</title></head>\n<body><p>Hello</p></body>\n</html>"
	os.WriteFile(filePath, []byte(content), 0644)

	mgr := session.NewManagerWithHome(homeDir)
	f, err := mgr.Register(filePath)
	if err != nil {
		t.Fatalf("register error: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	logger := logging.NewStdout()
	srv := New(ln, mgr, logger)

	return srv, f, content
}

func TestServeFile(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	req := httptest.NewRequest("GET", "/f/test.htmlclay", nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.SetPathValue("path", "test.htmlclay")

	w := httptest.NewRecorder()
	srv.handleServeFile(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, `htmlclaytoken="`+f.Token+`"`) {
		t.Fatal("response missing htmlclaytoken attribute")
	}
	if !strings.Contains(body, "<p>Hello</p>") {
		t.Fatal("response missing original content")
	}
}

func TestServeFileNotRegistered(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)

	req := httptest.NewRequest("GET", "/f/nonexistent.htmlclay", nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.SetPathValue("path", "nonexistent.htmlclay")

	w := httptest.NewRecorder()
	srv.handleServeFile(w, req)

	if w.Code != 404 {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestServeFilePathTraversal(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)

	req := httptest.NewRequest("GET", "/f/../../../etc/passwd", nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.SetPathValue("path", "../../../etc/passwd")

	w := httptest.NewRecorder()
	srv.handleServeFile(w, req)

	if w.Code != 404 {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestReadValid(t *testing.T) {
	srv, f, content := setupHandlerTest(t)

	req := httptest.NewRequest("GET", "/read/"+f.Token, nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.SetPathValue("token", f.Token)

	w := httptest.NewRecorder()
	srv.handleRead(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != content {
		t.Error("response doesn't match raw file content")
	}
}

func TestReadInvalidToken(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)

	req := httptest.NewRequest("GET", "/read/invalid-token", nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.SetPathValue("token", "invalid-token")

	w := httptest.NewRecorder()
	srv.handleRead(w, req)

	if w.Code != 401 {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestSaveValid(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	newContent := `<!DOCTYPE html><html htmlclaytoken="tok"><body>Updated!</body></html>`
	req := httptest.NewRequest("POST", "/save/"+f.Token, strings.NewReader(newContent))
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.SetPathValue("token", f.Token)

	w := httptest.NewRecorder()
	srv.handleSave(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	if !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Error("expected ok:true in response")
	}

	saved, err := os.ReadFile(f.AbsPath)
	if err != nil {
		t.Fatalf("error reading saved file: %v", err)
	}
	if strings.Contains(string(saved), "htmlclaytoken") {
		t.Error("saved file should not contain htmlclaytoken")
	}
	if !strings.Contains(string(saved), "Updated!") {
		t.Error("saved file should contain new content")
	}
}

func TestSaveInvalidToken(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)

	req := httptest.NewRequest("POST", "/save/bad-token", strings.NewReader("test"))
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.SetPathValue("token", "bad-token")

	w := httptest.NewRecorder()
	srv.handleSave(w, req)

	if w.Code != 401 {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestMetaValid(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	req := httptest.NewRequest("GET", "/meta/"+f.Token, nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.SetPathValue("token", f.Token)

	w := httptest.NewRecorder()
	srv.handleMeta(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, `"name":"test.htmlclay"`) {
		t.Errorf("response missing name, got: %s", body)
	}
	if !strings.Contains(body, `"size":`) {
		t.Error("response missing size")
	}
}

func TestMetaInvalidToken(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)

	req := httptest.NewRequest("GET", "/meta/bad-token", nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.SetPathValue("token", "bad-token")

	w := httptest.NewRecorder()
	srv.handleMeta(w, req)

	if w.Code != 401 {
		t.Errorf("expected 401, got %d", w.Code)
	}
}
