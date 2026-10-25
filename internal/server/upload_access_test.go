package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// accessMeta is the upload capability as a ClayJS-shaped client reads it.
type accessMeta struct {
	Allowed  bool   `json:"allowed"`
	MaxBytes int64  `json:"maxBytes"`
	Access   string `json:"access"`
}

func uploadAccessFromMeta(t *testing.T, w *httptest.ResponseRecorder) accessMeta {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("meta: got %d, want 200 (%q)", w.Code, w.Body.String())
	}
	var body struct {
		Document *struct {
			Upload accessMeta `json:"upload"`
		} `json:"document"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode meta %q: %v", w.Body.String(), err)
	}
	if body.Document == nil {
		t.Fatalf("meta carries no document block: %q", w.Body.String())
	}
	return body.Document.Upload
}

// The metadata and the result must agree, because a client checks the metadata
// before sending and verifies the result after: a host that announced `local` and
// answered something else would be asserting a promise it did not keep.
func TestUploadAccessMetadataAndResultAgree(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	meta := uploadAccessFromMeta(t, getThroughMux(t, srv, "/_/meta/"+f.Token))
	if meta.Access != "local" {
		t.Errorf("document.upload.access = %q, want local", meta.Access)
	}
	if !meta.Allowed {
		t.Error("document.upload.allowed = false, want true")
	}
	if meta.MaxBytes != maxUploadSize {
		t.Errorf("document.upload.maxBytes = %d, want %d", meta.MaxBytes, maxUploadSize)
	}

	w := postUpload(t, srv, f.Token, "cover.png", []byte("PNGDATA"))
	if w.Code != 200 {
		t.Fatalf("upload: got %d, want 200 (%q)", w.Code, w.Body.String())
	}
	var res struct {
		OK      bool `json:"ok"`
		Uploads []struct {
			Name   string `json:"name"`
			URL    string `json:"url"`
			Bytes  int    `json:"bytes"`
			Access string `json:"access"`
		} `json:"uploads"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode upload %q: %v", w.Body.String(), err)
	}
	if !res.OK || len(res.Uploads) != 1 {
		t.Fatalf("upload answer = %q, want one successful result", w.Body.String())
	}
	if res.Uploads[0].Access != meta.Access {
		t.Errorf("result access = %q, metadata access = %q, the two must agree", res.Uploads[0].Access, meta.Access)
	}
	if res.Uploads[0].Access != "local" {
		t.Errorf("result access = %q, want local", res.Uploads[0].Access)
	}
}

// Additive: a client written against the fields that were there before keeps
// working against the same answers, and the URL it is handed still resolves.
func TestUploadAccessKeepsOldFieldsWorking(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	var meta struct {
		Document *struct {
			Upload struct {
				Allowed  bool  `json:"allowed"`
				MaxBytes int64 `json:"maxBytes"`
			} `json:"upload"`
		} `json:"document"`
	}
	w := getThroughMux(t, srv, "/_/meta/"+f.Token)
	if err := json.Unmarshal(w.Body.Bytes(), &meta); err != nil {
		t.Fatalf("decode meta %q: %v", w.Body.String(), err)
	}
	if meta.Document == nil {
		t.Fatalf("meta carries no document block: %q", w.Body.String())
	}
	if !meta.Document.Upload.Allowed || meta.Document.Upload.MaxBytes != maxUploadSize {
		t.Errorf("old client read allowed=%v maxBytes=%d, want true and %d",
			meta.Document.Upload.Allowed, meta.Document.Upload.MaxBytes, maxUploadSize)
	}

	res := decodeUpload(t, postUpload(t, srv, f.Token, "cover.png", []byte("PNGDATA")))
	if !res.OK || len(res.Uploads) != 1 {
		t.Fatalf("old client could not read the result: %+v", res)
	}
	up := res.Uploads[0]
	if up.Name == "" || up.URL == "" || up.Bytes != len("PNGDATA") {
		t.Errorf("old client read name=%q url=%q bytes=%d from a six-byte upload", up.Name, up.URL, up.Bytes)
	}
	served := getFromLibrary(t, srv, up.URL)
	if served.Code != 200 || served.Body.String() != "PNGDATA" {
		t.Errorf("the URL the old client read served %d %q, want 200 PNGDATA", served.Code, served.Body.String())
	}
}

// Reporting an access model says nothing about where the bytes live: the local
// claim is only truthful while file serving stays inside the uploads folder.
func TestUploadAccessFileServingStaysContained(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	res := decodeUpload(t, postUpload(t, srv, f.Token, "cover.png", []byte("PNGDATA")))
	servedURL := res.Uploads[0].URL
	if !strings.HasPrefix(servedURL, "/_/uploads/") {
		t.Fatalf("uploaded URL = %q, want a path inside the library", servedURL)
	}
	if w := getFromLibrary(t, srv, servedURL); w.Code != 200 || w.Body.String() != "PNGDATA" {
		t.Errorf("%s served %d %q, want 200 PNGDATA", servedURL, w.Code, w.Body.String())
	}

	outside := filepath.Join(filepath.Dir(srv.uploadsDir), "outside-secret.txt")
	if err := os.WriteFile(outside, []byte("SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	tail := strings.TrimPrefix(servedURL, "/_/uploads/")
	for _, path := range []string{
		"/_/uploads/../outside-secret.txt",
		"/_/uploads/" + tail + "/../../outside-secret.txt",
		"/_/uploads/%2e%2e/outside-secret.txt",
	} {
		w := getFromLibrary(t, srv, path)
		if w.Code == 200 || strings.Contains(w.Body.String(), "SECRET") {
			t.Errorf("%s: escaped the library: %d %q", path, w.Code, w.Body.String())
		}
	}
	for _, rel := range []string{
		"../outside-secret.txt",
		tail + "/../../outside-secret.txt",
	} {
		req := httptest.NewRequest("GET", "/_/uploads/", nil)
		req.Host = "127.0.0.1"
		req.SetPathValue("path", rel)
		w := httptest.NewRecorder()
		srv.handleLibraryUpload(w, req)
		if w.Code == 200 || strings.Contains(w.Body.String(), "SECRET") {
			t.Errorf("path %q: escaped the library: %d %q", rel, w.Code, w.Body.String())
		}
	}
}
