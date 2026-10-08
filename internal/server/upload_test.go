package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/panphora/htmlclay/internal/logging"
	"github.com/panphora/htmlclay/internal/session"
)

type uploadResponse struct {
	OK      bool   `json:"ok"`
	Code    string `json:"code"`
	Uploads []struct {
		Name  string `json:"name"`
		URL   string `json:"url"`
		Bytes int    `json:"bytes"`
	} `json:"uploads"`
}

// postUpload sends one multipart file part named "file", the canonical request
// from section 9.
func postUpload(t *testing.T, srv *Server, token, filename string, content []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatalf("write part: %v", err)
	}
	mw.Close()

	req := httptest.NewRequest("POST", "/_/upload/"+token, &body)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.SetPathValue("token", token)

	w := httptest.NewRecorder()
	srv.handleUpload(w, req)
	return w
}

func decodeUpload(t *testing.T, w *httptest.ResponseRecorder) uploadResponse {
	t.Helper()
	var out uploadResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response %q: %v", w.Body.String(), err)
	}
	return out
}

// assetsDir is where uploads for the fixture document land.
func assetsDir(f *session.File) string {
	dir, _ := assetsDirFor(f.AbsPath)
	return dir
}

func TestUploadStoresBesideTheDocument(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	w := postUpload(t, srv, f.Token, "cover.png", []byte("PNGDATA"))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	res := decodeUpload(t, w)
	if len(res.Uploads) != 1 {
		t.Fatalf("expected one upload, got %d", len(res.Uploads))
	}
	up := res.Uploads[0]

	// The document is test.htmlclay, so the folder is assets-test and the URL is
	// relative to the document itself.
	if !strings.HasPrefix(up.URL, "assets-test/") {
		t.Errorf("url = %q, want it under assets-test/", up.URL)
	}
	if up.Bytes != 7 {
		t.Errorf("bytes = %d, want 7", up.Bytes)
	}
	stored, err := os.ReadFile(filepath.Join(assetsDir(f), up.Name))
	if err != nil {
		t.Fatalf("stored file: %v", err)
	}
	if string(stored) != "PNGDATA" {
		t.Errorf("stored %q, want PNGDATA", stored)
	}
}

func TestUploadIdenticalBytesConverge(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	a := decodeUpload(t, postUpload(t, srv, f.Token, "photo.png", []byte("same")))
	b := decodeUpload(t, postUpload(t, srv, f.Token, "photo.png", []byte("same")))
	if a.Uploads[0].URL != b.Uploads[0].URL {
		t.Errorf("same bytes stored twice: %q vs %q", a.Uploads[0].URL, b.Uploads[0].URL)
	}
	entries, _ := os.ReadDir(assetsDir(f))
	if len(entries) != 1 {
		t.Errorf("expected 1 file, got %d", len(entries))
	}
}

func TestUploadDifferentBytesBothSurvive(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	a := decodeUpload(t, postUpload(t, srv, f.Token, "photo.png", []byte("one")))
	b := decodeUpload(t, postUpload(t, srv, f.Token, "photo.png", []byte("two")))
	if a.Uploads[0].URL == b.Uploads[0].URL {
		t.Fatal("different bytes collapsed onto one name")
	}
	entries, _ := os.ReadDir(assetsDir(f))
	if len(entries) != 2 {
		t.Errorf("expected 2 files, got %d", len(entries))
	}
}

// The one case the exclusive create exists for. Content-hash naming means
// different bytes normally get different names and never contend, so without
// this the O_EXCL could become a plain write and every other test still pass.
func TestUploadNeverOverwritesADifferentFile(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	content := []byte("the real upload")
	sum := sha256.Sum256(content)
	taken := "photo-" + hex.EncodeToString(sum[:])[:6] + ".png"

	dir := assetsDir(f)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, taken), []byte("SOMETHING ELSE"), 0o644)

	res := decodeUpload(t, postUpload(t, srv, f.Token, "photo.png", content))
	if res.Uploads[0].Name == taken {
		t.Fatal("upload took a name that was already occupied by different bytes")
	}
	kept, _ := os.ReadFile(filepath.Join(dir, taken))
	if string(kept) != "SOMETHING ELSE" {
		t.Errorf("pre-existing file was overwritten: %q", kept)
	}
}

func TestUploadRefusesActiveContent(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	for _, name := range []string{"payload.html", "payload.js", "payload.htmlclay", "payload.XML"} {
		w := postUpload(t, srv, f.Token, name, []byte("<script>alert(1)</script>"))
		if w.Code != http.StatusUnsupportedMediaType {
			t.Errorf("%s: expected 415, got %d", name, w.Code)
		}
		if code := decodeUpload(t, w).Code; code != "unsupported-type" {
			t.Errorf("%s: code = %q", name, code)
		}
	}
	if _, err := os.Stat(assetsDir(f)); err == nil {
		t.Error("a refused upload created the assets folder")
	}
}

// SVG is accepted BECAUSE it is served inert. Both halves are asserted here, so
// dropping either one fails.
func TestUploadSVGIsStoredAndServedInert(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	res := decodeUpload(t, postUpload(t, srv, f.Token, "logo.svg", svg))
	if len(res.Uploads) != 1 {
		t.Fatal("SVG was refused; it should be stored and served inert instead")
	}

	rel := res.Uploads[0].URL
	req := httptest.NewRequest("GET", "/"+rel, nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.SetPathValue("path", rel)
	w := httptest.NewRecorder()
	srv.handleServeFile(w, req)

	if w.Code != 200 {
		t.Fatalf("serving the upload back: expected 200, got %d", w.Code)
	}
	if cd := w.Header().Get("Content-Disposition"); cd != "attachment" {
		t.Errorf("Content-Disposition = %q, want attachment", cd)
	}
	if xcto := w.Header().Get("X-Content-Type-Options"); xcto != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", xcto)
	}
}

// The upload has to be readable back, which is the half a store-only test misses:
// opening a document grants a read root over its folder, and the assets folder
// sits inside it.
func TestUploadIsReadableBack(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	res := decodeUpload(t, postUpload(t, srv, f.Token, "cover.png", []byte("PNGDATA")))
	rel := res.Uploads[0].URL

	req := httptest.NewRequest("GET", "/"+rel, nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.SetPathValue("path", rel)
	w := httptest.NewRecorder()
	srv.handleServeFile(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != "PNGDATA" {
		t.Errorf("served %q, want PNGDATA", w.Body.String())
	}
}

func TestUploadFilenameCannotEscape(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	res := decodeUpload(t, postUpload(t, srv, f.Token, "../../escaped.png", []byte("X")))
	name := res.Uploads[0].Name
	if strings.Contains(name, "/") || strings.Contains(name, "..") {
		t.Fatalf("stored name kept path structure: %q", name)
	}
	home := srv.sessions.HomeDir()
	if _, err := os.Stat(filepath.Join(filepath.Dir(home), "escaped.png")); err == nil {
		t.Error("upload escaped the home directory")
	}
	entries, _ := os.ReadDir(assetsDir(f))
	if len(entries) != 1 {
		t.Errorf("expected 1 file in the assets folder, got %d", len(entries))
	}
}

func TestUploadEncodesTheReturnedURL(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	res := decodeUpload(t, postUpload(t, srv, f.Token, "header photo.png", []byte("X")))
	up := res.Uploads[0]
	if !strings.Contains(up.URL, "%20") {
		t.Errorf("url = %q, want the space percent-encoded (a raw space breaks srcset)", up.URL)
	}
	if _, err := os.Stat(filepath.Join(assetsDir(f), up.Name)); err != nil {
		t.Errorf("stored name should keep its real characters: %v", err)
	}
}

func TestUploadInvalidToken(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)

	w := postUpload(t, srv, "bad-token", "cover.png", []byte("X"))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestUploadRejectsAnEmptyFile(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	w := postUpload(t, srv, f.Token, "empty.png", nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestMetaAnnouncesTheSpecAndExtensions(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	req := httptest.NewRequest("GET", "/_/meta/"+f.Token, nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.SetPathValue("token", f.Token)
	w := httptest.NewRecorder()
	srv.handleMeta(w, req)

	var meta struct {
		Spec       int      `json:"spec"`
		Extensions []string `json:"extensions"`
		Name       string   `json:"name"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &meta); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	if meta.Spec != specVersion {
		t.Errorf("spec = %d, want %d", meta.Spec, specVersion)
	}
	announced := map[string]bool{}
	for _, e := range meta.Extensions {
		announced[e] = true
	}
	// Membership, not position. The list grows, and the order it grows in was never
	// a contract: asserting Extensions[0] made adding a second capability a failure.
	if !announced["upload"] {
		t.Errorf("extensions = %v, want upload announced", meta.Extensions)
	}
	if !announced["sync"] {
		t.Errorf("extensions = %v, want sync announced now that both halves of /_/sync are served", meta.Extensions)
	}
	// Additive: every field that was there before still is.
	if meta.Name != "test.htmlclay" {
		t.Errorf("name = %q, the existing shape must not change", meta.Name)
	}
}

// The route must be guarded where it is registered, not by anything the handler
// does. Driving the real mux is what proves that: calling handleUpload directly
// bypasses sameOrigin entirely, so every other test in this file would still pass
// if the guard were dropped from the registration line.
func TestUploadRouteIsGuardedAtRegistration(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, _ := mw.CreateFormFile("file", "cover.png")
	part.Write([]byte("PNGDATA"))
	mw.Close()

	req := httptest.NewRequest("POST", "/_/upload/"+f.Token, &body)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	// Deliberately NOT Sec-Fetch-Site: cross-site. The host middleware in front of
	// the mux already rejects that one globally, so a cross-site request would
	// prove nothing about this route. `same-site` is the case that reaches the mux
	// and that only sameOrigin refuses: a page on another port of this same host
	// is a different origin, and a token that leaked to it must not save.
	req.Header.Set("Origin", "http://127.0.0.1:9999")
	req.Header.Set("Sec-Fetch-Site", "same-site")

	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code == 200 {
		t.Fatalf("a cross-origin upload was accepted: %s", w.Body.String())
	}
	dir, _ := assetsDirFor(f.AbsPath)
	if _, err := os.Stat(dir); err == nil {
		t.Error("a cross-origin upload created the assets folder")
	}
}

// The refusal has to cover the whole page-and-script set, not the three
// extensions the map used to enumerate: .shtml, .rss, .atom and .mml all serve
// as text/html or an XML document from this origin just as .html does.
func TestUploadRefusesEveryPageAndScriptType(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	for _, name := range []string{"x.shtml", "x.rss", "x.atom", "x.mml", "x.HTML"} {
		w := postUpload(t, srv, f.Token, name, []byte("<script>alert(1)</script>"))
		if w.Code != http.StatusUnsupportedMediaType {
			t.Errorf("%s: expected 415, got %d", name, w.Code)
		}
		if code := decodeUpload(t, w).Code; code != "unsupported-type" {
			t.Errorf("%s: code = %q", name, code)
		}
	}
	if _, err := os.Stat(assetsDir(f)); err == nil {
		t.Error("a refused upload created the assets folder")
	}
}

// An empty or unknown extension cannot be served as a page, so it is accepted.
// The asset lane is what makes that safe: it types the file application/octet-
// stream with nosniff, which a browser downloads instead of running.
func TestUploadAcceptsUnknownTypesAndServesThemAsDownloads(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	payload := []byte("<html><script>alert(1)</script></html>")
	for _, name := range []string{"note", "note.foo"} {
		res := decodeUpload(t, postUpload(t, srv, f.Token, name, payload))
		if len(res.Uploads) != 1 {
			t.Fatalf("%s: expected one upload, got %d", name, len(res.Uploads))
		}
		rel := res.Uploads[0].URL

		req := httptest.NewRequest("GET", "/"+rel, nil)
		req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
		req.SetPathValue("path", rel)
		w := httptest.NewRecorder()
		srv.handleServeFile(w, req)

		if w.Code != 200 {
			t.Fatalf("%s: expected 200, got %d", name, w.Code)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/octet-stream" {
			t.Errorf("%s: Content-Type = %q, want application/octet-stream", name, ct)
		}
		if xcto := w.Header().Get("X-Content-Type-Options"); xcto != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q, want nosniff", name, xcto)
		}
	}
}

// Any +xml type under any top-level type is an XML document to a browser, and
// the ecmascript types are scripts, so the pattern cannot be limited to
// application/….+xml. SVG stays accepted: refusedUpload answers for it before
// the pattern runs, and the asset lane serves it inert.
func TestRefusedUploadCoversEveryXMLAndScriptFamily(t *testing.T) {
	// refusedUpload judges by the MIME type, so a machine whose table lacks these
	// would classify them as unknown downloads instead.
	for ext, ctype := range map[string]string{".x3d": "model/x3d+xml", ".dae": "model/vnd.collada+xml"} {
		if mime.TypeByExtension(ext) != "" {
			continue
		}
		if err := mime.AddExtensionType(ext, ctype); err != nil {
			t.Fatal(err)
		}
	}

	for _, ext := range []string{".x3d", ".dae", ".ecma"} {
		if !refusedUpload(ext) {
			t.Errorf("refusedUpload(%q) = false, want true", ext)
		}
	}
	for _, ext := range []string{".svg", ".png", ".pdf", ""} {
		if refusedUpload(ext) {
			t.Errorf("refusedUpload(%q) = true, want false", ext)
		}
	}
}

func TestRefusedUploadClassifiesByTypeNotJustExtension(t *testing.T) {
	for _, tc := range []struct {
		ext  string
		want bool
	}{
		{".svg", false},
		{".svgz", false},
		{"", false},
		{".png", false},
		{".pdf", false},
		{".zip", false},
		{".shtml", true},
		{".xhtml", true},
		{".js", true},
	} {
		if got := refusedUpload(tc.ext); got != tc.want {
			t.Errorf("refusedUpload(%q) = %v, want %v", tc.ext, got, tc.want)
		}
	}
}

// The cap the meta route advertises is a cap on the FILE. Counting the multipart
// framing as part of the file refused a file of exactly the advertised size,
// which is the one size a client that read maxBytes will send.
func TestUploadAcceptsAFileExactlyAtTheCap(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	exact := bytes.Repeat([]byte("x"), maxUploadSize)
	res := decodeUpload(t, postUpload(t, srv, f.Token, "big.bin", exact))
	if len(res.Uploads) != 1 {
		t.Fatalf("a file of exactly maxUploadSize was refused: %s", res.Code)
	}
	if res.Uploads[0].Bytes != maxUploadSize {
		t.Errorf("bytes = %d, want %d", res.Uploads[0].Bytes, maxUploadSize)
	}

	w := postUpload(t, srv, f.Token, "big.bin", append(exact, 'x'))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("one byte over the cap: expected 413, got %d", w.Code)
	}
	if code := decodeUpload(t, w).Code; code != "too-large" {
		t.Errorf("one byte over the cap: code = %q, want too-large", code)
	}
}

// The upload token is this route's whole credential, as it is for save, and the
// access log is a plain file on disk. The path is logged; the secret is not.
func TestUploadTokenDoesNotReachTheAccessLog(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)
	logPath := filepath.Join(t.TempDir(), "access.log")
	logger, err := logging.New(logPath)
	if err != nil {
		t.Fatal(err)
	}
	srv.logger = logger

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, _ := mw.CreateFormFile("file", "cover.png")
	part.Write([]byte("PNGDATA"))
	mw.Close()

	req := httptest.NewRequest("POST", "/_/upload/"+f.Token, &body)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	// The route is guarded at registration; the guard is not what is under test.
	req.Header.Set("Sec-Fetch-Site", "same-origin")

	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)
	logger.Close()

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logged), f.Token) {
		t.Fatalf("upload token reached the access log: %s", logged)
	}
	if !strings.Contains(string(logged), "/_/upload/<redacted>") {
		t.Fatalf("access log does not name the upload route: %s", logged)
	}
}

// A symlinked assets folder is the whole reason the write path uses an *os.Root:
// MkdirAll and OpenFile on the joined path would follow the link and drop the
// file outside the document's folder, where the read root does not cover it.
func TestUploadRefusesASymlinkedAssetsFolder(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	outside := t.TempDir()
	dir, _ := assetsDirFor(f.AbsPath)
	if err := os.Symlink(outside, dir); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	w := postUpload(t, srv, f.Token, "cover.png", []byte("PNGDATA"))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 for a symlinked assets folder, got %d: %s", w.Code, w.Body.String())
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the write followed the symlink out of the document's folder: %v", entries)
	}
}

// The publish is a hard link from a hidden temp file. The temp file is the one
// name that must never survive, or the asset lane (which skips hidden
// components) would hold an invisible leak per upload.
func TestUploadLeavesNoTempFileBehind(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	postUpload(t, srv, f.Token, "cover.png", []byte("PNGDATA"))
	postUpload(t, srv, f.Token, "cover.png", []byte("PNGDATA"))

	entries, err := os.ReadDir(assetsDir(f))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".upload-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
	if len(entries) != 1 {
		t.Errorf("expected 1 published file, got %d", len(entries))
	}
}
