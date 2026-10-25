package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// versionsFromMeta reads document.versions out of the token metadata answer, the
// way a client deciding whether an upgrade can be undone does. The field is a
// pointer so a block that omits it is distinguishable from one that says false:
// absent means this host keeps no history, and a client that cannot tell the two
// apart would promise a rollback it cannot perform.
func versionsFromMeta(t *testing.T, body []byte) bool {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatalf("decode meta %q: %v", body, err)
	}
	if _, dup := top["versions"]; dup {
		t.Errorf("meta carries a top-level versions as well as the document block: %q", body)
	}
	var out struct {
		Document *struct {
			Versions *bool `json:"versions"`
		} `json:"document"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode meta %q: %v", body, err)
	}
	if out.Document == nil {
		t.Fatalf("meta carries no document block: %q", body)
	}
	if out.Document.Versions == nil {
		t.Fatalf("document block carries no versions: %q", body)
	}
	return *out.Document.Versions
}

// The claim is checked against the route it describes rather than taken on the
// host's word: a save is made, the history listing is read through the real
// version route, and only then is the announcement believed. A host that said
// versions were kept while /_/versions stayed empty would be promising a rollback
// that does not exist.
func TestMetaVersionsSaysHistoryIsKept(t *testing.T) {
	srv, f, content := setupHandlerTest(t)

	if names := listVersionNames(t, srv, f); len(names) != 0 {
		t.Fatalf("history is not empty before any save: %v", names)
	}

	edited := strings.Replace(content, "Hello", "Goodbye", 1)
	w := saveThroughMux(t, srv, f, edited, "")
	if w.Code != 200 {
		t.Fatalf("save: got %d, want 200 (%q)", w.Code, w.Body.String())
	}
	if names := listVersionNames(t, srv, f); len(names) == 0 {
		t.Fatalf("no entry in /_/versions after a save: this host keeps no history to announce")
	}

	meta := getThroughMux(t, srv, "/_/meta/"+f.Token)
	if meta.Code != 200 {
		t.Fatalf("meta: got %d, want 200 (%q)", meta.Code, meta.Body.String())
	}
	if !versionsFromMeta(t, meta.Body.Bytes()) {
		t.Errorf("document.versions is not true on a host that keeps history: %q", meta.Body.String())
	}
}

// A caller who may not see the document is told nothing about its history: the
// field lives in the document block, so withholding that block withholds this
// with it.
func TestMetaVersionsWithheldWithTheDocumentBlock(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)

	unauthorized := getThroughMux(t, srv, "/_/meta/not-a-token")
	if unauthorized.Code != 401 {
		t.Errorf("meta with an unknown token: got %d, want 401 (%q)", unauthorized.Code, unauthorized.Body.String())
	}
	if strings.Contains(unauthorized.Body.String(), "versions") {
		t.Errorf("refused meta still reports version history: %q", unauthorized.Body.String())
	}

	host := getThroughMux(t, srv, "/_/meta")
	if host.Code != 200 {
		t.Fatalf("tokenless meta: got %d, want 200", host.Code)
	}
	if strings.Contains(host.Body.String(), "versions") {
		t.Errorf("tokenless meta reports version history: %q", host.Body.String())
	}
}
