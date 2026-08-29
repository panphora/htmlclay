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
	"time"

	"github.com/panphora/htmlclay/internal/htmlutil"
	"github.com/panphora/htmlclay/internal/testutil"
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

// The narrow form (reading @savetoken) is not enough on its own: @outerHTML hands back the whole
// document, so if either server-side injection were present in the parsed tree it would ride out
// inside a string the narrow test never looks at.
func TestDataQueryCannotReachTheInjectionsViaOuterHTML(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	// Serve once as a document so the id and token exist, then save so an id lands on disk.
	get(t, srv, "/test.htmlclay", "test.htmlclay")
	saved := `<!DOCTYPE html><html documentid="persisted-id" savetoken="` + f.Token +
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
	if strings.Contains(body, f.Token) || strings.Contains(body, "savetoken") {
		t.Errorf("the whole-document read carried a save token: %s", body)
	}

	// The plain GET of the same file still carries both, so this is a data-face property rather
	// than the injections having stopped happening.
	plain := get(t, srv, "/test.htmlclay", "test.htmlclay")
	if !strings.Contains(plain.Body.String(), "savetoken") {
		t.Error("the plain GET lost its token; this test would pass for the wrong reason")
	}
}

func TestDataQueryHtmlclayidIsNullBeforeFirstSave(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)

	w := get(t, srv, `/test.htmlclay?data={id:"html@documentid"}`, "test.htmlclay")
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

// A save holds f.Lock() across read, backup, atomic replace and state update, and
// a data request takes the same mutex before reading, so no data response can be
// assembled from inside a save.
//
// serveRegistered's own comment records that the rename, not this mutex, is what
// makes a response a whole pre-save or post-save document, and that the lock is
// kept as the guarantee that would survive a write path that stopped being a
// rename. TestAtomicWriteFilePublishesOnlyByRename pins the rename. This pins the
// lock, by holding a save at its rename and showing a read cannot get past it.
func TestDataReadCannotLandInsideASave(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	page := func(marker string) string {
		return `<!DOCTYPE html><html savetoken="` + f.Token +
			`"><head><title>` + marker + `</title></head><body><p>` + marker + `</p></body></html>`
	}
	if err := os.WriteFile(f.AbsPath, []byte(page("AAA")), 0644); err != nil {
		t.Fatal(err)
	}

	staged := make(chan struct{})
	release := make(chan struct{})
	// Once, because a second concurrent entry would close a closed channel and take
	// the whole package down rather than fail this test.
	var stagedOnce sync.Once
	beforeAtomicReplace = func() {
		stagedOnce.Do(func() { close(staged) })
		<-release
	}
	t.Cleanup(func() { beforeAtomicReplace = nil })

	saved := make(chan int, 1)
	go func() {
		req := httptest.NewRequest("POST", "/_/save/"+f.Token, strings.NewReader(page("BBB")))
		req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
		req.SetPathValue("token", f.Token)
		w := httptest.NewRecorder()
		srv.handleSave(w, req)
		saved <- w.Code
	}()

	testutil.Receive(t, 10*time.Second, "the save to reach its rename", staged)
	if f.TryLock() {
		f.Unlock()
		t.Fatal("a save must hold the file lock across its rename, or a reader can land between the rename and the state update")
	}

	read := make(chan string, 1)
	dispatched := make(chan struct{})
	go func() {
		close(dispatched)
		w := get(t, srv, `/test.htmlclay?data={t:"title",p:"p"}`, "test.htmlclay")
		read <- w.Body.String()
	}()
	// Wait for the reader to be running before opening the window below. Without
	// this the window can expire while the goroutine has not been scheduled at all,
	// and a build whose read takes no lock would look indistinguishable from one
	// that blocked. The residual is the few instructions between this signal and
	// the lock acquisition inside the handler.
	testutil.Receive(t, 10*time.Second, "the reader goroutine to start", dispatched)

	// Expiry is success here, and a longer wait can only make the assertion harder
	// to satisfy: a read must not be able to complete while a save holds the lock.
	select {
	case got := <-read:
		t.Fatalf("a data read completed inside a save's critical section, returning %s", got)
	case <-time.After(250 * time.Millisecond):
	}

	close(release)
	if code := testutil.Receive(t, 10*time.Second, "the save to finish", saved); code != 200 {
		t.Fatalf("save: %d", code)
	}

	body := testutil.Receive(t, 10*time.Second, "the queued read to finish", read)
	var got struct{ T, P string }
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("read after the save returned %s: %v", body, err)
	}
	// One marker on both the title and the paragraph, so a response pairing one
	// document's title with another's shows up as a mismatch here.
	if got.T != got.P {
		t.Fatalf("the read saw a mixture: title %q, paragraph %q", got.T, got.P)
	}
	if got.T != "BBB" {
		t.Fatalf("the read was queued behind the save, so it must see the saved document, got %q", got.T)
	}
}

// The same paths under the race detector, with many readers against a continuous
// writer. What it proves is no data race and no failed request across the real
// save and read stacks; the tear property is pinned deterministically by
// TestDataReadCannotLandInsideASave above.
//
// It does NOT prove any read met an active save: a legal schedule runs the saves
// and the reads without overlapping, and the counts below cannot tell that apart.
// They are here so the test cannot pass having read nothing, which is a weaker
// claim than a staged interleaving and is deliberately all this one makes.
func TestDataReadsDuringConcurrentSavesStress(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	page := func(marker string) string {
		return `<!DOCTYPE html><html savetoken="` + f.Token +
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
	var torn, failed, saveFailures, reads, saves int
	note := func(p *int) { mu.Lock(); *p++; mu.Unlock() }

	start := make(chan struct{})
	stop := make(chan struct{})
	// Closed after the writer's first successful save. Making a goroutine runnable
	// is not the same as running it: without this, a schedule that runs all the
	// readers first and then closes stop leaves the writer taking the ready stop
	// case having saved nothing, and the count below fails for a scheduling reason
	// rather than a real one.
	saved := make(chan struct{})
	var savedOnce sync.Once
	var writer sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		<-start
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
			note(&saves)
			savedOnce.Do(func() { close(saved) })
		}
	}()

	const readerCount, each = 8, 40
	var readersWG sync.WaitGroup
	for i := 0; i < readerCount; i++ {
		readersWG.Add(1)
		go func() {
			defer readersWG.Done()
			<-start
			for j := 0; j < each; j++ {
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
				note(&reads)
				if got.T != got.P {
					note(&torn)
				}
			}
		}()
	}

	// A start barrier, so the readers and the writer run against each other rather
	// than trickling out behind however long the spawn loop took.
	close(start)
	testutil.Receive(t, 30*time.Second, "the writer's first save", saved)
	readersWG.Wait()
	close(stop)
	writer.Wait()

	if torn != 0 || failed != 0 || saveFailures != 0 {
		t.Errorf("torn=%d failed=%d saveFailures=%d; every data read must see a whole document",
			torn, failed, saveFailures)
	}
	// Without these the test passes just as happily having read nothing and saved
	// nothing, which is what an all-green stress test with no counts is worth.
	if reads != readerCount*each {
		t.Errorf("%d reads completed, want %d", reads, readerCount*each)
	}
	if saves == 0 {
		t.Error("the writer never completed a save, so nothing overlapped a read")
	}
}
