package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/panphora/htmlclay/internal/session"
	"github.com/panphora/htmlclay/internal/specwire"
)

// dataroutes_write_test.go covers POST /_/api/<path>: the trust model (local processes in, browsers
// only with the target file's Save-Token), the engine's refusals surfaced as bodies, and the
// commit itself, which must be exactly a save's.

// dataWriteDoc is the test document: one rules tag named "api" over a heading, a list and a URL
// attribute.
const dataWriteDoc = `<!DOCTYPE html>
<html><head><script type="application/json" data-rules-name="api" data-rules-version="1">{title:"h1",items:"li[]",link:"a@href"}</script></head>
<body>
<h1>Hello</h1>
<ul><li>A</li><li>B</li></ul>
<a href="/x">x</a>
</body></html>
`

// postDataWrite drives the full stack, HostValidationMiddleware, the mux, then the handler, so the
// routing of the write face is exercised rather than assumed. The default headers are a local
// process's: no Origin and no Sec-Fetch-Site.
func postDataWrite(t *testing.T, srv *Server, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", target, strings.NewReader(body))
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)
	return w
}

type decodedWriteError struct {
	Error   string          `json:"error"`
	Message string          `json:"message"`
	Details json.RawMessage `json:"details"`
}

func decodeWriteError(t *testing.T, w *httptest.ResponseRecorder) decodedWriteError {
	t.Helper()
	var body decodedWriteError
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body is not JSON (%d): %s", w.Code, w.Body.String())
	}
	return body
}

func readDoc(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// A local process writes through the page's own rules tag, content only, and the answer is the
// fresh extraction with the stamp of what landed on disk.
func TestDataWriteAppliesTheBodyThroughThePagesRulesTag(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	path := writeHome(t, srv, "test.htmlclay", dataWriteDoc)

	w := postDataWrite(t, srv, "/_/api/test.htmlclay", `{"title":"World"}`, nil)
	if w.Code != 200 {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != `{"title":"World","items":["A","B"],"link":"/x"}` {
		t.Errorf("body = %s", got)
	}

	after := readDoc(t, path)
	if want := specwire.Etag([]byte(after)); w.Header().Get("ETag") != want {
		t.Errorf("ETag = %q, want the stamp of the stored bytes %q", w.Header().Get("ETag"), want)
	}

	// The splice promise, stated on the bytes rather than on the parse: everything outside the
	// changed element is bit-for-bit what it was, so a write cannot reformat a document it did not
	// mean to touch.
	start := strings.Index(dataWriteDoc, "<h1>") + len("<h1>")
	end := strings.Index(dataWriteDoc, "</h1>")
	prefix, suffix := dataWriteDoc[:start], dataWriteDoc[end:]
	if !strings.HasPrefix(after, prefix) {
		t.Errorf("the bytes before the heading changed:\n%q\n%q", prefix, after)
	}
	if !strings.HasSuffix(after, suffix) {
		t.Errorf("the bytes after the heading changed:\n%q\n%q", suffix, after)
	}
	if len(after) > len(prefix)+len(suffix) {
		if mid := after[len(prefix) : len(after)-len(suffix)]; mid != "World" {
			t.Errorf("inside the heading = %q, want %q", mid, "World")
		}
	}
}

// A body that changes nothing writes nothing: no bytes, no mtime, no backup.
func TestDataWriteNoOpLeavesTheFileAlone(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	path := writeHome(t, srv, "test.htmlclay", dataWriteDoc)

	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	w := postDataWrite(t, srv, "/_/api/test.htmlclay", `{"title":"Hello"}`, nil)
	if w.Code != 200 {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if got := readDoc(t, path); got != dataWriteDoc {
		t.Errorf("the file changed:\n%s", got)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("mtime moved from %v to %v", before.ModTime(), after.ModTime())
	}
}

// A page from another origin, and a page on another port of this same host, is refused before any
// path work happens at all.
func TestDataWriteRefusesOtherSites(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	path := writeHome(t, srv, "test.htmlclay", dataWriteDoc)

	w := postDataWrite(t, srv, "/_/api/test.htmlclay", `{"title":"World"}`,
		map[string]string{"Sec-Fetch-Site": "same-site"})
	if w.Code != 403 {
		t.Fatalf("status = %d, want 403: %s", w.Code, w.Body.String())
	}
	if got := readDoc(t, path); got != dataWriteDoc {
		t.Errorf("a refused write changed the file:\n%s", got)
	}
}

// The rule SECURITY.md states: a silent background fetch() from a page in a trusted folder cannot
// write a sibling, because the write needs the TARGET file's own Save-Token.
func TestDataWriteBrowserNeedsTheTargetFilesSaveToken(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)
	path := writeHome(t, srv, "test.htmlclay", dataWriteDoc)

	otherPath := writeHome(t, srv, "other.htmlclay", dataWriteDoc)
	other, err := srv.sessions.Register(otherPath, session.ViaOsOpen)
	if err != nil {
		t.Fatal(err)
	}

	browser := func(extra map[string]string) map[string]string {
		headers := map[string]string{
			"Sec-Fetch-Site": "same-origin",
			"Origin":         fmt.Sprintf("http://127.0.0.1:%d", srv.port),
		}
		for k, v := range extra {
			headers[k] = v
		}
		return headers
	}

	noToken := postDataWrite(t, srv, "/_/api/test.htmlclay", `{"title":"World"}`, browser(nil))
	if noToken.Code != 403 {
		t.Fatalf("no token: status = %d, want 403: %s", noToken.Code, noToken.Body.String())
	}
	if got := decodeWriteError(t, noToken).Error; got != "Save-Token required" {
		t.Errorf("no token: error = %q", got)
	}

	withToken := postDataWrite(t, srv, "/_/api/test.htmlclay", `{"title":"World"}`,
		browser(map[string]string{helperTokenHeader: f.Token}))
	if withToken.Code != 200 {
		t.Fatalf("the file's own token: status = %d, want 200: %s", withToken.Code, withToken.Body.String())
	}

	wrongToken := postDataWrite(t, srv, "/_/api/test.htmlclay", `{"title":"Again"}`,
		browser(map[string]string{helperTokenHeader: other.Token}))
	if wrongToken.Code != 403 {
		t.Fatalf("a sibling's token: status = %d, want 403: %s", wrongToken.Code, wrongToken.Body.String())
	}
	if got := readDoc(t, path); !strings.Contains(got, "<h1>World</h1>") {
		t.Errorf("the refused write changed the file:\n%s", got)
	}
}

func TestDataWriteRequiresJSON(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	writeHome(t, srv, "test.htmlclay", dataWriteDoc)

	w := postDataWrite(t, srv, "/_/api/test.htmlclay", `{"title":"World"}`,
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if w.Code != 415 {
		t.Fatalf("status = %d, want 415: %s", w.Code, w.Body.String())
	}
	if got := decodeWriteError(t, w).Error; got != "Unsupported Media Type" {
		t.Errorf("error = %q", got)
	}
}

func TestDataWriteInvalidJSON(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	writeHome(t, srv, "test.htmlclay", dataWriteDoc)

	w := postDataWrite(t, srv, "/_/api/test.htmlclay", `{title:"World"}`, nil)
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if got := decodeWriteError(t, w).Error; got != "Invalid JSON body" {
		t.Errorf("error = %q", got)
	}
}

// The engine's refusals reach the caller structured, not as a sentence: a caller that sent a key
// the rules do not define is told which one.
func TestDataWriteRejectedCarriesTheUnknownKeys(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	path := writeHome(t, srv, "test.htmlclay", dataWriteDoc)

	w := postDataWrite(t, srv, "/_/api/test.htmlclay", `{"nmae":"x"}`, nil)
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	body := decodeWriteError(t, w)
	if body.Error != "Write rejected" {
		t.Fatalf("error = %q", body.Error)
	}
	var details struct {
		UnknownKeys []string `json:"unknownKeys"`
	}
	if err := json.Unmarshal(body.Details, &details); err != nil {
		t.Fatalf("details is not the structured refusal: %s (%v)", body.Details, err)
	}
	if len(details.UnknownKeys) != 1 || details.UnknownKeys[0] != "nmae" {
		t.Errorf("unknownKeys = %v, want [nmae]", details.UnknownKeys)
	}
	if got := readDoc(t, path); got != dataWriteDoc {
		t.Errorf("a rejected write changed the file:\n%s", got)
	}
}

// Content only: a URL the policy refuses is refused whole, and nothing lands on disk.
func TestDataWriteRefusedLeavesTheFileAlone(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	path := writeHome(t, srv, "test.htmlclay", dataWriteDoc)

	w := postDataWrite(t, srv, "/_/api/test.htmlclay", `{"link":"javascript:alert(1)"}`, nil)
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if got := decodeWriteError(t, w).Error; got != "Write refused" {
		t.Errorf("error = %q", got)
	}
	if got := readDoc(t, path); got != dataWriteDoc {
		t.Errorf("a refused write changed the file:\n%s", got)
	}
}

// A write may carry the stamp of the bytes it read, and then it is judged against them before
// anything is versioned or written.
func TestDataWriteIfMatch(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	path := writeHome(t, srv, "test.htmlclay", dataWriteDoc)

	stale := postDataWrite(t, srv, "/_/api/test.htmlclay", `{"title":"World"}`,
		map[string]string{"If-Match": `"wrong"`})
	if stale.Code != 412 {
		t.Fatalf("status = %d, want 412: %s", stale.Code, stale.Body.String())
	}
	if want := specwire.Etag([]byte(dataWriteDoc)); stale.Header().Get("ETag") != want {
		t.Errorf("412 ETag = %q, want the current stamp %q", stale.Header().Get("ETag"), want)
	}
	if got := readDoc(t, path); got != dataWriteDoc {
		t.Fatalf("a refused conditional write changed the file:\n%s", got)
	}

	// The stamp the read face hands out is exactly the one the write face accepts.
	etag := getAPI(t, srv, "test.htmlclay").Header().Get("ETag")
	if etag == "" {
		t.Fatal("GET /_/api/<file> carried no ETag")
	}
	fresh := postDataWrite(t, srv, "/_/api/test.htmlclay", `{"title":"World"}`,
		map[string]string{"If-Match": etag})
	if fresh.Code != 200 {
		t.Fatalf("If-Match from the GET: status = %d, want 200: %s", fresh.Code, fresh.Body.String())
	}
}

// The commit is a save's commit, which is what makes the write recoverable: the previous content is
// in history before the file is replaced.
func TestDataWriteVersionsThePreviousContent(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)
	writeHome(t, srv, "test.htmlclay", dataWriteDoc)

	if w := postDataWrite(t, srv, "/_/api/test.htmlclay", `{"title":"World"}`, nil); w.Code != 200 {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}

	f.Lock()
	key := f.HistoryKey()
	f.Unlock()
	if key == "" {
		t.Fatal("the write resolved no history key")
	}
	entries, err := srv.versions.List(key, f.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("a changed write left no version behind")
	}
	// Both states, not just one: the pre-write content is what a caller would
	// want back, and the written content is what the receipt promises.
	before, after := false, false
	for _, e := range entries {
		body, err := srv.versions.Read(key, f.AbsPath, e.Name)
		if err != nil {
			t.Fatal(err)
		}
		before = before || strings.Contains(string(body), "<h1>Hello</h1>")
		after = after || strings.Contains(string(body), "<h1>World</h1>")
	}
	if !before || !after {
		t.Errorf("history holds the old content: %v, the written content: %v; want both", before, after)
	}
}

// The refusal for a path this route may not write is decided on text alone: it reads the same
// whether or not anything is at that path, so the route cannot be used to enumerate the home tree.
func TestDataWriteUnregisteredIsNotAnExistenceOracle(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	writeHome(t, srv, "loose.htmlclay", dataWriteDoc)

	present := postDataWrite(t, srv, "/_/api/loose.htmlclay", `{"title":"World"}`, nil)
	absent := postDataWrite(t, srv, "/_/api/absent.htmlclay", `{"title":"World"}`, nil)

	for name, w := range map[string]*httptest.ResponseRecorder{"present": present, "absent": absent} {
		if w.Code != 403 {
			t.Fatalf("%s: status = %d, want 403: %s", name, w.Code, w.Body.String())
		}
		if got := decodeWriteError(t, w).Error; got != "Not writable" {
			t.Errorf("%s: error = %q", name, got)
		}
	}
	if present.Body.String() != absent.Body.String() {
		t.Errorf("the refusals differ:\npresent %s\nabsent  %s", present.Body.String(), absent.Body.String())
	}
	if got := readDoc(t, filepath.Join(srv.sessions.HomeDir(), "loose.htmlclay")); got != dataWriteDoc {
		t.Errorf("a refused write changed the file:\n%s", got)
	}
}

// The write face owns the same address as the read face, including the bare /_/api that ServeMux
// would otherwise 307-redirect, and answers its own path errors before any filesystem access.
func TestDataWriteRouting(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	writeHome(t, srv, "test.htmlclay", dataWriteDoc)

	cases := []struct {
		target, wantError string
		wantStatus        int
	}{
		{"/_/api", "Missing path", 400},
		{"/_/api/", "Missing path", 400},
		{"/_/api/style.css", "Unsupported file type", 404},
	}
	for _, c := range cases {
		t.Run(c.target, func(t *testing.T) {
			w := postDataWrite(t, srv, c.target, `{"title":"World"}`, nil)
			if w.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d: %s", w.Code, c.wantStatus, w.Body.String())
			}
			if got := decodeWriteError(t, w).Error; got != c.wantError {
				t.Errorf("error = %q, want %q", got, c.wantError)
			}
		})
	}
}

// A trusted folder's file registers on demand, through the same seam a navigation uses, so a local
// script can write a document the user never opened.
func TestDataWriteRegistersATrustedFolderFile(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	ws := filepath.Join(srv.sessions.HomeDir(), "ws")
	page := writeHome(t, srv, "ws/note.htmlclay", dataWriteDoc)

	srv.SetHooks(Hooks{
		TrustedCovers: func(absPath string) bool { return session.EqualOrUnder(absPath, ws) },
		Route: func(absPath string) (string, bool) {
			if _, err := srv.sessions.Register(absPath, session.ViaTrusted); err != nil {
				return "", false
			}
			return fmt.Sprintf("http://127.0.0.1:%d/", srv.port), true
		},
	})
	if err := srv.sessions.InstallTrustedRoot(ws); err != nil {
		t.Fatal(err)
	}

	w := postDataWrite(t, srv, "/_/api/ws/note.htmlclay", `{"title":"World"}`, nil)
	if w.Code != 200 {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if _, registered := srv.sessions.LookupByPath(page); !registered {
		t.Error("the write did not register the file")
	}
	if got := readDoc(t, page); !strings.Contains(got, "<h1>World</h1>") {
		t.Errorf("the write did not land:\n%s", got)
	}
}

// A page can only write a file it already holds a token for, so the write face never registers on
// a browser's behalf and never answers differently for a trusted file than for a missing one: the
// three leaks the review found all needed the resolvable-path answer that only a local process gets.
func TestDataWriteBrowserCannotRegisterOrProbe(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	ws := filepath.Join(srv.sessions.HomeDir(), "ws")
	page := writeHome(t, srv, "ws/note.htmlclay", dataWriteDoc)

	routes := 0
	srv.SetHooks(Hooks{
		TrustedCovers: func(absPath string) bool { return session.EqualOrUnder(absPath, ws) },
		Route: func(absPath string) (string, bool) {
			routes++
			if _, err := srv.sessions.Register(absPath, session.ViaTrusted); err != nil {
				return "", false
			}
			return fmt.Sprintf("http://127.0.0.1:%d/", srv.port), true
		},
	})
	if err := srv.sessions.InstallTrustedRoot(ws); err != nil {
		t.Fatal(err)
	}

	browser := func() map[string]string {
		return map[string]string{
			"Sec-Fetch-Site": "same-origin",
			"Origin":         fmt.Sprintf("http://127.0.0.1:%d", srv.port),
		}
	}

	present := postDataWrite(t, srv, "/_/api/ws/note.htmlclay", `{"title":"World"}`, browser())
	absent := postDataWrite(t, srv, "/_/api/ws/absent.htmlclay", `{"title":"World"}`, browser())

	for name, w := range map[string]*httptest.ResponseRecorder{"present": present, "absent": absent} {
		if w.Code != 403 {
			t.Fatalf("%s: status = %d, want 403: %s", name, w.Code, w.Body.String())
		}
		if got := decodeWriteError(t, w).Error; got != "Save-Token required" {
			t.Errorf("%s: error = %q, want %q", name, got, "Save-Token required")
		}
	}
	if present.Body.String() != absent.Body.String() {
		t.Errorf("the refusals differ:\npresent %s\nabsent  %s", present.Body.String(), absent.Body.String())
	}
	if routes != 0 {
		t.Errorf("Route was called %d time(s) for a browser caller, want 0", routes)
	}
	if _, registered := srv.sessions.LookupByPath(page); registered {
		t.Error("a browser write registered a trusted-folder file")
	}
	if got := readDoc(t, page); got != dataWriteDoc {
		t.Errorf("a refused write changed the file:\n%s", got)
	}
}

// The existence-oracle check runs again with the trusted-folder hooks wired, which is the only
// state in which the old path resolution could have answered a local process differently for a
// present and an absent file.
func TestDataWriteUnregisteredOutsideTrustedFoldersIsNotAnExistenceOracle(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	ws := filepath.Join(srv.sessions.HomeDir(), "ws")
	if err := os.MkdirAll(ws, 0755); err != nil {
		t.Fatal(err)
	}
	writeHome(t, srv, "loose.htmlclay", dataWriteDoc)

	srv.SetHooks(Hooks{
		TrustedCovers: func(absPath string) bool { return session.EqualOrUnder(absPath, ws) },
		Route: func(absPath string) (string, bool) {
			if _, err := srv.sessions.Register(absPath, session.ViaTrusted); err != nil {
				return "", false
			}
			return fmt.Sprintf("http://127.0.0.1:%d/", srv.port), true
		},
	})
	if err := srv.sessions.InstallTrustedRoot(ws); err != nil {
		t.Fatal(err)
	}

	present := postDataWrite(t, srv, "/_/api/loose.htmlclay", `{"title":"World"}`, nil)
	absent := postDataWrite(t, srv, "/_/api/absent.htmlclay", `{"title":"World"}`, nil)

	for name, w := range map[string]*httptest.ResponseRecorder{"present": present, "absent": absent} {
		if w.Code != 403 {
			t.Fatalf("%s: status = %d, want 403: %s", name, w.Code, w.Body.String())
		}
		if got := decodeWriteError(t, w).Error; got != "Not writable" {
			t.Errorf("%s: error = %q", name, got)
		}
	}
	if present.Body.String() != absent.Body.String() {
		t.Errorf("the refusals differ:\npresent %s\nabsent  %s", present.Body.String(), absent.Body.String())
	}
}

// A rule that names a DOM property the engine refuses to write is the caller's mistake, so the
// write face answers 400 with the engine's own words rather than the extraction face's 500.
func TestDataWriteReadOnlyRule(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	writeHome(t, srv, "test.htmlclay", `<!DOCTYPE html>
<html><head><script type="application/json" data-rules-name="api" data-rules-version="1">{t:"h1",tag:"h1@tagName"}</script></head>
<body><h1>Hello</h1></body></html>
`)

	w := postDataWrite(t, srv, "/_/api/test.htmlclay", `{"tag":"H2"}`, nil)
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	body := decodeWriteError(t, w)
	if body.Error != "Read-only rule" {
		t.Errorf("error = %q, want %q", body.Error, "Read-only rule")
	}
	if body.Message != `cannot write to read-only DOM property "tagName"` {
		t.Errorf("message = %q", body.Message)
	}
}

// Both GET faces answer the stamp of the stored bytes, which is what a client holds to write
// conditionally.
func TestDataReadFacesCarryTheETag(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	path := writeHome(t, srv, "test.htmlclay", dataWriteDoc)
	want := specwire.Etag([]byte(readDoc(t, path)))

	if got := getAPI(t, srv, "test.htmlclay").Header().Get("ETag"); got != want {
		t.Errorf("/_/api ETag = %q, want %q", got, want)
	}
	query := get(t, srv, `/test.htmlclay?data={title:"h1"}`, "test.htmlclay")
	if query.Code != 200 {
		t.Fatalf("?data= status = %d: %s", query.Code, query.Body.String())
	}
	if got := query.Header().Get("ETag"); got != want {
		t.Errorf("?data= ETag = %q, want %q", got, want)
	}
}

// A data write has no browser tab behind it to relay the snapshot to the other editors, so without
// an announcement an open edit-mode tab learns of it only on its next save, as a 412. It is
// announced exactly as an external edit is: the content rides the live lane's notification, the
// disk HTML follows on the saved lane, and the re-anchoring acceptServerReplacement performs before
// the publish keeps the incarnation's generation from rolling and throwing the tab's replay away.
func TestDataWriteReachesEditTabsOnTheLiveLane(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)
	path := writeHome(t, srv, "test.htmlclay", dataWriteDoc)

	live := newSubscriber(f.AbsPath, laneLive)
	saved := newSubscriber(f.AbsPath, laneSaved)
	srv.hub.add(live)
	srv.hub.add(saved)
	before := srv.hub.incs[f.AbsPath].generation

	if w := postDataWrite(t, srv, "/_/api/test.htmlclay", `{"title":"World"}`, nil); w.Code != 200 {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}

	f.Lock()
	key := f.HistoryKey()
	f.Unlock()
	stored := []byte(readDoc(t, path))
	want := forBrowser(stored, key)

	notice := waitFrame(t, live, time.Second)
	if notice["type"] != "notification" {
		t.Fatalf("live lane got %v, want a notification", notice)
	}
	data, ok := notice["data"].(map[string]interface{})
	if !ok {
		t.Fatalf("notification carries no data object: %v", notice)
	}
	if data["kind"] != "external-change" {
		t.Errorf("kind = %v, want external-change", data["kind"])
	}
	if data["html"] != want {
		t.Errorf("html = %q, want the browser-facing bytes %q", data["html"], want)
	}
	if data["etag"] != specwire.Etag(stored) {
		t.Errorf("etag = %v, want the stamp of the disk bytes %q", data["etag"], specwire.Etag(stored))
	}

	content := waitFrame(t, saved, time.Second)
	if content["html"] != want || content["sender"] != "file-system" {
		t.Errorf("saved lane payload = %v, want the stored bytes from the file-system", content)
	}

	if got := srv.hub.incs[f.AbsPath].generation; got != before {
		t.Errorf("an API write rolled the generation from %d to %d", before, got)
	}
}

// An API write's bytes are this host's, but they are not an open tab's. A tab still holding the
// stamp from before it must be refused, and told nothing about who moved the document, because
// `another-tab` here would be a confident wrong answer about an agent's write.
func TestDataWriteConflictIsNotAnotherTab(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)
	writeHome(t, srv, "test.htmlclay", dataWriteDoc)
	stamp := specwire.Etag([]byte(dataWriteDoc))

	if w := postDataWrite(t, srv, "/_/api/test.htmlclay", `{"title":"World"}`, nil); w.Code != 200 {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}

	w := saveThroughMux(t, srv, f, dataWriteDoc, stamp)
	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("a save holding the pre-write stamp = %d, want 412: %s", w.Code, w.Body.String())
	}
	if got, present := decode(t, w)["changedBy"]; present {
		t.Errorf("changedBy = %v, but the write came through the data API, not another tab", got)
	}
}

// The write is announced once. RecordServerWrite keeps the watcher's suppression in step, so its
// next look at the same bytes confirms a change already delivered rather than publishing a second
// external change for the tab to apply again.
func TestDataWriteWatcherStaysQuiet(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)
	writeHome(t, srv, "test.htmlclay", dataWriteDoc)

	live := newSubscriber(f.AbsPath, laneLive)
	srv.hub.add(live)

	if w := postDataWrite(t, srv, "/_/api/test.htmlclay", `{"title":"World"}`, nil); w.Code != 200 {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if notice := waitFrame(t, live, time.Second); notice["type"] != "notification" {
		t.Fatalf("live lane got %v, want the write's notification", notice)
	}

	// Hand-driven and hand-aged, as the watcher's own tests do: the first look arms the candidate,
	// the second is the one that would publish.
	wt := srv.watcher
	wt.quiet = 0
	e := &watchEntry{file: f, refs: 1}
	wt.check(e)
	wt.check(e)

	expectNoFrame(t, live, 100*time.Millisecond)
}
