package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/panphora/htmlclay/internal/session"
)

// writableOf reads the document block's writable field, failing if the block or
// the field is missing: "absent" is not a third answer to this question.
func writableOf(t *testing.T, meta map[string]any) bool {
	t.Helper()
	doc, ok := meta["document"].(map[string]any)
	if !ok {
		t.Fatalf("no document block: %v", meta)
	}
	raw, present := doc["writable"]
	if !present {
		t.Fatalf("document block carries no writable: %v", doc)
	}
	writable, ok := raw.(bool)
	if !ok {
		t.Fatalf("writable is %T, want bool: %v", raw, raw)
	}
	return writable
}

// A file this person opened in the app is one this session may save, and the
// discovery answer says so before any save is attempted.
func TestMetaWritableForAnOpenedFile(t *testing.T) {
	srv, f, content := setupHandlerTest(t)

	checked := false
	if got := writableOf(t, metaOf(t, srv, f)); !got {
		t.Errorf("document.writable = false for an opened file, want true")
	}
	checked = true

	if w := saveThroughMux(t, srv, f, content, ""); w.Code != 200 {
		t.Fatalf("save answered %d, want 200: %s", w.Code, w.Body.String())
	}

	if !checked {
		t.Fatal("no writable=true assertion was reached")
	}
}

// Untrusting the folder revokes the trusted write, so discovery stops offering
// Edit at the same moment the save route stops accepting one: writable and the
// save answer come from one admission, not two.
func TestMetaWritableFalseAfterTrustedWriteRevoked(t *testing.T) {
	srv, _, content := setupHandlerTest(t)
	ws := filepath.Join(srv.sessions.HomeDir(), "ws")
	if err := os.MkdirAll(ws, 0755); err != nil {
		t.Fatal(err)
	}
	page := filepath.Join(ws, "auto.htmlclay")
	if err := os.WriteFile(page, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	if err := srv.sessions.InstallTrustedRoot(ws); err != nil {
		t.Fatalf("install trusted root: %v", err)
	}
	f, err := srv.sessions.Register(page, session.ViaTrusted)
	if err != nil {
		t.Fatalf("register trusted-folder file: %v", err)
	}
	if got := writableOf(t, metaOf(t, srv, f)); !got {
		t.Errorf("document.writable = false while the folder is trusted, want true")
	}

	srv.sessions.RevokeTrustedRoot(ws)

	revokedFalse := false
	if got := writableOf(t, metaOf(t, srv, f)); got {
		t.Errorf("document.writable = true after the trusted write was revoked, want false")
	}
	revokedFalse = true

	if w := saveThroughMux(t, srv, f, content, ""); w.Code != 401 {
		t.Fatalf("save after revoke answered %d, want 401: %s", w.Code, w.Body.String())
	}

	if !revokedFalse {
		t.Fatal("no writable=false assertion was reached")
	}
}

// Discovery without a token is about the host, never a document, and a token
// nobody holds answers exactly as it did before this field existed.
func TestMetaWritableAbsentWithoutADocument(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)

	host := decode(t, getThroughMux(t, srv, "/_/meta"))
	hostChecked := false
	if _, present := host["document"]; present {
		t.Errorf("untokened discovery carries a document block: %v", host)
	}
	hostChecked = true

	unknown := getThroughMux(t, srv, "/_/meta/no-such-token")
	unknownChecked := false
	if unknown.Code != 401 {
		t.Fatalf("unknown token answered %d, want 401: %s", unknown.Code, unknown.Body.String())
	}
	body := decode(t, unknown)
	if ok, present := body["ok"]; !present || ok != false {
		t.Errorf("unknown token body ok = %v, want false: %v", ok, body)
	}
	if msg, _ := body["error"].(string); msg != "invalid token" {
		t.Errorf("unknown token body error = %q, want %q", msg, "invalid token")
	}
	if _, present := body["document"]; present {
		t.Errorf("unknown token answered with a document block: %v", body)
	}
	unknownChecked = true

	if !hostChecked || !unknownChecked {
		t.Fatal("not every assertion in this test was reached")
	}
}
