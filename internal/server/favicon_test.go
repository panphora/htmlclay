package server

import (
	"bytes"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func getPath(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/"+path, nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.SetPathValue("path", path)
	w := httptest.NewRecorder()
	srv.handleServeFile(w, req)
	return w
}

// Before this, both paths resolved to $HOME, which can never become a read root,
// so every document window asked for an icon and got a 403.
func TestRootFaviconPathsServeTheAppIcon(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)

	for _, tc := range []struct {
		path  string
		ctype string
		want  []byte
	}{
		{"favicon.ico", "image/x-icon", faviconICO},
		{"favicon.svg", "image/svg+xml", faviconSVG},
	} {
		w := getPath(t, srv, tc.path)
		if w.Code != 200 {
			t.Errorf("%s: expected 200, got %d", tc.path, w.Code)
			continue
		}
		if got := w.Header().Get("Content-Type"); got != tc.ctype {
			t.Errorf("%s: Content-Type %q, want %q", tc.path, got, tc.ctype)
		}
		if len(tc.want) == 0 {
			t.Fatalf("%s: embedded asset is empty", tc.path)
		}
		if !bytes.Equal(w.Body.Bytes(), tc.want) {
			t.Errorf("%s: body is not the embedded asset (%d bytes vs %d)", tc.path, w.Body.Len(), len(tc.want))
		}
	}
}

// The answer must not depend on the filesystem, or it becomes the existence
// oracle every other branch of serveFile is written to avoid.
func TestRootFaviconAnswerIsConstant(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)

	before := getPath(t, srv, "favicon.ico")

	real := filepath.Join(srv.sessions.HomeDir(), "favicon.ico")
	if err := os.WriteFile(real, []byte("a real file at the home root"), 0644); err != nil {
		t.Fatal(err)
	}
	after := getPath(t, srv, "favicon.ico")

	if before.Code != after.Code {
		t.Errorf("status changed once the file existed: %d then %d", before.Code, after.Code)
	}
	if !bytes.Equal(before.Body.Bytes(), after.Body.Bytes()) {
		t.Error("body changed once the file existed")
	}
	if bytes.Contains(after.Body.Bytes(), []byte("a real file")) {
		t.Error("served the file on disk instead of the embedded icon")
	}
}

// A folder's own icon is requested at the folder's own path, so it never reaches
// the fallback and is still served from disk.
func TestFolderFaviconIsNotShadowed(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	dir := registerSubdirPage(t, srv, "project")

	own := []byte("<svg xmlns=\"http://www.w3.org/2000/svg\"><!-- the folder's own --></svg>")
	if err := os.WriteFile(filepath.Join(dir, "favicon.svg"), own, 0644); err != nil {
		t.Fatal(err)
	}

	w := getPath(t, srv, "project/favicon.svg")
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !bytes.Equal(w.Body.Bytes(), own) {
		t.Error("the folder's own favicon.svg was not served")
	}
}
