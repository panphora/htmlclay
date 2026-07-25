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

	"github.com/panphora/htmlclay/htmlutil"
	"github.com/panphora/htmlclay/logging"
	"github.com/panphora/htmlclay/session"
	"github.com/panphora/htmlclay/versions"
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
	srv := New(ln, mgr, logger, versions.New(t.TempDir()))

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

// The token read route reads a registered file by its pinned path. If a local process
// swaps that path for a symlink into htmlclay's internal state, the descriptor-bound
// check in openRegisteredFile must refuse it: the read goes through the held
// descriptor, asks the OS where it really points, and 404s when that is the internal
// tree. Before this route had no internal check at all, so a stable symlink leaked
// config with no race needed.
func TestReadRouteRefusesSymlinkIntoInternal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require privileges on windows")
	}
	homeDir, _ := filepath.EvalSymlinks(t.TempDir())
	internalDir := filepath.Join(homeDir, "internal")
	if err := os.MkdirAll(internalDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(internalDir, "config.json"), []byte("SECRETVALUE"), 0644); err != nil {
		t.Fatal(err)
	}
	page := filepath.Join(homeDir, "page.htmlclay")
	if err := os.WriteFile(page, []byte("<!DOCTYPE html>\n<html><body>ok</body></html>"), 0644); err != nil {
		t.Fatal(err)
	}

	mgr := session.NewManagerWithHome(homeDir)
	f, err := mgr.Register(page)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	srv := New(ln, mgr, logging.NewStdout(), versions.New(t.TempDir()))
	srv.SetInternalDir(internalDir)

	read := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/_/read/"+f.Token, nil)
		req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
		req.SetPathValue("token", f.Token)
		w := httptest.NewRecorder()
		srv.handleRead(w, req)
		return w
	}

	if w := read(); w.Code != 200 {
		t.Fatalf("pre-swap read should be 200, got %d", w.Code)
	}

	// A local process swaps the registered file for a symlink into the internal tree.
	if err := os.Remove(page); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(internalDir, "config.json"), page); err != nil {
		t.Fatal(err)
	}

	w := read()
	if strings.Contains(w.Body.String(), "SECRETVALUE") {
		t.Fatalf("read route leaked internal state through a swapped symlink (code %d)", w.Code)
	}
	if w.Code == 200 {
		t.Errorf("read route should refuse the swapped path, got 200")
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
	srv := New(ln, mgr, logging.NewStdout(), versions.New(t.TempDir()))

	req := httptest.NewRequest("GET", "/other/secret.txt", nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.SetPathValue("path", "other/secret.txt")
	w := httptest.NewRecorder()
	srv.handleServeFile(w, req)

	// An out-of-scope but grantable sibling now parks and prompts; the test
	// confirm denies, so the request resolves to the fixed 403, never served.
	if w.Code != 403 {
		t.Errorf("expected 403, got %d", w.Code)
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
	srv := New(ln, mgr, logging.NewStdout(), versions.New(t.TempDir()))

	req := httptest.NewRequest("GET", "/site/link.txt", nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.SetPathValue("path", "site/link.txt")
	w := httptest.NewRecorder()
	srv.handleServeFile(w, req)

	// The symlink resolves to an out-of-scope in-home file, so it is treated as
	// any other out-of-scope read: parked, prompted, and (test-denied) refused
	// with 403. The escape target is never served without an explicit grant.
	if w.Code != 403 {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

// An asset inside an opened folder that does not exist returns 404 promptly. The
// missing path fails resolution before the broker is ever consulted, so a missing
// in-scope file is never parked and never answered with the out-of-scope 403.
func TestMissingInScopeAssetReturns404(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	registerSubdirPage(t, srv, "site")

	req := httptest.NewRequest("GET", "/site/missing.css", nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.SetPathValue("path", "site/missing.css")
	w := httptest.NewRecorder()
	srv.handleServeFile(w, req)

	if w.Code != 404 {
		t.Errorf("a missing in-scope asset must 404, not park or 403: got %d", w.Code)
	}
}

// A symlink inside a served tree that resolves OUTSIDE home is refused: the
// resolved target leaves home, so it is never served whatever the read roots say.
// The existing symlink test covers an in-home, out-of-scope target; this covers
// the escape past home entirely.
func TestServeAssetSymlinkEscapeOutsideHomeRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require privileges on windows")
	}
	homeDir, _ := filepath.EvalSymlinks(t.TempDir())
	outside, _ := filepath.EvalSymlinks(t.TempDir())
	os.MkdirAll(filepath.Join(homeDir, "site"), 0755)
	pagePath := filepath.Join(homeDir, "site", "page.htmlclay")
	os.WriteFile(pagePath, []byte("<!DOCTYPE html>\n<html><body>hi</body></html>"), 0644)
	secret := filepath.Join(outside, "secret.txt")
	// Assert the fixture and the escaping symlink actually exist, or a failed
	// creation would 404 for a missing file and pass this denial test vacuously.
	if err := os.WriteFile(secret, []byte("outside-secret"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(homeDir, "site", "escape.txt")); err != nil {
		t.Fatal(err)
	}

	mgr := session.NewManagerWithHome(homeDir)
	if _, err := mgr.Register(pagePath); err != nil {
		t.Fatalf("register: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	srv := New(ln, mgr, logging.NewStdout(), versions.New(t.TempDir()))

	req := httptest.NewRequest("GET", "/site/escape.txt", nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.SetPathValue("path", "site/escape.txt")
	w := httptest.NewRecorder()
	srv.handleServeFile(w, req)

	if w.Code != 404 {
		t.Errorf("a symlink escaping home must be refused with 404, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "outside-secret") {
		t.Error("the out-of-home target must never be served")
	}
}

// A dotfile under an opened folder is never served as an asset: hidden components
// are refused before authorization, so a page cannot read its own folder's .env,
// .git, or .ssh even though the folder is in scope.
func TestServeAssetDotfileRefused(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	dir := registerSubdirPage(t, srv, "site")
	// Assert the dotfile exists, or a failed write would 404 as a missing file and
	// pass this denial test without exercising the hidden-component rule.
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=1"), 0644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/site/.env", nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.SetPathValue("path", "site/.env")
	w := httptest.NewRecorder()
	srv.handleServeFile(w, req)

	if w.Code != 404 {
		t.Errorf("a dotfile must be refused, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "SECRET") {
		t.Error("a dotfile's contents must never be served")
	}
}

// A path inside the versions/backup tree is refused on the serve path even when an
// opened root would otherwise authorize it. Backups are internal state, denied
// structurally before any read root is consulted; a sibling non-versions asset
// under the same root still serves, so the denial is scoped to the versions tree.
func TestVersionsTreeRefusedOnServePath(t *testing.T) {
	homeDir, _ := filepath.EvalSymlinks(t.TempDir())
	os.MkdirAll(filepath.Join(homeDir, "site"), 0755)
	pagePath := filepath.Join(homeDir, "site", "page.htmlclay")
	os.WriteFile(pagePath, []byte("<!DOCTYPE html>\n<html><body>hi</body></html>"), 0644)
	os.WriteFile(filepath.Join(homeDir, "site", "ok.css"), []byte("body{}"), 0644)

	// The store lives inside the opened root, so the opened root covers it; the
	// serve path must still refuse the whole versions subtree.
	storeDir := filepath.Join(homeDir, "site", "versions")
	// Assert the backup fixture exists, or a failed write would 404 as a missing file
	// and pass this denial test without exercising the versions-tree refusal.
	if err := os.MkdirAll(storeDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storeDir, "backup.html"), []byte("<html>backup</html>"), 0644); err != nil {
		t.Fatal(err)
	}
	store := versions.New(storeDir)

	mgr := session.NewManagerWithHome(homeDir)
	if _, err := mgr.Register(pagePath); err != nil { // opened root = home/site
		t.Fatalf("register: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	srv := New(ln, mgr, logging.NewStdout(), store)

	// A normal in-scope asset serves, proving the opened root is live.
	reqOK := httptest.NewRequest("GET", "/site/ok.css", nil)
	reqOK.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	reqOK.SetPathValue("path", "site/ok.css")
	wOK := httptest.NewRecorder()
	srv.handleServeFile(wOK, reqOK)
	if wOK.Code != 200 {
		t.Fatalf("in-scope asset should serve: got %d", wOK.Code)
	}

	// The versions subtree under the same opened root is refused.
	req := httptest.NewRequest("GET", "/site/versions/backup.html", nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.SetPathValue("path", "site/versions/backup.html")
	w := httptest.NewRecorder()
	srv.handleServeFile(w, req)
	if w.Code != 404 {
		t.Errorf("a path inside the versions tree must be refused, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "backup") {
		t.Error("versions-tree contents must never be served")
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
	srv := New(ln, mgr, logging.NewStdout(), versions.New(t.TempDir()))

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

// The host never writes an identity to disk. The tracked id rides in the response
// bytes only; it reaches the file when the client's own save carries it back.
func TestServeHTMLClayFileInjectsIDWithoutWritingDisk(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	req := httptest.NewRequest("GET", "/test.htmlclay", nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.SetPathValue("path", "test.htmlclay")
	w := httptest.NewRecorder()
	srv.handleServeFile(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	served := htmlutil.ReadHTMLClayID(w.Body.Bytes())
	if !versions.IsCanonicalUUID(served) {
		t.Errorf("the response carries no canonical htmlclayid: %q", served)
	}
	onDisk, _ := os.ReadFile(f.AbsPath)
	if strings.Contains(string(onDisk), "htmlclayid=") {
		t.Error("serving wrote an htmlclayid to disk")
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

	// A home-root sibling's only grantable ancestor is home itself, which is
	// never offered, so the broker denies without prompting: fixed 403.
	if w.Code != 403 {
		t.Errorf("expected 403 for home-root sibling, got %d", w.Code)
	}
}
