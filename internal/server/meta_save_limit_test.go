package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

// multibyteSaveBody builds a complete HTML document of exactly maxSaveSize+extra
// bytes whose padding is two-byte characters, so the body is shorter in runes than
// in bytes. A host that advertised or enforced a character count would draw the
// boundary somewhere else than a host that counts the UTF-8 bytes it is sent.
func multibyteSaveBody(t *testing.T, extra int) string {
	t.Helper()
	const prefix = "<!DOCTYPE html>\n<html lang=\"en\">\n<body>\n<p>"
	const suffix = "</p>\n</body>\n</html>"
	fill := maxSaveSize + extra - len(prefix) - len(suffix)
	if fill < 0 {
		t.Fatalf("no room for padding at %d bytes", maxSaveSize)
	}
	pad := strings.Repeat("é", fill/2)
	if fill%2 != 0 {
		pad += "x"
	}
	body := prefix + pad + suffix
	if len(body) != maxSaveSize+extra {
		t.Fatalf("fixture is %d bytes, want %d", len(body), maxSaveSize+extra)
	}
	return body
}

// The limit as a client reads it out of the token metadata answer. The document
// block is the only place it may appear, which is why a top-level field of the
// same name fails here.
func saveLimitFromMeta(t *testing.T, w *httptest.ResponseRecorder) int64 {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("meta: got %d, want 200 (%q)", w.Code, w.Body.String())
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &top); err != nil {
		t.Fatalf("decode meta %q: %v", w.Body.String(), err)
	}
	if _, dup := top["maxBytes"]; dup {
		t.Errorf("meta carries a top-level maxBytes as well as the document block: %q", w.Body.String())
	}
	var body struct {
		Document *struct {
			MaxBytes *int64 `json:"maxBytes"`
		} `json:"document"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode meta %q: %v", w.Body.String(), err)
	}
	if body.Document == nil {
		t.Fatalf("meta carries no document block: %q", w.Body.String())
	}
	if body.Document.MaxBytes == nil {
		t.Fatalf("document block carries no maxBytes: %q", w.Body.String())
	}
	return *body.Document.MaxBytes
}

// The number a client warns with must be the number the save route refuses at, so
// the boundary is exercised rather than the constant merely compared: a body of
// exactly the announced size is stored, one byte more is refused.
func TestMetaSaveLimitIsTheSaveBoundary(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	announced := saveLimitFromMeta(t, getThroughMux(t, srv, "/_/meta/"+f.Token))
	if announced != maxSaveSize {
		t.Fatalf("document.maxBytes = %d, want the %d the save route enforces", announced, maxSaveSize)
	}

	body := multibyteSaveBody(t, 0)
	if utf8.RuneCountInString(body) >= len(body) {
		t.Fatalf("fixture is not multibyte: %d runes for %d bytes", utf8.RuneCountInString(body), len(body))
	}
	w := saveThroughMux(t, srv, f, body, "")
	if w.Code != 200 {
		t.Fatalf("save of exactly maxBytes (%d bytes, %d runes): got %d, want 200 (%q)",
			len(body), utf8.RuneCountInString(body), w.Code, w.Body.String())
	}
	if res := decode(t, w); res["ok"] != true {
		t.Fatalf("save answer = %q, want ok", w.Body.String())
	}
	stored, err := os.ReadFile(f.AbsPath)
	if err != nil {
		t.Fatalf("read stored document: %v", err)
	}
	if len(stored) != len(body) {
		t.Errorf("stored document is %d bytes, sent %d: the limit was not counted in UTF-8 bytes", len(stored), len(body))
	}

	over := multibyteSaveBody(t, 1)
	w = saveThroughMux(t, srv, f, over, "")
	if w.Code != 413 {
		t.Fatalf("save of maxBytes+1 (%d bytes): got %d, want 413 (%q)", len(over), w.Code, w.Body.String())
	}
	stored, err = os.ReadFile(f.AbsPath)
	if err != nil {
		t.Fatalf("read stored document after refusal: %v", err)
	}
	if len(stored) != len(body) {
		t.Errorf("refused save changed the file: %d bytes on disk, %d before", len(stored), len(body))
	}
}

// Withholding the document block withholds the limit with it. A caller who may not
// see the document, and the tokenless discovery route that never carries a
// document block, must not be handed a save limit as though they had one.
func TestMetaSaveLimitWithheldWithTheDocumentBlock(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)

	unauthorized := getThroughMux(t, srv, "/_/meta/not-a-token")
	if unauthorized.Code != 401 {
		t.Errorf("meta with an unknown token: got %d, want 401 (%q)", unauthorized.Code, unauthorized.Body.String())
	}
	if strings.Contains(unauthorized.Body.String(), "maxBytes") {
		t.Errorf("refused meta still reports a save limit: %q", unauthorized.Body.String())
	}

	host := getThroughMux(t, srv, "/_/meta")
	if host.Code != 200 {
		t.Fatalf("tokenless meta: got %d, want 200", host.Code)
	}
	if strings.Contains(host.Body.String(), "maxBytes") {
		t.Errorf("tokenless meta reports a save limit: %q", host.Body.String())
	}
	var hostMeta map[string]json.RawMessage
	if err := json.Unmarshal(host.Body.Bytes(), &hostMeta); err != nil {
		t.Fatalf("decode tokenless meta %q: %v", host.Body.String(), err)
	}
	if _, ok := hostMeta["document"]; ok {
		t.Errorf("tokenless meta carries a document block: %q", host.Body.String())
	}
}
