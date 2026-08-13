package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/panphora/htmlclay/htmlutil"
)

// datasecurity_test.go covers what the data faces must NOT do. Every case here is a property of the
// serve path the faces attach to rather than of the extractor, which is the point: the mode flag is
// only worth anything if the guards above it keep applying.

// serve drives the full stack — HostValidationMiddleware, the mux, then the handler — because the
// guards under test live in the middleware and would be invisible to a direct handler call.
func serve(t *testing.T, srv *Server, target string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", target, nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)
	return w
}

// Both guards wrap the mux, so they cover the data routes for free. "For free" is exactly the kind
// of claim that stops being true after a refactor, so it is asserted rather than reasoned about.
func TestDataFacesInheritTheHostAndCrossSiteGuards(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	writeHome(t, srv, "test.htmlclay",
		`<html><head><title>T</title></head><body>`+
			`<script data-rules-name="api" data-rules-version="1">{t:"title"}</script></body></html>`)

	targets := []string{`/test.htmlclay?data={t:"title"}`, "/_/api/test.htmlclay"}

	for _, target := range targets {
		t.Run("cross-site "+target, func(t *testing.T) {
			w := serve(t, srv, target, map[string]string{"Sec-Fetch-Site": "cross-site"})
			if w.Code != 403 {
				t.Fatalf("status = %d, want 403", w.Code)
			}
			if strings.Contains(w.Body.String(), `"t"`) {
				t.Fatalf("extraction ran for a cross-site request: %s", w.Body.String())
			}
		})

		t.Run("bad host "+target, func(t *testing.T) {
			req := httptest.NewRequest("GET", target, nil)
			req.Host = "evil.example.com"
			w := httptest.NewRecorder()
			srv.httpServer.Handler.ServeHTTP(w, req)

			if w.Code != 403 {
				t.Fatalf("status = %d, want 403", w.Code)
			}
			if strings.Contains(w.Body.String(), `"t"`) {
				t.Fatalf("extraction ran for a bad Host: %s", w.Body.String())
			}
		})
	}
}

// ServeMux hands PathValue over ALREADY DECODED, so ValidatePath is the only thing standing between
// an encoded traversal and the file it names. That it runs on the new route is worth proving.
func TestDataAPIRejectsEncodedTraversal(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	home := srv.sessions.HomeDir()
	outside := filepath.Join(filepath.Dir(home), "outside-secret.html")
	// A marker that cannot appear in a URL, so a redirect naming the path is not mistaken for the
	// file's contents. The first version of this test used the word "secret" and flagged ServeMux's
	// own 307 body as a leak.
	os.WriteFile(outside, []byte("<h1>TOPSECRETCONTENT</h1>"), 0644)
	t.Cleanup(func() { os.Remove(outside) })

	for _, target := range []string{
		"/_/api/..%2Foutside-secret.html",
		"/_/api/%2e%2e%2foutside-secret.html",
		"/_/api/..%5Coutside-secret.html",
		"/_/api/../outside-secret.html",
		"/_/api/sub/..%2f..%2foutside-secret.html",
	} {
		t.Run(target, func(t *testing.T) {
			w := serve(t, srv, target, nil)
			if strings.Contains(w.Body.String(), "TOPSECRETCONTENT") {
				t.Fatalf("traversal leaked content: %s", w.Body.String())
			}
			if w.Code == 200 {
				t.Fatalf("traversal succeeded: %s", w.Body.String())
			}
			// An unencoded ../ never reaches the handler: ServeMux normalises the path and
			// 307-redirects first. Follow it and prove the destination dead-ends too, rather than
			// treating the redirect itself as the answer.
			if loc := w.Header().Get("Location"); loc != "" {
				followed := serve(t, srv, loc, nil)
				if followed.Code == 200 || strings.Contains(followed.Body.String(), "TOPSECRETCONTENT") {
					t.Fatalf("the redirect target served the file: %d %s", followed.Code, followed.Body.String())
				}
			}
		})
	}
}

// The narrow form (reading @htmlclaytoken) is not enough on its own: @outerHTML hands back the whole
// document, so if either server-side injection were present in the parsed tree it would ride out
// inside a string the narrow test never looks at.
func TestDataQueryCannotReachTheInjectionsViaOuterHTML(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	// Serve once as a document so the id and token exist, then save so an id lands on disk.
	get(t, srv, "/test.htmlclay", "test.htmlclay")
	saved := `<!DOCTYPE html><html htmlclayid="persisted-id" htmlclaytoken="` + f.Token +
		`"><head><title>T</title></head><body><p>x</p></body></html>`
	req := httptest.NewRequest("POST", "/_/save/"+f.Token, strings.NewReader(saved))
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.SetPathValue("token", f.Token)
	sw := httptest.NewRecorder()
	srv.handleSave(sw, req)
	if sw.Code != 200 {
		t.Fatalf("save failed: %d %s", sw.Code, sw.Body.String())
	}

	w := get(t, srv, `/test.htmlclay?data={d:"html@outerHTML"}`, "test.htmlclay")
	if w.Code != 200 {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, f.Token) || strings.Contains(body, "htmlclaytoken") {
		t.Errorf("the whole-document read carried a save token: %s", body)
	}

	// The plain GET of the same file still carries both, so this is a data-face property rather
	// than the injections having stopped happening.
	plain := get(t, srv, "/test.htmlclay", "test.htmlclay")
	if !strings.Contains(plain.Body.String(), "htmlclaytoken") {
		t.Error("the plain GET lost its token; this test would pass for the wrong reason")
	}
}

func TestDataQueryHtmlclayidIsNullBeforeFirstSave(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)

	w := get(t, srv, `/test.htmlclay?data={id:"html@htmlclayid"}`, "test.htmlclay")
	if w.Code != 200 {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != `{"id":null}` {
		t.Errorf("body = %s, want null: the id rides in the served bytes, never on disk", got)
	}
}

// sidecarEncoding serves page.html.gz as gzip bytes. Handing those to an HTML parser would produce a
// confident null rather than an error, which is the worst possible answer. isExtractable closes it,
// but only via extractFilePath declining to truncate at ".html." — subtle enough to pin.
func TestDataIsIgnoredOnCompressedSidecars(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	registerSubdirPage(t, srv, "sub")
	gz := "\x1f\x8b\x08\x00fake-gzip-bytes"
	writeHome(t, srv, "sub/page.html.gz", gz)

	w := get(t, srv, `/sub/page.html.gz?data={t:"h1"}`, "sub/page.html.gz")
	if w.Code != 200 {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != gz {
		t.Errorf("the sidecar was not served byte-identically: %q", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); strings.Contains(ct, "json") {
		t.Errorf("Content-Type = %q; a .gz must never be parsed as HTML", ct)
	}

	// And the /_/api face refuses it outright rather than parsing gzip.
	if a := getAPI(t, srv, "sub/page.html.gz"); a.Code != 404 {
		t.Errorf("/_/api on a .gz = %d, want 404", a.Code)
	}
}

func TestDataRequestCapturesNoVersion(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	for i := 0; i < 3; i++ {
		if w := get(t, srv, `/test.htmlclay?data={t:"title"}`, "test.htmlclay"); w.Code != 200 {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}
	}
	if key := f.HistoryKey(); key != "" {
		t.Fatalf("a data request resolved a history key %q", key)
	}

	// A plain GET then does capture one, so the store is genuinely reachable in this harness and
	// the assertion above is not passing because nothing ever writes history.
	get(t, srv, "/test.htmlclay", "test.htmlclay")
	key := f.HistoryKey()
	if key == "" {
		t.Fatal("the plain GET captured no history; this test would pass for the wrong reason")
	}
	if versionList, err := srv.versions.List(key, f.AbsPath); err != nil {
		t.Fatalf("list versions: %v", err)
	} else if len(versionList) == 0 {
		t.Error("the plain GET captured no first-open snapshot")
	}
}

// A request with no data parameter must be untouched by any of this: a query string the data face
// does not claim changes nothing at all.
func TestNoDataParameterServesTheDocumentUnchanged(t *testing.T) {
	srv, _, content := setupHandlerTest(t)

	var first string
	for _, target := range []string{"/test.htmlclay", "/test.htmlclay?", "/test.htmlclay?other=1", "/test.htmlclay?other=%zz"} {
		w := get(t, srv, target, "test.htmlclay")
		if w.Code != 200 {
			t.Fatalf("%s = %d: %s", target, w.Code, w.Body.String())
		}
		if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
			t.Errorf("%s Content-Type = %q", target, ct)
		}

		// The serve path adds exactly two things to the disk bytes: the durable id and the save
		// token. Remove both and what is left must be the file.
		stripped := string(htmlutil.StripHTMLClayID(htmlutil.StripToken(w.Body.Bytes())))
		if stripped != content {
			t.Errorf("%s body diverged from disk:\n got: %q\nwant: %q", target, stripped, content)
		}

		// Every one of these is a plain serve, so they must agree with each other too.
		if first == "" {
			first = w.Body.String()
		} else if w.Body.String() != first {
			t.Errorf("%s differed from the bare request", target)
		}
	}
}

// The two data routes sit ahead of the catch-all, so every existing /_/ route must still win.
func TestDataRoutesDoNotShadowExistingUnderscoreRoutes(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	cases := []struct{ target string }{
		{"/_/read/" + f.Token},
		{"/_/meta/" + f.Token},
		{"/_/versions/" + f.Token},
	}
	for _, c := range cases {
		t.Run(c.target, func(t *testing.T) {
			w := serve(t, srv, c.target, nil)
			if w.Code != 200 {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "Missing path") {
				t.Fatal("the request reached the data API handler")
			}
		})
	}

	// ...and a file whose name merely starts with the prefix still serves normally.
	registerSubdirPage(t, srv, "_apix")
	if w := serve(t, srv, "/_apix/page.htmlclay", nil); w.Code != 200 {
		t.Errorf("/_apix/page.htmlclay = %d, want the file", w.Code)
	}
}

func TestDataFacesAcquireNoCORSOnOPTIONS(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)

	req := httptest.NewRequest("OPTIONS", `/test.htmlclay?data={t:"title"}`, nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Access-Control-Request-Method", "GET")
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	for _, h := range []string{
		"Access-Control-Allow-Origin",
		"Access-Control-Allow-Methods",
		"Access-Control-Allow-Credentials",
	} {
		if v := w.Header().Get(h); v != "" {
			t.Errorf("OPTIONS acquired %s = %q", h, v)
		}
	}
}

// An out-of-scope data request must produce the SAME prompt-then-403 a plain GET produces, body and
// error header included. A data face that answered its own JSON 403 here would be distinguishable
// from a navigation, and the fixed body exists precisely so a page learns nothing from a denial.
func TestDataOutOfScopeIsTheInheritedDenial(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)

	plain := get(t, srv, "/outside/page.html", "outside/page.html")
	data := get(t, srv, `/outside/page.html?data={t:"h1"}`, "outside/page.html")

	if data.Code != plain.Code {
		t.Fatalf("data face = %d, plain GET = %d", data.Code, plain.Code)
	}
	if data.Body.String() != plain.Body.String() {
		t.Errorf("data body %q, plain body %q", data.Body.String(), plain.Body.String())
	}
	for _, h := range []string{"Content-Type", "X-HTMLClay-Error"} {
		if data.Header().Get(h) != plain.Header().Get(h) {
			t.Errorf("%s: data %q, plain %q", h, data.Header().Get(h), plain.Header().Get(h))
		}
	}
}

// Saves hold f.Lock() across read, backup, atomic replace and state update, and a data request takes
// the same mutex before reading. So every response must be a WHOLE pre-save or post-save document,
// never a mixture, and no save may fail because reads are in flight.
//
// The tear detector is that each document's title and paragraph carry the SAME marker, so any
// response pairing one document's title with another's is a mixture. The seed write matters: the
// harness fixture has a title and paragraph that differ, which the detector would read as 320 torn
// responses before a single save had landed.
func TestDataReadsDuringConcurrentSaves(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	page := func(marker string) string {
		return `<!DOCTYPE html><html htmlclaytoken="` + f.Token +
			`"><head><title>` + marker + `</title></head><body><p>` + marker + `</p></body></html>`
	}
	if err := os.WriteFile(f.AbsPath, []byte(page("AAA")), 0644); err != nil {
		t.Fatal(err)
	}

	save := func(marker string) int {
		req := httptest.NewRequest("POST", "/_/save/"+f.Token, strings.NewReader(page(marker)))
		req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
		req.SetPathValue("token", f.Token)
		w := httptest.NewRecorder()
		srv.handleSave(w, req)
		return w.Code
	}

	var mu sync.Mutex
	var torn, failed, saveFailures int
	note := func(p *int) { mu.Lock(); *p++; mu.Unlock() }

	stop := make(chan struct{})
	var writer sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		markers := []string{"AAA", "BBB"}
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if save(markers[i%2]) != 200 {
				note(&saveFailures)
				return
			}
		}
	}()

	var readers sync.WaitGroup
	for i := 0; i < 8; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for j := 0; j < 40; j++ {
				w := get(t, srv, `/test.htmlclay?data={t:"title",p:"p"}`, "test.htmlclay")
				if w.Code != 200 {
					note(&failed)
					continue
				}
				var got struct{ T, P string }
				if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
					note(&failed)
					continue
				}
				if got.T != got.P {
					note(&torn)
				}
			}
		}()
	}

	readers.Wait()
	close(stop)
	writer.Wait()

	if torn != 0 || failed != 0 || saveFailures != 0 {
		t.Errorf("torn=%d failed=%d saveFailures=%d; every data read must see a whole document",
			torn, failed, saveFailures)
	}
}
