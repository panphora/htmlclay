package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/panphora/htmlclay/internal/session"
)

// registerNestedPage writes and registers a page inside its own folder, which is what
// the scope test needs: two documents with one name are the case a scope built from
// the file name would get wrong, and the case a scope built from the path or the id
// gets right. Nothing is served here, so neither file has been given a tracked id yet
// and both take the path branch of the identity rule.
func registerNestedPage(t *testing.T, srv *Server, dirName, name, content string) *session.File {
	t.Helper()
	dir := filepath.Join(srv.sessions.HomeDir(), dirName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", dirName, err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	f, err := srv.sessions.Register(p, session.ViaOsOpen)
	if err != nil {
		t.Fatalf("register %s: %v", p, err)
	}
	return f
}

func awarenessOf(t *testing.T, meta map[string]any) map[string]any {
	t.Helper()
	doc, ok := meta["document"].(map[string]any)
	if !ok {
		t.Fatalf("no document block in the meta answer: %v", meta)
	}
	awareness, ok := doc["awareness"].(map[string]any)
	if !ok {
		t.Fatalf("no awareness in the document block: %v", doc)
	}
	return awareness
}

// HTML Clay has no presence relay and never will: a document's tabs, and the people in
// them, are in one browser on one machine. So it advertises the capability names a
// client gates on AND says where the frames actually travel, which is the only thing
// that keeps a client from opening an HTTP endpoint this host does not serve. The
// label is fixed and honest: these are tabs on this computer, not remote accounts.
func TestMetaAwarenessAdvertisesTheSameBrowserTransport(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	meta := metaOf(t, srv, f)

	announced := map[string]bool{}
	if exts, ok := meta["extensions"].([]any); ok {
		for _, e := range exts {
			if name, ok := e.(string); ok {
				announced[name] = true
			}
		}
	}
	for _, name := range []string{"page-awareness-v1", "record-awareness-v1"} {
		if !announced[name] {
			t.Errorf("host does not announce %q: %v", name, meta["extensions"])
		}
	}
	if announced["presence"] {
		t.Errorf("host announces presence, which names a relay it does not have and turns off ClayJS's shared sync worker: %v", meta["extensions"])
	}

	awareness := awarenessOf(t, meta)
	if got := awareness["transport"]; got != "broadcast" {
		t.Errorf("transport = %v, want broadcast: this host relays nothing", got)
	}
	if got := awareness["label"]; got != "This computer" {
		t.Errorf("label = %v, want %q", got, "This computer")
	}
	if scope, _ := awareness["scope"].(string); scope == "" {
		t.Errorf("no scope: a client cannot name its channel without one: %v", awareness)
	}
}

// The advertisement must not read as an HTTP presence route. `awareness` carries a
// transport, a scope and a label; any URL, path or route in it is a client's
// invitation to probe an endpoint this host does not have, and the probe is how a
// presence delivery silently becomes no presence at all.
func TestMetaAwarenessAdvertisesNoHTTPPresenceEndpoint(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	w := getThroughMux(t, srv, "/_/meta/"+f.Token)
	body := w.Body.String()

	for _, endpoint := range []string{"/_/presence", "presenceUrl", "presencePath", "presenceEndpoint"} {
		if strings.Contains(body, endpoint) {
			t.Errorf("the meta answer names an HTTP presence destination %q: %q", endpoint, body)
		}
	}

	awareness := awarenessOf(t, decode(t, w))
	for key := range awareness {
		switch key {
		case "transport", "scope", "label":
		default:
			t.Errorf("awareness carries an unplanned field %q: %v", key, awareness)
		}
	}
	if _, present := awareness["url"]; present {
		t.Errorf("awareness names a url: %v", awareness)
	}

	// The capability names are the only place presence may be mentioned, and they are
	// names of a client behaviour, not of a route this host answers.
	exts, _ := decode(t, getThroughMux(t, srv, "/_/meta"))["extensions"].([]any)
	for _, e := range exts {
		if name, ok := e.(string); ok && strings.Contains(name, "/") {
			t.Errorf("an extension name is a route, not a capability: %q", name)
		}
	}
}

// Two documents called index.htmlclay in two folders are two documents. A channel
// named for the file name would draw one document's peers into the other, which is
// exactly the merge the plan forbids.
func TestMetaAwarenessScopesTwoDocumentsApart(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	drafts := registerNestedPage(t, srv, "drafts", "index.htmlclay",
		`<!DOCTYPE html><html lang="en"><body>draft</body></html>`)
	archive := registerNestedPage(t, srv, "archive", "index.htmlclay",
		`<!DOCTYPE html><html lang="en"><body>archive</body></html>`)

	draftsScope, _ := awarenessOf(t, metaOf(t, srv, drafts))["scope"].(string)
	archiveScope, _ := awarenessOf(t, metaOf(t, srv, archive))["scope"].(string)

	if draftsScope == "" || archiveScope == "" {
		t.Fatalf("missing scope: drafts %q, archive %q", draftsScope, archiveScope)
	}
	if draftsScope == archiveScope {
		t.Errorf("two documents share one scope %q: their peers would see each other", draftsScope)
	}
	if draftsScope == drafts.Name {
		t.Errorf("scope is the bare file name %q, which merges every folder's copy", draftsScope)
	}
}

// The scope is per document and does not drift between two answers for it: a client
// that asked twice and joined two channels would leave its own roster behind.
func TestMetaAwarenessScopeIsStableForOneDocument(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	first, _ := awarenessOf(t, metaOf(t, srv, f))["scope"].(string)
	second, _ := awarenessOf(t, metaOf(t, srv, f))["scope"].(string)
	if first != second {
		t.Errorf("scope moved between two answers: %q then %q", first, second)
	}
}

// Where the host supplies an id, the scope is that id and not the path: the same
// document reached through a rename keeps its channel, and the client already files
// its own per-document records under the same identity.
func TestMetaAwarenessScopeUsesTheDocumentIDWhereSupplied(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	f := registerNestedPage(t, srv, "notes", "tracked.htmlclay",
		`<!DOCTYPE html><html documentid="`+stableUUID+`" lang="en"><body>tracked</body></html>`)

	scope, _ := awarenessOf(t, metaOf(t, srv, f))["scope"].(string)
	if !strings.Contains(scope, stableUUID) {
		t.Errorf("scope %q does not carry the document id %q", scope, stableUUID)
	}
}
