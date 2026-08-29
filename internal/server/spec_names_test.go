package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/panphora/htmlclay/internal/htmlutil"
	"github.com/panphora/htmlclay/internal/session"
)

// The pre-spec attribute names, assembled rather than written out. A rename sweep
// across the test tree is what created the need for these tests and is also what
// would silently rewrite them into the new spelling, leaving assertions that pass
// while testing nothing.
const oldTokenAttr = "htmlclay" + "token"
const oldIDAttr = "htmlclay" + "id"

// Real UUIDs: ResolveIdentity adopts a disk id only when it is canonical, and
// mints a fresh one otherwise. A readable placeholder here would be silently
// discarded, and the test would pass on a build with no fallback at all.
const legacyUUID = "a1b2c3d4-1111-4222-8333-444455556666"
const stableUUID = "b2c3d4e5-2222-4333-9444-555566667777"

func registerPageWithContent(t *testing.T, srv *Server, name, content string) *session.File {
	t.Helper()
	p := filepath.Join(srv.sessions.HomeDir(), name)
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	f, err := srv.sessions.Register(p, session.ViaOsOpen)
	if err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
	return f
}

func getThroughMux(t *testing.T, srv *Server, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", target, nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)
	return w
}

// --- tokenless discovery, spec §5 -----------------------------------------

// The route is exercised through the real mux, because the thing most likely to
// break it is routing rather than the handler: "/_/meta" does not match the
// "/_/meta/{token}" pattern, so without its own registration it falls to the file
// catch-all and comes back 403. §5 makes a client strict about what counts as an
// answer, so anything but a 2xx capability document is read as "this host offers
// no discovery" and the client silently drops to plain saves.
//
// Not hypothetical: both clients already send this request whenever the page
// carries no token, which on this host is every read-only serve.
func TestHostMetaAnsweredWithoutAToken(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)

	w := getThroughMux(t, srv, "/_/meta")
	if w.Code != 200 {
		t.Fatalf("tokenless discovery: got %d, want 200", w.Code)
	}

	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("not a JSON object: %v (%q)", err, w.Body.String())
	}
	if got["spec"] != float64(specVersion) {
		t.Errorf("spec: got %v, want %d", got["spec"], specVersion)
	}
	exts, ok := got["extensions"].([]any)
	if !ok || len(exts) == 0 {
		t.Fatalf("no extensions listed: %q", w.Body.String())
	}
}

// §5: the document block is the only part a host withholds, and it is withheld by
// omission. This route names no document, so it must describe none. Emitting the
// per-file fields empty would answer with a zero-byte nameless file, which is a
// worse answer than saying nothing.
func TestHostMetaDescribesNoDocument(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)

	w := getThroughMux(t, srv, "/_/meta")
	// Checked before the field sweep, and not incidental: with this route
	// unregistered the path falls to the file catch-all, which answers 403 with a
	// JSON error body. That body parses and contains none of the fields below, so
	// a sweep on its own would pass on a host with no discovery at all.
	if w.Code != 200 {
		t.Fatalf("discovery: got %d, want 200 (%q)", w.Code, w.Body.String())
	}

	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, perDocument := range []string{"document", "path", "absolutePath", "name", "size", "lastModified", "etag"} {
		if _, present := got[perDocument]; present {
			t.Errorf("host-scope discovery leaked the per-document field %q: %q", perDocument, got)
		}
	}
}

// Both routes answer the same question about the host, so they must not drift.
// This is the test that keeps the extension list in one place: adding `conditional`
// to one route and not the other would leave half this host's clients saving
// blindly.
func TestBothMetaRoutesAgreeOnHostScope(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	var host, perFile map[string]any
	if err := json.Unmarshal(getThroughMux(t, srv, "/_/meta").Body.Bytes(), &host); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(getThroughMux(t, srv, "/_/meta/"+f.Token).Body.Bytes(), &perFile); err != nil {
		t.Fatal(err)
	}

	if fmt.Sprint(host["spec"]) != fmt.Sprint(perFile["spec"]) {
		t.Errorf("spec differs: tokenless %v, token %v", host["spec"], perFile["spec"])
	}
	if fmt.Sprint(host["extensions"]) != fmt.Sprint(perFile["extensions"]) {
		t.Errorf("extensions differ: tokenless %v, token %v", host["extensions"], perFile["extensions"])
	}
}

// A bad token is a per-document reason, and §5 forbids answering one with a 404:
// the client reads that as a spec-unaware host and loses every capability this one
// really has. The tokenless route is what stays available, so a client that gets
// nowhere with its token can still discover the host.
func TestHostMetaStillAnswersWhenATokenDoesNot(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)

	if w := getThroughMux(t, srv, "/_/meta/not-a-real-token"); w.Code == 200 {
		t.Fatal("a bogus token was accepted; this test proves nothing")
	}
	if w := getThroughMux(t, srv, "/_/meta"); w.Code != 200 {
		t.Errorf("discovery unavailable without a valid token: got %d", w.Code)
	}
}

// --- legacy attribute names, spec §9 --------------------------------------

// A page served by a build from before the rename holds `htmlclaytoken`. If that
// page is still open when the file is saved, the strip has to recognise the old
// name or it writes a live credential into the file on disk.
func TestSaveStripsALegacyToken(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	body := `<!DOCTYPE html><html ` + oldTokenAttr + `="` + f.Token + `"><body>from an older tab</body></html>`
	req := httptest.NewRequest("POST", "/_/save/"+f.Token, strings.NewReader(body))
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	sameOriginHeaders(req)

	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("save: got %d, want 200", w.Code)
	}

	saved, err := os.ReadFile(f.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), oldTokenAttr) {
		t.Error("a live save token was written to disk under its legacy name")
	}
	if strings.Contains(string(saved), f.Token) {
		t.Error("the token value reached disk")
	}
	if !strings.Contains(string(saved), "from an older tab") {
		t.Error("the save did not land")
	}
}

// Every .htmlclay file saved before the rename carries `htmlclayid` on disk, and
// its whole version history is filed under that value. Serving must read it,
// because a file whose id is not recognised is given a fresh one and loses every
// version ever taken of it.
func TestServeAdoptsALegacyDocumentID(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	f := registerPageWithContent(t, srv, "legacy.htmlclay",
		`<!DOCTYPE html><html `+oldIDAttr+`="`+legacyUUID+`" lang="en"><body>old file</body></html>`)

	w := getThroughMux(t, srv, "/"+f.RelPath)
	if w.Code != 200 {
		t.Fatalf("serve: got %d, want 200", w.Code)
	}

	served := w.Body.Bytes()
	if got := htmlutil.ReadHTMLClayID(served); got != legacyUUID {
		t.Errorf("serving lost the legacy identity: got %q, want %q", got, legacyUUID)
	}
	// Serving re-anchors the id under the current spelling, so the file migrates
	// on the client's next save. One id, not two: a document carrying both would
	// answer differently depending on which name a reader looked for first.
	if strings.Contains(string(served), oldIDAttr) {
		t.Errorf("the served document carries both spellings of its id: %q", served)
	}
	if !strings.Contains(string(served), `documentid="`+legacyUUID+`"`) {
		t.Errorf("the served document carries no current-spelling id: %q", served)
	}
}

// The history key is the reason the fallback exists. Serving a legacy file and then
// saving it must leave both operations pointed at the same history, which is what
// a client upgrading in place actually does.
func TestLegacyDocumentKeepsItsHistoryAcrossASave(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	f := registerPageWithContent(t, srv, "legacy2.htmlclay",
		`<!DOCTYPE html><html `+oldIDAttr+`="`+stableUUID+`" lang="en"><body>v1</body></html>`)

	if w := getThroughMux(t, srv, "/"+f.RelPath); w.Code != 200 {
		t.Fatalf("serve: got %d", w.Code)
	}
	keyBefore := f.HistoryKey()
	if keyBefore != "id:"+stableUUID {
		t.Fatalf("history key did not follow the legacy id: got %q", keyBefore)
	}

	body := `<!DOCTYPE html><html documentid="` + stableUUID + `" lang="en"><body>v2</body></html>`
	req := httptest.NewRequest("POST", "/_/save/"+f.Token, strings.NewReader(body))
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	sameOriginHeaders(req)
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("save: got %d, want 200", w.Code)
	}

	if got := f.HistoryKey(); got != keyBefore {
		t.Errorf("the save moved the file's history: %q became %q", keyBefore, got)
	}
}

// The per-document meta answer reports the id under the same name the attribute
// uses. Nothing pinned this field before, which is how it could have kept the old
// spelling while the attribute moved.
func TestTokenMetaReportsTheDocumentIDUnderItsCurrentName(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	f := registerPageWithContent(t, srv, "meta-id.htmlclay",
		`<!DOCTYPE html><html documentid="`+stableUUID+`" lang="en"><body>hi</body></html>`)

	// Serving is what resolves and tracks the identity; meta reports what it found.
	if w := getThroughMux(t, srv, "/"+f.RelPath); w.Code != 200 {
		t.Fatalf("serve: got %d", w.Code)
	}

	var got map[string]any
	w := getThroughMux(t, srv, "/_/meta/"+f.Token)
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("meta: %v (%q)", err, w.Body.String())
	}
	if got["documentid"] != stableUUID {
		t.Errorf("documentid: got %v, want %q (%q)", got["documentid"], stableUUID, w.Body.String())
	}
	if _, stale := got[oldIDAttr]; stale {
		t.Errorf("meta still reports the id under its pre-spec name: %q", w.Body.String())
	}
}

// The whole point of serving both token spellings: a document written before the
// rename reads the old name in its own inline script, and no update can reach that
// script. This walks the path such a document takes — serve, read the attribute by
// the only name it knows, save with what it found — and it is the test that fails
// if the legacy name is ever dropped from the injection.
//
// Not hypothetical. ~/htmlclay/examples/welcome.htmlclay is written once and never
// overwritten on upgrade, so every existing user has one of these on disk.
func TestADocumentThatKnowsOnlyTheOldTokenNameStillSaves(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	f := registerPageWithContent(t, srv, "old-client.htmlclay",
		`<!DOCTYPE html><html lang="en"><body><p>v1</p>`+
			`<script>const t = document.documentElement.getAttribute('`+oldTokenAttr+`');</script>`+
			`</body></html>`)

	w := getThroughMux(t, srv, "/"+f.RelPath)
	if w.Code != 200 {
		t.Fatalf("serve: got %d, want 200", w.Code)
	}

	// Read the attribute exactly as that inline script would, by its old name only.
	m := regexp.MustCompile(oldTokenAttr + `="([^"]+)"`).FindSubmatch(w.Body.Bytes())
	if m == nil {
		t.Fatalf("a document reading %s finds no token in the served page: %q",
			oldTokenAttr, rootTagOf(w.Body.Bytes()))
	}

	body := `<!DOCTYPE html><html lang="en"><body><p>v2</p></body></html>`
	req := httptest.NewRequest("POST", "/_/save/"+string(m[1]), strings.NewReader(body))
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	sameOriginHeaders(req)
	sw := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(sw, req)
	if sw.Code != 200 {
		t.Fatalf("save with the legacy-named token: got %d, want 200 (%q)", sw.Code, sw.Body.String())
	}

	saved, err := os.ReadFile(f.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), "v2") {
		t.Error("the save did not land")
	}
}

// Both names carry ONE value. Two different credentials on one tag would mean a
// reader's choice of name decided which one it saved with.
func TestTheTwoTokenSpellingsCarryTheSameValue(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	root := rootTagOf(getThroughMux(t, srv, "/"+f.RelPath).Body.Bytes())
	var seen []string
	for _, name := range []string{"savetoken", oldTokenAttr} {
		m := regexp.MustCompile(name + `="([^"]*)"`).FindStringSubmatch(root)
		if m == nil {
			t.Fatalf("the served root carries no %s: %q", name, root)
		}
		seen = append(seen, m[1])
	}
	if seen[0] != seen[1] {
		t.Errorf("the two spellings carry different credentials: %q vs %q", seen[0], seen[1])
	}
	if seen[0] != f.Token {
		t.Errorf("neither is this file's token: got %q, want %q", seen[0], f.Token)
	}
}

func rootTagOf(data []byte) string {
	s := string(data)
	i := strings.Index(s, "<html")
	if i < 0 {
		return s
	}
	j := strings.Index(s[i:], ">")
	if j < 0 {
		return s[i:]
	}
	return s[i : i+j+1]
}
