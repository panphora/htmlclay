package server

import (
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
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

func registerSubdirPage(t *testing.T, srv *Server, dirName string) string {
	t.Helper()
	home := srv.sessions.HomeDir()
	dir := filepath.Join(home, dirName)
	os.MkdirAll(dir, 0755)
	page := filepath.Join(dir, "page.htmlclay")
	os.WriteFile(page, []byte("<!DOCTYPE html>\n<html><body>sub</body></html>"), 0644)
	if _, err := srv.sessions.Register(page); err != nil {
		t.Fatalf("register subdir page: %v", err)
	}
	return dir
}

func TestServeFile(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	req := httptest.NewRequest("GET", "/test.htmlclay", nil)
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

	req := httptest.NewRequest("GET", "/nonexistent.htmlclay", nil)
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

	req := httptest.NewRequest("GET", "/../../../etc/passwd", nil)
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

	req := httptest.NewRequest("GET", "/_/read/"+f.Token, nil)
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

	req := httptest.NewRequest("GET", "/_/read/invalid-token", nil)
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
	req := httptest.NewRequest("POST", "/_/save/"+f.Token, strings.NewReader(newContent))
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

	req := httptest.NewRequest("POST", "/_/save/bad-token", strings.NewReader("test"))
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

	req := httptest.NewRequest("GET", "/_/meta/"+f.Token, nil)
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

	req := httptest.NewRequest("GET", "/_/meta/bad-token", nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.SetPathValue("token", "bad-token")

	w := httptest.NewRecorder()
	srv.handleMeta(w, req)

	if w.Code != 401 {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestServeAssetUnderOpenedDir(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	dir := registerSubdirPage(t, srv, "site")
	os.WriteFile(filepath.Join(dir, "style.css"), []byte("body { color: red }"), 0644)

	req := httptest.NewRequest("GET", "/site/style.css", nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.SetPathValue("path", "site/style.css")
	w := httptest.NewRecorder()
	srv.handleServeFile(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/css") {
		t.Errorf("Content-Type = %q, want text/css", ct)
	}
	if w.Body.String() != "body { color: red }" {
		t.Errorf("unexpected body %q", w.Body.String())
	}
}

func TestServeAssetInSubdir(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	dir := registerSubdirPage(t, srv, "site")
	os.MkdirAll(filepath.Join(dir, "img"), 0755)
	os.WriteFile(filepath.Join(dir, "img", "logo.png"), []byte("fakepng"), 0644)

	req := httptest.NewRequest("GET", "/site/img/logo.png", nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.SetPathValue("path", "site/img/logo.png")
	w := httptest.NewRecorder()
	srv.handleServeFile(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "fakepng" {
		t.Errorf("unexpected body %q", w.Body.String())
	}
}

func TestServeAssetOutsideOpenedDirs(t *testing.T) {
	homeDir, _ := filepath.EvalSymlinks(t.TempDir())
	os.MkdirAll(filepath.Join(homeDir, "site"), 0755)
	os.MkdirAll(filepath.Join(homeDir, "other"), 0755)
	pagePath := filepath.Join(homeDir, "site", "page.htmlclay")
	os.WriteFile(pagePath, []byte("<!DOCTYPE html>\n<html><body>hi</body></html>"), 0644)
	os.WriteFile(filepath.Join(homeDir, "other", "secret.txt"), []byte("secret"), 0644)

	mgr := session.NewManagerWithHome(homeDir)
	if _, err := mgr.Register(pagePath); err != nil {
		t.Fatalf("register: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	srv := New(ln, mgr, logging.NewStdout())

	req := httptest.NewRequest("GET", "/other/secret.txt", nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.SetPathValue("path", "other/secret.txt")
	w := httptest.NewRecorder()
	srv.handleServeFile(w, req)

	if w.Code != 404 {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestServeAssetDirectoryRequest(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	dir := registerSubdirPage(t, srv, "site")
	os.MkdirAll(filepath.Join(dir, "img"), 0755)

	req := httptest.NewRequest("GET", "/site/img", nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.SetPathValue("path", "site/img")
	w := httptest.NewRecorder()
	srv.handleServeFile(w, req)

	if w.Code != 404 {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestServeAssetLinkedPageNoToken(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	dir := registerSubdirPage(t, srv, "site")
	linked := filepath.Join(dir, "linked.html")
	content := "<!DOCTYPE html>\n<html><body>linked</body></html>"
	os.WriteFile(linked, []byte(content), 0644)

	req := httptest.NewRequest("GET", "/site/linked.html", nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.SetPathValue("path", "site/linked.html")
	w := httptest.NewRecorder()
	srv.handleServeFile(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "htmlclaytoken") {
		t.Error("linked page must not receive a save token")
	}
	onDisk, _ := os.ReadFile(linked)
	if string(onDisk) != content {
		t.Error("linked page was modified on disk")
	}
}

func TestServeAssetSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require privileges on windows")
	}
	homeDir, _ := filepath.EvalSymlinks(t.TempDir())
	os.MkdirAll(filepath.Join(homeDir, "site"), 0755)
	os.MkdirAll(filepath.Join(homeDir, "private"), 0755)
	pagePath := filepath.Join(homeDir, "site", "page.htmlclay")
	os.WriteFile(pagePath, []byte("<!DOCTYPE html>\n<html><body>hi</body></html>"), 0644)
	secret := filepath.Join(homeDir, "private", "secret.txt")
	os.WriteFile(secret, []byte("secret"), 0644)
	os.Symlink(secret, filepath.Join(homeDir, "site", "link.txt"))

	mgr := session.NewManagerWithHome(homeDir)
	if _, err := mgr.Register(pagePath); err != nil {
		t.Fatalf("register: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	srv := New(ln, mgr, logging.NewStdout())

	req := httptest.NewRequest("GET", "/site/link.txt", nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.SetPathValue("path", "site/link.txt")
	w := httptest.NewRecorder()
	srv.handleServeFile(w, req)

	if w.Code != 404 {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestServeOpenedHTMLFileNotMutated(t *testing.T) {
	homeDir, _ := filepath.EvalSymlinks(t.TempDir())
	pagePath := filepath.Join(homeDir, "page.html")
	content := "<!DOCTYPE html>\n<html><body>hi</body></html>"
	os.WriteFile(pagePath, []byte(content), 0644)

	mgr := session.NewManagerWithHome(homeDir)
	f, err := mgr.Register(pagePath)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	srv := New(ln, mgr, logging.NewStdout())

	req := httptest.NewRequest("GET", "/page.html", nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.SetPathValue("path", "page.html")
	w := httptest.NewRecorder()
	srv.handleServeFile(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `htmlclaytoken="`+f.Token+`"`) {
		t.Error("opened html file should receive a save token")
	}
	onDisk, _ := os.ReadFile(pagePath)
	if string(onDisk) != content {
		t.Errorf("plain .html file was modified on disk:\n%s", onDisk)
	}
}

func TestServeHTMLClayFilePersistsID(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	req := httptest.NewRequest("GET", "/test.htmlclay", nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.SetPathValue("path", "test.htmlclay")
	w := httptest.NewRecorder()
	srv.handleServeFile(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	onDisk, _ := os.ReadFile(f.AbsPath)
	if !strings.Contains(string(onDisk), "htmlclayid=") {
		t.Error(".htmlclay file should get a persistent htmlclayid on disk")
	}
}

func TestServeAssetHomeRootNotExposed(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	home := srv.sessions.HomeDir()
	os.WriteFile(filepath.Join(home, "secret.txt"), []byte("secret"), 0644)

	req := httptest.NewRequest("GET", "/secret.txt", nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.SetPathValue("path", "secret.txt")
	w := httptest.NewRecorder()
	srv.handleServeFile(w, req)

	if w.Code != 404 {
		t.Errorf("expected 404 for home-root sibling, got %d", w.Code)
	}
}
