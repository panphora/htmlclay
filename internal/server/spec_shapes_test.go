package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The three shapes this host answered in its own older form while announcing the
// Malleable HTML File protocol. None of them stopped the conformance gate passing,
// which says more about the gate than about the host: it checks the shapes it was
// built to check, and an error body, the layout of a discovery answer, and the
// channel's header were not among them.
//
// Every one of these keeps its old form alongside the new. A document's inline script
// is frozen the moment the file is saved, so a reader of the old shape can never be
// updated, and removing it would turn a working read into `undefined` against an API
// that still answers 200.

// §3 fixes the set of error codes so a client can tell "you are not allowed" from
// "that file is gone" without parsing prose. Without a code every refusal from this
// host is an opaque non-2xx.
func TestWriteErrorCarriesBothShapes(t *testing.T) {
	s := &Server{}

	cases := []struct {
		status int
		code   string
	}{
		{http.StatusUnauthorized, "unauthorized"},
		{http.StatusForbidden, "forbidden"},
		{http.StatusNotFound, "not-found"},
		{http.StatusRequestEntityTooLarge, "too-large"},
		{http.StatusUnsupportedMediaType, "unsupported-type"},
		{http.StatusUnprocessableEntity, "invalid-document"},
	}

	for _, c := range cases {
		w := httptest.NewRecorder()
		s.writeError(w, c.status, "because")

		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("status %d: %v", c.status, err)
		}
		if body["code"] != c.code {
			t.Errorf("status %d: code = %v, want %q", c.status, body["code"], c.code)
		}
		if body["msg"] != "because" {
			t.Errorf("status %d: msg = %v, want the message", c.status, body["msg"])
		}
		// The pre-spec pair, which a frozen document reads.
		if body["error"] != "because" {
			t.Errorf("status %d: error = %v, want the message", c.status, body["error"])
		}
		if body["ok"] != false {
			t.Errorf("status %d: ok = %v, want false", c.status, body["ok"])
		}
	}
}

// A status §3 does not name carries NO code. The registry is open, so a host may add a
// name it genuinely needs, but a client branches on the value, and a name invented here
// on the way past is worse than an absent field.
func TestWriteErrorInventsNoCode(t *testing.T) {
	s := &Server{}

	// 409 is here rather than in the table above on purpose. §6 gives `conflict` one
	// meaning, the If-Match refusal, which writes its own body. This host's only other
	// 409 is the truncation guard, and it lifts itself, so calling it a conflict made
	// clayjs suspend autosave over a condition that had already cleared.
	for _, status := range []int{http.StatusBadRequest, http.StatusConflict, http.StatusInternalServerError} {
		w := httptest.NewRecorder()
		s.writeError(w, status, "because")

		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if _, present := body["code"]; present {
			t.Errorf("status %d: carries code %v, want none", status, body["code"])
		}
		if body["msg"] != "because" {
			t.Errorf("status %d: msg = %v", status, body["msg"])
		}
	}
}

// §11's channel addresses a document the way §3 does. Reading only the pre-spec header
// meant a client written against the spec named no target at all.
func TestWireTargetReadsDocumentURL(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	f := registerPageWithContent(t, srv, "wire-target.html",
		"<!DOCTYPE html><html lang=\"en\"><body>hi</body></html>")

	for _, name := range []string{"Document-URL", "Page-URL"} {
		r := httptest.NewRequest(http.MethodPost, "/_/wire", strings.NewReader(""))
		r.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
		r.Header.Set(name, fmt.Sprintf("http://127.0.0.1:%d/%s", srv.port, f.RelPath))

		got, ok := srv.wireTarget(r, true, "")
		if !ok {
			t.Fatalf("%s: no target resolved", name)
		}
		if got.AbsPath != f.AbsPath {
			t.Errorf("%s: resolved %s, want %s", name, got.AbsPath, f.AbsPath)
		}
	}
}

// §5 puts a fact about one document inside the `document` block, so a client can tell
// it from a fact about the host. This host answered with them at the top level only, so
// a client reading only the spec found an almost empty block.
//
// Both places carry them now. The top level is what every shipped HTML Clay answered
// with, and this route exists for the reader that cannot be updated: a sandboxed
// document's own inline script, frozen into the file at its first save.
func TestMetaRepeatsDocumentFactsInTheDocumentBlock(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	f := registerPageWithContent(t, srv, "meta-shape.html",
		"<!DOCTYPE html><html lang=\"en\"><body>hi</body></html>")

	body := decode(t, getThroughMux(t, srv, "/_/meta/"+f.Token))

	doc, ok := body["document"].(map[string]any)
	if !ok {
		t.Fatal("no document block in the meta answer")
	}

	for _, key := range []string{"path", "absolutePath", "name", "size", "lastModified"} {
		if _, present := doc[key]; !present {
			t.Errorf("document block is missing %q", key)
		}
		if doc[key] != body[key] {
			t.Errorf("%q disagrees: document %v, top level %v", key, doc[key], body[key])
		}
	}

	// The top level keeps answering. This is the half a frozen document reads.
	if body["name"] != f.Name {
		t.Errorf("top-level name = %v, want %q", body["name"], f.Name)
	}
}
