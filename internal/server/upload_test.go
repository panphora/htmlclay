package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/panphora/htmlclay/internal/logging"
	"github.com/panphora/htmlclay/internal/session"
	"github.com/panphora/htmlclay/internal/versions"
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

// assetsDir is where uploads for the fixture document land: its folder inside the
// per-computer library this server was pointed at.
func assetsDir(srv *Server, f *session.File) string {
	return filepath.Join(srv.uploadsDir, assetsFolderName(f.AbsPath))
}

// getFromLibrary fetches a library path through the real mux, so the route
// registration, the wildcard value and the host gate are all exercised rather
// than bypassed by calling a handler directly.
func getFromLibrary(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)
	return w
}

func TestUploadLandsInTheLibrary(t *testing.T) {
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

	// The document is test.htmlclay, so the library folder is assets-test and the
	// URL is a host path into the library, which every site server answers. The
	// name carries at least 128 bits of content hash, so it is not guessable from
	// the document's own name.
	pattern := regexp.MustCompile(`^/_/uploads/assets-test/cover-[0-9a-f]{32,}\.png$`)
	if !pattern.MatchString(up.URL) {
		t.Errorf("url = %q, want it to match %s", up.URL, pattern)
	}
	if up.Bytes != 7 {
		t.Errorf("bytes = %d, want 7", up.Bytes)
	}
	stored, err := os.ReadFile(filepath.Join(assetsDir(srv, f), up.Name))
	if err != nil {
		t.Fatalf("stored file: %v", err)
	}
	if string(stored) != "PNGDATA" {
		t.Errorf("stored %q, want PNGDATA", stored)
	}
	// Nothing lands beside the document any more: that folder is what made the
	// link break the moment the document moved.
	if _, err := os.Stat(filepath.Join(filepath.Dir(f.AbsPath), "assets-test")); err == nil {
		t.Error("the upload was also stored beside the document")
	}
}

func TestUploadIdenticalBytesConverge(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	a := decodeUpload(t, postUpload(t, srv, f.Token, "photo.png", []byte("same")))
	b := decodeUpload(t, postUpload(t, srv, f.Token, "photo.png", []byte("same")))
	if a.Uploads[0].URL != b.Uploads[0].URL {
		t.Errorf("same bytes stored twice: %q vs %q", a.Uploads[0].URL, b.Uploads[0].URL)
	}
	entries, _ := os.ReadDir(assetsDir(srv, f))
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
	entries, _ := os.ReadDir(assetsDir(srv, f))
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
	taken := "photo-" + hex.EncodeToString(sum[:])[:uploadHashMin] + ".png"

	dir := assetsDir(srv, f)
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
	if _, err := os.Stat(assetsDir(srv, f)); err == nil {
		t.Error("a refused upload created the library folder")
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

	w := getFromLibrary(t, srv, res.Uploads[0].URL)
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

// The upload has to be readable back, which is the half a store-only test misses.
// It is answered from the library by the site server, not through a read root:
// nothing here installs ~/htmlclay/uploads as one.
func TestUploadIsReadableBack(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	res := decodeUpload(t, postUpload(t, srv, f.Token, "cover.png", []byte("PNGDATA")))
	w := getFromLibrary(t, srv, res.Uploads[0].URL)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != "PNGDATA" {
		t.Errorf("served %q, want PNGDATA", w.Body.String())
	}
}

// The whole point of one per-computer library: the same link resolves on every
// site server, whatever folder its document sits in.
func TestUploadServedFromASecondSiteServer(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	payload := []byte("PNGDATA")
	res := decodeUpload(t, postUpload(t, srv, f.Token, "cover.png", payload))

	// A second site on its own port, serving a document from a folder the first
	// site has never seen, with the same per-computer library.
	otherHome := t.TempDir()
	otherDoc := filepath.Join(otherHome, "sub", "other.htmlclay")
	if err := os.MkdirAll(filepath.Dir(otherDoc), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(otherDoc, []byte("<!DOCTYPE html>\n<html><body>other</body></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	mgr := newTestManager(t, otherHome)
	other, err := mgr.Register(otherDoc, session.ViaOsOpen)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	site := New(ln, mgr, logging.NewStdout(), versions.New(t.TempDir()))
	site.uploadsDir = srv.uploadsDir

	w := getFromLibrary(t, site, res.Uploads[0].URL)
	if w.Code != 200 {
		t.Fatalf("second site server: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !bytes.Equal(w.Body.Bytes(), payload) {
		t.Errorf("second site server served %q, want %q", w.Body.String(), payload)
	}

	// An upload from that second document lands in the same library, under its
	// own folder, and the first site answers it from there.
	made := decodeUpload(t, postUpload(t, site, other.Token, "other.png", []byte("OTHER")))
	if !strings.HasPrefix(made.Uploads[0].URL, "/_/uploads/assets-other/") {
		t.Errorf("second site upload url = %q, want it under /_/uploads/assets-other/", made.Uploads[0].URL)
	}
	if _, err := os.Stat(filepath.Join(srv.uploadsDir, "assets-other", made.Uploads[0].Name)); err != nil {
		t.Errorf("second site upload is not in the shared library: %v", err)
	}
	if back := getFromLibrary(t, srv, made.Uploads[0].URL); back.Code != 200 || back.Body.String() != "OTHER" {
		t.Errorf("first site serving the second site's upload: %d %q", back.Code, back.Body.String())
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
	entries, _ := os.ReadDir(assetsDir(srv, f))
	if len(entries) != 1 {
		t.Errorf("expected 1 file in the library folder, got %d", len(entries))
	}
}

func TestUploadEncodesTheReturnedURL(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	res := decodeUpload(t, postUpload(t, srv, f.Token, "header photo.png", []byte("X")))
	up := res.Uploads[0]
	if !strings.Contains(up.URL, "%20") {
		t.Errorf("url = %q, want the space percent-encoded (a raw space breaks srcset)", up.URL)
	}
	if _, err := os.Stat(filepath.Join(assetsDir(srv, f), up.Name)); err != nil {
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
	if _, err := os.Stat(assetsDir(srv, f)); err == nil {
		t.Error("a cross-origin upload created the library folder")
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
	if _, err := os.Stat(assetsDir(srv, f)); err == nil {
		t.Error("a refused upload created the library folder")
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
		w := getFromLibrary(t, srv, res.Uploads[0].URL)
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

// A symlinked library folder is the whole reason the write path uses an *os.Root:
// MkdirAll and OpenFile on the joined path would follow the link and drop the
// file outside the library.
func TestUploadRefusesASymlinkedLibraryFolder(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	outside := t.TempDir()
	if err := os.Symlink(outside, assetsDir(srv, f)); err != nil {
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
		t.Errorf("the write followed the symlink out of the library: %v", entries)
	}
}

// The publish is a hard link from a hidden temp file. The temp file is the one
// name that must never survive, or the asset lane (which skips hidden
// components) would hold an invisible leak per upload.
func TestUploadLeavesNoTempFileBehind(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	postUpload(t, srv, f.Token, "cover.png", []byte("PNGDATA"))
	postUpload(t, srv, f.Token, "cover.png", []byte("PNGDATA"))

	entries, err := os.ReadDir(assetsDir(srv, f))
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

// The library folder is the document's own name, slugged to the alphabet
// hyperclay.com accepts, so the same layout works on every host.
func TestAssetsFolderName(t *testing.T) {
	for _, tc := range []struct {
		path string
		want string
	}{
		{"/home/d/board.htmlclay", "assets-board"},
		{"/home/d/My Board.v2.htmlclay", "assets-my-board-v2"},
		{"/home/d/---.html", "assets-document"},
		{"/home/d/Plan (draft)!.html", "assets-plan-draft"},
		{"/home/d/Ünicode 名前.htmlclay", "assets-nicode"},
		{"/home/d/keep_this-one.html", "assets-keep_this-one"},
		{"/home/d/folder.html", "assets-folder"},
	} {
		if got := assetsFolderName(tc.path); got != tc.want {
			t.Errorf("assetsFolderName(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// The library is served without a token, so containment cannot rest on one: the
// OS refuses anything that leaves the library, whatever the spelling.
func TestLibraryRefusesTraversal(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	res := decodeUpload(t, postUpload(t, srv, f.Token, "cover.png", []byte("PNGDATA")))
	tail := strings.TrimPrefix(res.Uploads[0].URL, "/_/uploads/")

	outside := filepath.Join(filepath.Dir(srv.uploadsDir), "outside-secret.txt")
	if err := os.WriteFile(outside, []byte("SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		"/_/uploads/../outside-secret.txt",
		"/_/uploads/assets-test/../../outside-secret.txt",
		"/_/uploads/%2e%2e/outside-secret.txt",
		"/_/uploads/assets-test/%2e%2e%2foutside-secret.txt",
		"/_/uploads/assets-test/..%2Foutside-secret.txt",
	} {
		w := getFromLibrary(t, srv, path)
		if w.Code == 200 || strings.Contains(w.Body.String(), "SECRET") {
			t.Errorf("%s: escaped the library: %d %q", path, w.Code, w.Body.String())
		}
		// A raw ".." is cleaned by the mux into a 307 before the handler runs, so
		// the one thing left to check is that it does not redirect back in here.
		if loc := w.Header().Get("Location"); strings.HasPrefix(loc, "/_/uploads/") {
			t.Errorf("%s: redirected back into the library: %q", path, loc)
		}
	}

	// The same paths as the handler receives them once the mux has decoded the
	// wildcard, so the check does not depend on ServeMux cleaning anything first.
	for _, rel := range []string{
		"../outside-secret.txt",
		"assets-test/../../outside-secret.txt",
		tail + "/../../outside-secret.txt",
	} {
		req := httptest.NewRequest("GET", "/_/uploads/", nil)
		req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
		req.SetPathValue("path", rel)
		w := httptest.NewRecorder()
		srv.handleLibraryUpload(w, req)
		if w.Code == 200 || strings.Contains(w.Body.String(), "SECRET") {
			t.Errorf("path %q: escaped the library: %d %q", rel, w.Code, w.Body.String())
		}
	}
}

// A symlink inside the library is the one way a path that looks contained can
// point elsewhere, so it must be refused by the open itself.
func TestLibraryRefusesSymlinksLeavingIt(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	postUpload(t, srv, f.Token, "cover.png", []byte("PNGDATA"))

	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(assetsDir(srv, f), "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(srv.uploadsDir, "assets-linked")); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		"/_/uploads/assets-test/link.txt",
		"/_/uploads/assets-linked/secret.txt",
	} {
		w := getFromLibrary(t, srv, path)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: expected 404, got %d", path, w.Code)
		}
		if strings.Contains(w.Body.String(), "SECRET") {
			t.Errorf("%s: served a file from outside the library", path)
		}
	}
}

// The library is a flat folder of files. Nothing in it is a listing, and a hidden
// name is the temp file an interrupted upload leaves rather than a file to serve.
func TestLibraryServesOnlyRegularNonHiddenFiles(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	postUpload(t, srv, f.Token, "cover.png", []byte("PNGDATA"))
	if err := os.WriteFile(filepath.Join(assetsDir(srv, f), ".hidden.png"), []byte("HIDDEN"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		"/_/uploads/",
		"/_/uploads/assets-test",
		"/_/uploads/assets-test/",
		"/_/uploads/assets-test/.hidden.png",
		"/_/uploads/.hidden.png",
		"/_/uploads/assets-nope/cover.png",
		"/_/uploads/assets-test/nope.png",
	} {
		w := getFromLibrary(t, srv, path)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: expected 404, got %d: %s", path, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "HIDDEN") || strings.Contains(w.Body.String(), "PNGDATA") {
			t.Errorf("%s: answered with file bytes", path)
		}
	}
}

// Everything in the library arrived as an upload, so a page type is handed over as
// a download however it got there. A hand-placed page.html is the case a
// name-based rule would miss.
func TestLibraryServesPagesAsAttachments(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)

	page := []byte(`<html><script>alert(1)</script></html>`)
	dir := filepath.Join(srv.uploadsDir, "assets-x")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "page.html"), page, 0o644); err != nil {
		t.Fatal(err)
	}

	w := getFromLibrary(t, srv, "/_/uploads/assets-x/page.html")
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if cd := w.Header().Get("Content-Disposition"); cd != "attachment" {
		t.Errorf("Content-Disposition = %q, want attachment", cd)
	}
	if xcto := w.Header().Get("X-Content-Type-Options"); xcto != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", xcto)
	}
	if !bytes.Equal(w.Body.Bytes(), page) {
		t.Errorf("served %q, want the stored bytes", w.Body.String())
	}
}

// A refused type reached through the upload route is refused on the way in, so a
// file in the library is only ever one that was accepted; the attachment rule is
// still what makes an unknown type safe.
func TestLibraryUnknownTypeIsADownload(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)

	dir := filepath.Join(srv.uploadsDir, "assets-x")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "note"), []byte("<html>hi</html>"), 0o644); err != nil {
		t.Fatal(err)
	}

	w := getFromLibrary(t, srv, "/_/uploads/assets-x/note")
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", ct)
	}
	if xcto := w.Header().Get("X-Content-Type-Options"); xcto != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", xcto)
	}
}

// With no home directory there is no library. Uploads have to fail loudly and the
// route has to answer 404, never fall back to writing somewhere else.
func TestUploadAndLibraryWithoutALibraryDirectory(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)
	srv.uploadsDir = ""

	w := postUpload(t, srv, f.Token, "cover.png", []byte("PNGDATA"))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	if code := decodeUpload(t, w).Code; code != "error" {
		t.Errorf("code = %q, want error", code)
	}

	if got := getFromLibrary(t, srv, "/_/uploads/assets-test/cover.png"); got.Code != http.StatusNotFound {
		t.Errorf("library route with no library: expected 404, got %d", got.Code)
	}
}
