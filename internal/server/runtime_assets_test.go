package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/panphora/htmlclay/internal/clayruntime"
	"github.com/panphora/htmlclay/internal/htmlutil"
	"github.com/panphora/htmlclay/internal/session"
	"github.com/panphora/htmlclay/internal/specwire"
)

func runtimeRequest(t *testing.T, srv *Server, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)
	return w
}

func runtimeAssetTarget(name string) string {
	return "/_/clayjs/" + clayruntime.Selected + "/" + name
}

func kanbanDocument() string {
	return `<!DOCTYPE html>
<html lang="en">
<head>
<script src="https://clayjs.com/v1/clay.js?plugins=sortable,people,upload,sync,visitor-drafts,unsent,menu&exclude=richclay"></script>
<script src="https://clayjs.com/v1/sap.js"></script>
<link rel="stylesheet" href="https://clayjs.com/v1/src/page/themes/light-1.css">
</head>
<body><p>board</p></body>
</html>`
}

// The route answers the embedded release itself, not a mock of it: every path the
// index lists is requested and its bytes are checked against the index digest, so a
// file the binary cannot produce fails here rather than in a browser offline.
func TestRuntimeAssetsServeEveryEmbeddedFile(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	files := clayruntime.Releases[clayruntime.Selected].Files
	if len(files) <= 400 {
		t.Fatalf("release lists %d files, want the full vendored release", len(files))
	}

	counted := 0
	for name, entry := range files {
		w := runtimeRequest(t, srv, "GET", runtimeAssetTarget(name), nil)
		if w.Code != 200 {
			t.Errorf("%s = %d", name, w.Code)
			continue
		}
		if ct := w.Header().Get("Content-Type"); ct != entry.Type {
			t.Errorf("%s Content-Type = %q, want %q", name, ct, entry.Type)
		}
		sum := sha256.Sum256(w.Body.Bytes())
		if got := hex.EncodeToString(sum[:]); got != entry.SHA256 {
			t.Errorf("%s bytes hash %s, want %s", name, got, entry.SHA256)
		}
		counted++
	}
	if counted != len(files) {
		t.Fatalf("served %d of %d files", counted, len(files))
	}
}

func TestRuntimeAssetsHeadAndConditionalRequest(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	name := "clay.js"
	entry := clayruntime.Releases[clayruntime.Selected].Files[name]

	// HEAD and 304 are net/http's own behavior on the response, so this one case runs
	// over the real listener rather than a recorder, which keeps every byte written.
	go srv.Start()
	t.Cleanup(func() { srv.Close() })
	base := "http://" + srv.Host()

	headResp, err := http.Head(base + runtimeAssetTarget(name))
	if err != nil {
		t.Fatal(err)
	}
	headBody, err := io.ReadAll(headResp.Body)
	headResp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if headResp.StatusCode != 200 {
		t.Fatalf("HEAD = %d", headResp.StatusCode)
	}
	if len(headBody) != 0 {
		t.Errorf("HEAD carried %d bytes", len(headBody))
	}
	head := headResp
	if ct := head.Header.Get("Content-Type"); ct != entry.Type {
		t.Errorf("HEAD Content-Type = %q, want %q", ct, entry.Type)
	}
	if cc := head.Header.Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Errorf("HEAD Cache-Control = %q", cc)
	}
	if etag := head.Header.Get("ETag"); etag != `"`+entry.SHA256+`"` {
		t.Errorf("HEAD ETag = %q, want the index digest", etag)
	}

	// The URL a served document actually asks for, query and all: the query is the
	// page's plugin selection, kept verbatim by the projection.
	withQuery, err := http.Get(base + runtimeAssetTarget(name) + "?plugins=sortable,people,upload,sync,visitor-drafts,unsent,menu&exclude=richclay")
	if err != nil {
		t.Fatal(err)
	}
	queryBody, err := io.ReadAll(withQuery.Body)
	withQuery.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if withQuery.StatusCode != 200 {
		t.Fatalf("GET with the plugin query = %d", withQuery.StatusCode)
	}
	querySum := sha256.Sum256(queryBody)
	if got := hex.EncodeToString(querySum[:]); got != entry.SHA256 {
		t.Errorf("GET with the plugin query served %s, want the %s digest", got, name)
	}

	conditionalReq, err := http.NewRequest("GET", base+runtimeAssetTarget(name), nil)
	if err != nil {
		t.Fatal(err)
	}
	conditionalReq.Header.Set("If-None-Match", `"`+entry.SHA256+`"`)
	conditionalResp, err := http.DefaultClient.Do(conditionalReq)
	if err != nil {
		t.Fatal(err)
	}
	conditionalBody, err := io.ReadAll(conditionalResp.Body)
	conditionalResp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if conditionalResp.StatusCode != 304 {
		t.Fatalf("If-None-Match = %d, want 304", conditionalResp.StatusCode)
	}
	if len(conditionalBody) != 0 {
		t.Errorf("304 carried %d bytes", len(conditionalBody))
	}
}

// A miss must never answer 200 and never carry a bundled file's bytes, whatever
// shape the request takes.
func TestRuntimeAssetsNeverServesAMiss(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	clay := clayruntime.Releases[clayruntime.Selected].Files["clay.js"]

	targets := []string{
		"/_/clayjs/0.0.1/clay.js",
		runtimeAssetTarget("not-a-real-file.js"),
		runtimeAssetTarget("../../test.htmlclay"),
		runtimeAssetTarget("%2e%2e/%2e%2e/test.htmlclay"),
		runtimeAssetTarget("..%2ftest.htmlclay"),
		runtimeAssetTarget("src%2fpage%2fthemes%2flight-1.css"),
		"/_/clayjs/" + clayruntime.Selected + "/",
	}
	for _, target := range targets {
		w := runtimeRequest(t, srv, "GET", target, nil)
		if w.Code == 200 {
			t.Errorf("%s = 200", target)
			continue
		}
		if strings.Contains(w.Body.String(), clay.SHA256) || len(w.Body.String()) == int(clay.Bytes) {
			t.Errorf("%s returned release bytes with status %d", target, w.Code)
		}
	}

	req := httptest.NewRequest("GET", runtimeAssetTarget("clay.js"), nil)
	req.Host = "evil.example.com"
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)
	if w.Code == 200 {
		t.Errorf("a wrong Host was answered with the runtime asset")
	}
	if w.Code != 403 {
		t.Logf("HostGate answered a wrong Host with %d", w.Code)
	}
}

func TestRuntimeProjectionRewritesServedDocument(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	body := kanbanDocument()
	page := writeHome(t, srv, "kanban.htmlclay", body)
	if _, err := srv.sessions.Register(page, session.ViaOsOpen); err != nil {
		t.Fatal(err)
	}

	w := serve(t, srv, "/kanban.htmlclay", nil)
	if w.Code != 200 {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	served := w.Body.String()
	selected := clayruntime.Selected

	for _, want := range []string{
		"/_/clayjs/" + selected + "/clay.js?plugins=sortable,people,upload,sync,visitor-drafts,unsent,menu&exclude=richclay",
		"/_/clayjs/" + selected + "/sap.js",
		"/_/clayjs/" + selected + "/src/page/themes/light-1.css",
		`id="clay-runtime-map"`,
	} {
		if !strings.Contains(served, want) {
			t.Errorf("served document is missing %q:\n%s", want, served)
		}
	}
	for _, public := range []string{`src="https://clayjs.com/v1/`, `href="https://clayjs.com/v1/`} {
		if strings.Contains(served, public) {
			t.Errorf("served document still loads the public runtime (%s):\n%s", public, served)
		}
	}

	disk, err := os.ReadFile(page)
	if err != nil {
		t.Fatal(err)
	}
	if string(disk) != body {
		t.Errorf("the file on disk changed:\n%s", disk)
	}
	if got, want := decodedDocumentETag(t, w.Body.Bytes()), specwire.Etag(disk); got != want {
		t.Errorf("documentetag = %q, want %q (specwire.Etag of the disk bytes)", got, want)
	}
	if vary := w.Header().Get("Vary"); !strings.Contains(vary, "Clay-Source") {
		t.Errorf("Vary = %q, want it to name Clay-Source", vary)
	}
}

func TestRuntimeProjectionLeavesSourceReadsPublic(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	page := writeHome(t, srv, "kanban.htmlclay", kanbanDocument())
	if _, err := srv.sessions.Register(page, session.ViaOsOpen); err != nil {
		t.Fatal(err)
	}

	w := serve(t, srv, "/kanban.htmlclay", map[string]string{"Clay-Source": "1"})
	if w.Code != 200 {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	served := w.Body.String()
	if !strings.Contains(served, `src="https://clayjs.com/v1/clay.js?plugins=`) ||
		!strings.Contains(served, `src="https://clayjs.com/v1/sap.js"`) ||
		!strings.Contains(served, `href="https://clayjs.com/v1/src/page/themes/light-1.css"`) {
		t.Errorf("a source read lost its public runtime URLs:\n%s", served)
	}
	if strings.Contains(served, "/_/clayjs/") || strings.Contains(served, "clay-runtime-map") {
		t.Errorf("a source read was projected:\n%s", served)
	}

	disk, err := os.ReadFile(page)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := decodedDocumentETag(t, w.Body.Bytes()), specwire.Etag(disk); got != want {
		t.Errorf("documentetag = %q, want %q", got, want)
	}
}

func TestRuntimeProjectionLeavesOtherDocumentsAlone(t *testing.T) {
	t.Run("no runtime URL", func(t *testing.T) {
		srv, f, content := setupHandlerTest(t)
		w := serve(t, srv, "/test.htmlclay", nil)
		if w.Code != 200 {
			t.Fatalf("status = %d", w.Code)
		}

		disk := []byte(content)
		id := htmlutil.ReadHTMLClayID(w.Body.Bytes())
		if id == "" {
			t.Fatal("the served document carries no id to rebuild the chain with")
		}
		want := htmlutil.InjectToken(
			htmlutil.InjectDocumentETag(htmlutil.SetHTMLClayID(disk, id), specwire.Etag(disk)),
			f.Token,
		)
		if got := w.Body.String(); got != string(want) {
			t.Errorf("served bytes changed:\ngot  %q\nwant %q", got, want)
		}
	})

	t.Run("not utf8", func(t *testing.T) {
		srv, _, _ := setupHandlerTest(t)
		raw := "<html><head><script src=\"https://clayjs.com/v1/clay.js\"></script></head><body>\xff\xfe</body></html>"
		page := writeHome(t, srv, "binary.htmlclay", raw)
		if _, err := srv.sessions.Register(page, session.ViaOsOpen); err != nil {
			t.Fatal(err)
		}

		w := serve(t, srv, "/binary.htmlclay", nil)
		if w.Code != 200 {
			t.Fatalf("status = %d", w.Code)
		}
		if !strings.Contains(w.Body.String(), `src="https://clayjs.com/v1/clay.js"`) {
			t.Errorf("a non-UTF-8 document was projectable:\n%q", w.Body.String())
		}
		if strings.Contains(w.Body.String(), "/_/clayjs/") {
			t.Errorf("a non-UTF-8 document was projected:\n%q", w.Body.String())
		}
	})
}

// The read-only banner path buffers its own bytes, so it is projected in its own
// right rather than inheriting serveRegistered's work.
func TestRuntimeProjectionReachesTheReadOnlyBanner(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	dir := registerSubdirPage(t, srv, "site")
	page := filepath.Join(dir, "board.htmlclay")
	if err := os.WriteFile(page, []byte(kanbanDocument()), 0644); err != nil {
		t.Fatal(err)
	}
	srv.SetHooks(Hooks{
		TrustRequest: func(string, bool) (string, bool) { return "", false },
	})

	w := runtimeRequest(t, srv, "GET", "/site/board.htmlclay", map[string]string{
		"Sec-Fetch-Dest": "document",
		"Sec-Fetch-User": "?1",
	})
	if w.Code != 200 {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	served := w.Body.String()
	if !strings.Contains(served, "htmlclay-banner") {
		t.Fatalf("the banner path did not run:\n%s", served)
	}
	if !strings.Contains(served, "/_/clayjs/"+clayruntime.Selected+"/clay.js?plugins=sortable,people,upload,sync,visitor-drafts,unsent,menu&exclude=richclay") {
		t.Errorf("the bannered document was not projected:\n%s", served)
	}
	for _, public := range []string{`src="https://clayjs.com/v1/`, `href="https://clayjs.com/v1/`} {
		if strings.Contains(served, public) {
			t.Errorf("the bannered document still loads the public runtime (%s):\n%s", public, served)
		}
	}
}
