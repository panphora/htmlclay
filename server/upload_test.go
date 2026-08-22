package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/panphora/htmlclay/session"
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
