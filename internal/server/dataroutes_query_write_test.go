package server

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/panphora/htmlclay/internal/session"
	"github.com/panphora/htmlclay/internal/specwire"
)

// dataroutes_query_write_test.go covers ?data= on the write faces: the plain page address and
// /_/api/<path> both take caller rules, and every gate the /_/api write already had still applies.

// queryWriteDoc is the tagless fixture. It has no rules tag at all, so only the caller's own rules
// can answer for it, and the paragraph is the content no write may touch.
const queryWriteDoc = `<!DOCTYPE html>
<html lang="en">
<head><title>Test</title></head>
<body>
<h1>Hello</h1>
<p>Untouched</p>
</body>
</html>
`

// registerFixture writes body under fixtures/ and registers it, which is what a write needs before it
// may touch a file.
func registerFixture(t *testing.T, srv *Server, name, body string) (*session.File, string) {
	t.Helper()
	path := writeHome(t, srv, filepath.Join("fixtures", name), body)
	f, err := srv.sessions.Register(path, session.ViaOsOpen)
	if err != nil {
		t.Fatalf("register %s: %v", path, err)
	}
	return f, path
}

func assertUnchanged(t *testing.T, path, want string) {
	t.Helper()
	if got := readDoc(t, path); got != want {
		t.Errorf("the file changed:\n%s", got)
	}
}

// The whole feature: a tagless document, rules from the caller, on the plain address and on the API
// address, for both extensions. A write through either address is visible through the other.
func TestDataQueryWriteRoundTripOnBothAliasesAndExtensions(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	const q = `?data={title:"h1"}`

	cases := []struct {
		name, file, read, write, other string
	}{
		{"plain html", "plain.html", "/fixtures/plain.html", "/fixtures/plain.html", "/_/api/fixtures/plain.html"},
		{"api html", "api.html", "/_/api/fixtures/api.html", "/_/api/fixtures/api.html", "/fixtures/api.html"},
		{"plain htmlclay", "plain.htmlclay", "/fixtures/plain.htmlclay", "/fixtures/plain.htmlclay", "/_/api/fixtures/plain.htmlclay"},
		{"api htmlclay", "api.htmlclay", "/_/api/fixtures/api.htmlclay", "/_/api/fixtures/api.htmlclay", "/fixtures/api.htmlclay"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, path := registerFixture(t, srv, c.file, queryWriteDoc)

			read := serve(t, srv, c.read+q, nil)
			if read.Code != http.StatusOK {
				t.Fatalf("GET %s = %d: %s", c.read, read.Code, read.Body.String())
			}
			if got := read.Body.String(); got != `{"title":"Hello"}` {
				t.Errorf("GET body = %s", got)
			}
			if want := specwire.Etag([]byte(readDoc(t, path))); read.Header().Get("ETag") != want {
				t.Errorf("GET ETag = %q, want the stamp of the stored bytes %q", read.Header().Get("ETag"), want)
			}

			write := postDataWrite(t, srv, c.write+q, `{"title":"World"}`, nil)
			if write.Code != http.StatusOK {
				t.Fatalf("POST %s = %d: %s", c.write, write.Code, write.Body.String())
			}
			if got := write.Body.String(); got != `{"title":"World"}` {
				t.Errorf("POST body = %s", got)
			}

			onDisk := readDoc(t, path)
			if !strings.Contains(onDisk, "<h1>World</h1>") {
				t.Errorf("the write did not land:\n%s", onDisk)
			}
			if !strings.Contains(onDisk, "<p>Untouched</p>") {
				t.Errorf("the write touched content outside its rules:\n%s", onDisk)
			}

			other := serve(t, srv, c.other+q, nil)
			if other.Code != http.StatusOK || other.Body.String() != `{"title":"World"}` {
				t.Errorf("GET %s = %d %s, want the written value", c.other, other.Code, other.Body.String())
			}
		})
	}
}

// Explicit caller rules win over the document's own api rules tag, on the read and on the write. A
// tag the engine cannot parse is not consulted at all when the caller supplied rules.
func TestDataAPIQueryRulesOverrideTheTag(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	tagged := `<!DOCTYPE html>
<html><head><script type="application/json" data-rules-name="api" data-rules-version="1">{title:"p"}</script></head>
<body><h1>Head</h1><p>Para</p></body></html>
`
	malformed := `<!DOCTYPE html>
<html><head><script type="application/json" data-rules-name="api" data-rules-version="1">{a:</script></head>
<body><h1>Head</h1></body></html>
`
	_, taggedPath := registerFixture(t, srv, "tagged.htmlclay", tagged)
	_, malformedPath := registerFixture(t, srv, "malformed-tag.htmlclay", malformed)

	// Without a data parameter the tag answers, exactly as it did before this route existed.
	tagRead := serve(t, srv, "/_/api/fixtures/tagged.htmlclay", nil)
	if tagRead.Code != http.StatusOK || tagRead.Body.String() != `{"title":"Para"}` {
		t.Fatalf("the tag face changed: %d %s", tagRead.Code, tagRead.Body.String())
	}
	tagWrite := postDataWrite(t, srv, "/_/api/fixtures/tagged.htmlclay", `{"title":"ViaTag"}`, nil)
	if tagWrite.Code != http.StatusOK || tagWrite.Body.String() != `{"title":"ViaTag"}` {
		t.Fatalf("the tag write changed: %d %s", tagWrite.Code, tagWrite.Body.String())
	}

	override := serve(t, srv, `/fixtures/tagged.htmlclay?data={title:"h1"}`, nil)
	if override.Code != http.StatusOK || override.Body.String() != `{"title":"Head"}` {
		t.Errorf("?data= on the plain address = %d %s, want the caller's rules", override.Code, override.Body.String())
	}
	apiOverride := serve(t, srv, `/_/api/fixtures/tagged.htmlclay?data={title:"h1"}`, nil)
	if apiOverride.Code != http.StatusOK || apiOverride.Body.String() != `{"title":"Head"}` {
		t.Errorf("?data= on the api address = %d %s, want the caller's rules", apiOverride.Code, apiOverride.Body.String())
	}

	beforeOverride := readDoc(t, taggedPath)
	if w := postDataWrite(t, srv, `/_/api/fixtures/tagged.htmlclay?data={title:"h1"}`, `{"title":"World"}`, nil); w.Code != http.StatusOK {
		t.Fatalf("query write = %d: %s", w.Code, w.Body.String())
	}
	after := readDoc(t, taggedPath)
	if want := strings.Replace(beforeOverride, "<h1>Head</h1>", "<h1>World</h1>", 1); after != want {
		t.Errorf("the override write changed more than the h1 it named:\n%s", after)
	}

	// A malformed tag is a 400 on its own, and irrelevant once the caller sent rules.
	bad := serve(t, srv, "/_/api/fixtures/malformed-tag.htmlclay", nil)
	if bad.Code != http.StatusBadRequest || decodeError(t, bad).Error != "Malformed api rules tag" {
		t.Fatalf("the malformed tag no longer 400s: %d %s", bad.Code, bad.Body.String())
	}
	fixed := serve(t, srv, `/_/api/fixtures/malformed-tag.htmlclay?data={t:"h1"}`, nil)
	if fixed.Code != http.StatusOK || fixed.Body.String() != `{"t":"Head"}` {
		t.Errorf("caller rules did not replace the malformed tag: %d %s", fixed.Code, fixed.Body.String())
	}
	if w := postDataWrite(t, srv, `/_/api/fixtures/malformed-tag.htmlclay?data={t:"h1"}`, `{"t":"Yo"}`, nil); w.Code != http.StatusOK {
		t.Fatalf("write with a malformed tag in the document = %d: %s", w.Code, w.Body.String())
	}
	if got := readDoc(t, malformedPath); !strings.Contains(got, "<h1>Yo</h1>") {
		t.Errorf("the write with caller rules did not land:\n%s", got)
	}
}

// A query the caller got wrong is refused before any file is resolved, on both faces and both
// methods, and nothing is written.
func TestDataQueryWriteRefusesBadRulesOnBothAliases(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	_, path := registerFixture(t, srv, "bad-rules.htmlclay", queryWriteDoc)

	cases := []struct {
		name, query, wantError, wantMessage string
	}{
		{"empty parameter", "?data=", "Missing data parameter", "Please provide extraction rules via ?data= parameter"},
		{"duplicate parameter", `?data={t:"h1"}&data={t:"p"}`, "Invalid extraction rules", "Provide exactly one data parameter."},
		{"undecodable parameter", "?data=%zz", "Invalid extraction rules", ""},
		{"malformed rules", `?data={t:`, "Invalid extraction rules", ""},
	}
	forms := []struct{ name, address string }{
		{"plain", "/fixtures/bad-rules.htmlclay"},
		{"api", "/_/api/fixtures/bad-rules.htmlclay"},
	}

	for _, c := range cases {
		for _, form := range forms {
			t.Run(c.name+" "+form.name, func(t *testing.T) {
				read := serve(t, srv, form.address+c.query, nil)
				if read.Code != http.StatusBadRequest {
					t.Fatalf("GET = %d, want 400: %s", read.Code, read.Body.String())
				}
				body := decodeError(t, read)
				if body.Error != c.wantError {
					t.Errorf("GET error = %q, want %q", body.Error, c.wantError)
				}
				if c.wantMessage != "" && body.Message != c.wantMessage {
					t.Errorf("GET message = %q, want %q", body.Message, c.wantMessage)
				}

				write := postDataWrite(t, srv, form.address+c.query, `{"t":"Changed"}`, nil)
				if write.Code != http.StatusBadRequest {
					t.Fatalf("POST = %d, want 400: %s", write.Code, write.Body.String())
				}
				if got := decodeError(t, write).Error; got != c.wantError {
					t.Errorf("POST error = %q, want %q", got, c.wantError)
				}
				assertUnchanged(t, path, queryWriteDoc)
			})
		}
	}

	// A rules value that parses but is not rules at all: the engine refuses null roots.
	for _, form := range forms {
		w := postDataWrite(t, srv, form.address+"?data=null", `{"t":"Changed"}`, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: POST ?data=null = %d, want 400: %s", form.name, w.Code, w.Body.String())
		}
		if got := decodeError(t, w).Error; got != "Invalid extraction rules" {
			t.Errorf("%s: error = %q", form.name, got)
		}
	}
	assertUnchanged(t, path, queryWriteDoc)
}

// The plain address accepts a JSON write only when the URL names a document AND carries a data
// parameter. Everything else keeps the 405 the catch-all used to answer with.
func TestDataFileWriteWithoutDataParameterIsRefused(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)
	_, path := registerFixture(t, srv, "not-a-write.htmlclay", queryWriteDoc)

	cases := []struct{ name, target string }{
		{"no query", "/fixtures/not-a-write.htmlclay"},
		{"unrelated query", "/fixtures/not-a-write.htmlclay?other=1"},
		{"non-extractable path", `/fixtures/style.css?data={t:"h1"}`},
		{"reserved read route", `/_/read/` + f.Token + `?data={t:"h1"}`},
		{"unmatched reserved route", `/_/meta/fixtures/not-a-write.htmlclay?data={t:"h1"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := postDataWrite(t, srv, c.target, `{"t":"World"}`, nil)
			if w.Code != http.StatusMethodNotAllowed {
				t.Fatalf("POST %s = %d, want 405: %s", c.target, w.Code, w.Body.String())
			}
			if got := w.Header().Get("Allow"); got != "GET, HEAD" {
				t.Errorf("Allow = %q, want %q", got, "GET, HEAD")
			}
			if strings.Contains(w.Body.String(), `"t"`) {
				t.Errorf("the refusal answered like a data response: %s", w.Body.String())
			}
			assertUnchanged(t, path, queryWriteDoc)
		})
	}

	// A reserved subtree is refused by prefix, whether or not the specific route matched: /_/save
	// takes one token segment, so this address reaches the catch-all and is still not a write.
	browser := map[string]string{
		"Sec-Fetch-Site": "same-origin",
		"Origin":         fmt.Sprintf("http://127.0.0.1:%d", srv.port),
	}
	stolen := postDataWrite(t, srv, `/_/save/fixtures/not-a-write.htmlclay?data={t:"h1"}`, `{"t":"World"}`, browser)
	if stolen.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /_/save/<a document path> = %d, want 405: %s", stolen.Code, stolen.Body.String())
	}
	assertUnchanged(t, path, queryWriteDoc)
}

// The conditions a conditional write has always honored still hold through the query face, and a
// write the engine refuses whole leaves the document exactly as it was.
func TestDataQueryWriteHonorsConditionalsAndRefusals(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	const q = `?data={title:"h1"}`

	t.Run("stale If-Match", func(t *testing.T) {
		_, path := registerFixture(t, srv, "conditional.htmlclay", queryWriteDoc)
		w := postDataWrite(t, srv, "/fixtures/conditional.htmlclay"+q, `{"title":"World"}`,
			map[string]string{"If-Match": `"wrong"`})
		if w.Code != http.StatusPreconditionFailed {
			t.Fatalf("status = %d, want 412: %s", w.Code, w.Body.String())
		}
		if want := specwire.Etag([]byte(queryWriteDoc)); w.Header().Get("ETag") != want {
			t.Errorf("412 ETag = %q, want the current stamp %q", w.Header().Get("ETag"), want)
		}
		assertUnchanged(t, path, queryWriteDoc)
	})

	t.Run("no-op", func(t *testing.T) {
		_, path := registerFixture(t, srv, "noop.htmlclay", queryWriteDoc)
		before, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		w := postDataWrite(t, srv, "/fixtures/noop.htmlclay"+q, `{"title":"Hello"}`, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}
		assertUnchanged(t, path, queryWriteDoc)
		after, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !after.ModTime().Equal(before.ModTime()) {
			t.Errorf("mtime moved from %v to %v", before.ModTime(), after.ModTime())
		}
	})

	t.Run("protected script fixture", func(t *testing.T) {
		doc := `<!DOCTYPE html>
<html lang="en">
<head><title>Test</title><script id="cfg">var a = 1;</script></head>
<body><h1>Hello</h1><p>Untouched</p></body>
</html>
`
		_, path := registerFixture(t, srv, "script.htmlclay", doc)
		w := postDataWrite(t, srv, `/fixtures/script.htmlclay?data={cfg:"#cfg",title:"h1"}`,
			`{"cfg":"var a = 2;","title":"World"}`, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
		}
		if got := decodeWriteError(t, w).Error; got != "Write refused" {
			t.Errorf("error = %q, want %q", got, "Write refused")
		}
		assertUnchanged(t, path, doc)
	})
}

// A write through the plain address is still a browser request when the browser says so, and then it
// needs the target file's own Save-Token, exactly as the /_/api write does.
func TestDataFileWriteBrowserNeedsTheTargetFilesSaveToken(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	f, path := registerFixture(t, srv, "browser.htmlclay", queryWriteDoc)
	const target = `/fixtures/browser.htmlclay?data={title:"h1"}`

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

	foreign := postDataWrite(t, srv, target, `{"title":"World"}`,
		map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://evil.example"})
	if foreign.Code != http.StatusForbidden {
		t.Fatalf("foreign Origin = %d, want 403: %s", foreign.Code, foreign.Body.String())
	}

	noToken := postDataWrite(t, srv, target, `{"title":"World"}`, browser(nil))
	if noToken.Code != http.StatusForbidden {
		t.Fatalf("no token = %d, want 403: %s", noToken.Code, noToken.Body.String())
	}
	if got := decodeWriteError(t, noToken).Error; got != "Save-Token required" {
		t.Errorf("error = %q, want %q", got, "Save-Token required")
	}
	assertUnchanged(t, path, queryWriteDoc)

	withToken := postDataWrite(t, srv, target, `{"title":"World"}`,
		browser(map[string]string{helperTokenHeader: f.Token}))
	if withToken.Code != http.StatusOK {
		t.Fatalf("the file's own token = %d, want 200: %s", withToken.Code, withToken.Body.String())
	}
	if got := readDoc(t, path); !strings.Contains(got, "<h1>World</h1>") {
		t.Errorf("the token-authorized write did not land:\n%s", got)
	}
}

// The query face follows the same path resolution as the GET that read it: an encoded file name and
// a client-side route suffix both name the same file.
func TestDataQueryWriteTargetsTheSameFileAsTheGet(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	const q = `?data={title:"h1"}`

	_, encoded := registerFixture(t, srv, "my page.htmlclay", queryWriteDoc)
	if w := serve(t, srv, "/fixtures/my%20page.htmlclay"+q, nil); w.Code != http.StatusOK {
		t.Fatalf("GET an encoded name = %d: %s", w.Code, w.Body.String())
	}
	if w := postDataWrite(t, srv, "/fixtures/my%20page.htmlclay"+q, `{"title":"World"}`, nil); w.Code != http.StatusOK {
		t.Fatalf("POST an encoded name = %d: %s", w.Code, w.Body.String())
	}
	if got := readDoc(t, encoded); !strings.Contains(got, "<h1>World</h1>") {
		t.Errorf("the encoded-name write did not land:\n%s", got)
	}
	if w := serve(t, srv, "/_/api/fixtures/my%20page.htmlclay"+q, nil); w.Code != http.StatusOK || w.Body.String() != `{"title":"World"}` {
		t.Errorf("the other alias = %d %s", w.Code, w.Body.String())
	}

	_, spa := registerFixture(t, srv, "spa.htmlclay", queryWriteDoc)
	if w := postDataWrite(t, srv, "/fixtures/spa.htmlclay/spa/deep"+q, `{"title":"World"}`, nil); w.Code != http.StatusOK {
		t.Fatalf("POST a client-side route = %d: %s", w.Code, w.Body.String())
	}
	if got := readDoc(t, spa); !strings.Contains(got, "<h1>World</h1>") {
		t.Errorf("the client-side-route write did not land:\n%s", got)
	}
	viaSuffix := serve(t, srv, "/fixtures/spa.htmlclay/spa/deep"+q, nil)
	viaPlain := serve(t, srv, "/fixtures/spa.htmlclay"+q, nil)
	if viaSuffix.Code != http.StatusOK || viaSuffix.Body.String() != `{"title":"World"}` {
		t.Errorf("the suffix alias = %d %s", viaSuffix.Code, viaSuffix.Body.String())
	}
	if viaPlain.Body.String() != viaSuffix.Body.String() {
		t.Errorf("the suffix alias answered %s, the plain path %s", viaSuffix.Body.String(), viaPlain.Body.String())
	}
}

// The 1 MB body limit is the write face's, so the alias carries it too, on both addresses.
func TestDataQueryWriteKeepsTheBodyLimitOnBothAddresses(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	_, path := registerFixture(t, srv, "limit.htmlclay", queryWriteDoc)
	big := `{"title":"` + strings.Repeat("a", 1<<20) + `"}`

	for _, address := range []string{
		"/fixtures/limit.htmlclay",
		"/_/api/fixtures/limit.htmlclay",
	} {
		w := postDataWrite(t, srv, address+`?data={title:"h1"}`, big, nil)
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("%s = %d, want 413: %s", address, w.Code, w.Body.String())
		}
		if got := decodeWriteError(t, w).Error; got != "Payload Too Large" {
			t.Errorf("%s error = %q", address, got)
		}
	}
	assertUnchanged(t, path, queryWriteDoc)
}

func TestDataQueryInvalidOmittedRuleDoesNotWrite(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	_, path := registerFixture(t, srv, "invalid-omitted.htmlclay", queryWriteDoc)
	for _, prefix := range []string{"/fixtures/", "/_/api/fixtures/"} {
		w := postDataWrite(t, srv, prefix+`invalid-omitted.htmlclay?data={title:h1,unused:"["}`, `{"title":"Wrong"}`, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400: %s", prefix, w.Code, w.Body.String())
		}
		assertUnchanged(t, path, queryWriteDoc)
	}
}
