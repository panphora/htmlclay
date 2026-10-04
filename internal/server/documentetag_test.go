package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/panphora/htmlclay/internal/htmlutil"
	"github.com/panphora/htmlclay/internal/specwire"
)

// decodedDocumentETag reads the root element's documentetag the way a client does:
// by parsing the response, not by searching the bytes for a substring. The
// difference matters here, because the file the test serves through carries the
// same word inside a child element's own attribute.
func decodedDocumentETag(t *testing.T, body []byte) string {
	t.Helper()
	z := html.NewTokenizer(bytes.NewReader(body))
	for {
		switch z.Next() {
		case html.ErrorToken:
			t.Fatalf("no root element in %q", body)
		case html.StartTagToken, html.SelfClosingTagToken:
			tok := z.Token()
			if !strings.EqualFold(tok.Data, "html") {
				continue
			}
			for _, a := range tok.Attr {
				if strings.EqualFold(a.Key, "documentetag") {
					return a.Val
				}
			}
			return ""
		}
	}
}

// The stamp is computed from the bytes read off disk and nothing else, so it must
// equal what this host already stamps those bytes with. Any other source — the id
// this serve injects, the token, the response as sent — would be a value no client
// could ever match against a discovery answer.
func TestServeDocumentETagStampsNavigationAndFetchFromDiskBytes(t *testing.T) {
	srv, _, content := setupHandlerTest(t)
	want := specwire.Etag([]byte(content))

	nav := serve(t, srv, "/test.htmlclay", map[string]string{"Sec-Fetch-Dest": "document"})
	if nav.Code != 200 {
		t.Fatalf("navigation = %d: %s", nav.Code, nav.Body.String())
	}
	if got := decodedDocumentETag(t, nav.Body.Bytes()); got != want {
		t.Errorf("navigation stamp = %q, want %q (specwire.Etag of the disk bytes)", got, want)
	}
	if !strings.Contains(nav.Body.String(), `savetoken="`) {
		t.Error("the navigation lost its save token")
	}

	// A silent fetch gets the same bytes without the capability. The stamp is not a
	// capability: a read-only reader still has to be able to tell which version it
	// is looking at.
	fetch := serve(t, srv, "/test.htmlclay", map[string]string{"Sec-Fetch-Dest": "empty"})
	if fetch.Code != 200 {
		t.Fatalf("fetch = %d: %s", fetch.Code, fetch.Body.String())
	}
	if got := decodedDocumentETag(t, fetch.Body.Bytes()); got != want {
		t.Errorf("fetch stamp = %q, want %q", got, want)
	}
	if strings.Contains(fetch.Body.String(), "savetoken") {
		t.Errorf("a silent fetch received the save capability: %q", fetch.Body.String())
	}

	// A response attribute is not a write. The file is exactly what it was.
	onDisk, err := os.ReadFile(filepath.Join(srv.sessions.HomeDir(), "test.htmlclay"))
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != content {
		t.Errorf("serving rewrote the file:\n got: %q\nwant: %q", onDisk, content)
	}
}

// A stamp that described the response, or a memoised first serve, would answer the
// same value after the file changed underneath. The whole use of this attribute is
// comparing the version a tab holds with the version on disk now.
func TestServeDocumentETagFollowsTheFileItDescribes(t *testing.T) {
	srv, f, content := setupHandlerTest(t)

	before := decodedDocumentETag(t, get(t, srv, "/test.htmlclay", "test.htmlclay").Body.Bytes())
	updated := content + "\n<!-- a newer write -->"
	if err := os.WriteFile(f.AbsPath, []byte(updated), 0644); err != nil {
		t.Fatal(err)
	}
	after := decodedDocumentETag(t, get(t, srv, "/test.htmlclay", "test.htmlclay").Body.Bytes())

	if before != specwire.Etag([]byte(content)) {
		t.Errorf("first stamp = %q, want %q", before, specwire.Etag([]byte(content)))
	}
	if after != specwire.Etag([]byte(updated)) {
		t.Errorf("second stamp = %q, want %q", after, specwire.Etag([]byte(updated)))
	}
	if before == after {
		t.Error("the stamp did not change with the file")
	}
}

// The data faces return before any of this: a JSON read is not a document response,
// and the attribute describes a navigation. An extract that carried a stamp would
// also be an extract that re-read and re-derived per-file state.
func TestServeDocumentETagIsAbsentFromDataResponses(t *testing.T) {
	srv, f, content := setupHandlerTest(t)

	w := get(t, srv, `/test.htmlclay?data={t:"title"}`, "test.htmlclay")
	if w.Code != 200 {
		t.Fatalf("data request = %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "documentetag") {
		t.Errorf("a data response carries the document stamp: %q", w.Body.String())
	}
	onDisk, err := os.ReadFile(f.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != content {
		t.Errorf("the data request rewrote the file: %q", onDisk)
	}
}

// Saving the response a tab was served is ordinary: a client that copies the whole
// document back carries the stamp it was handed. It describes one response of one
// tab, so it must never reach the file — while the child element's own attribute of
// that name, and the durable id the tab was served, both belong there.
func TestServeDocumentETagIsStrippedOnSave(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	page := `<!DOCTYPE html>` + "\n" + `<html lang="en"><body><p>v1</p>` +
		`<div documentetag="child-owns-this">x</div></body></html>`
	f := registerPageWithContent(t, srv, "stamped.htmlclay", page)

	served := get(t, srv, "/stamped.htmlclay", "stamped.htmlclay")
	if served.Code != 200 {
		t.Fatalf("serving = %d: %s", served.Code, served.Body.String())
	}
	if decodedDocumentETag(t, served.Body.Bytes()) == "" {
		t.Fatal("the serve carried no stamp, so this test could not tell a strip from a no-op")
	}
	servedID := htmlutil.ReadHTMLClayID(served.Body.Bytes())

	if w := saveThroughMux(t, srv, f, served.Body.String(), ""); w.Code != 200 {
		t.Fatalf("save = %d: %s", w.Code, w.Body.String())
	}

	onDisk := docString(t, f.AbsPath)
	if got := decodedDocumentETag(t, []byte(onDisk)); got != "" {
		t.Errorf("the response stamp was written to disk: %q\n%s", got, onDisk)
	}
	if !strings.Contains(onDisk, `<div documentetag="child-owns-this">`) {
		t.Errorf("the child's own attribute was stripped too: %s", onDisk)
	}
	if got := htmlutil.ReadHTMLClayID([]byte(onDisk)); got != servedID {
		t.Errorf("the saved file carries documentid %q, want the served %q", got, servedID)
	}

	// And the next serve stamps the new bytes, not the ones the tab arrived with.
	next := decodedDocumentETag(t, get(t, srv, "/stamped.htmlclay", "stamped.htmlclay").Body.Bytes())
	if next != specwire.Etag([]byte(onDisk)) {
		t.Errorf("stamp after the save = %q, want %q", next, specwire.Etag([]byte(onDisk)))
	}
}

// One tab's original response version is meaningless in another tab: the relay
// hands a peer the current disk bytes, and a stamp riding along would tell that
// peer it holds a version it never fetched. Same rule as the save token, and the
// same seam strips both.
func TestServeDocumentETagIsStrippedFromTheRelay(t *testing.T) {
	srv, f := setupLiveSyncTest(t)
	pageURL := fmt.Sprintf("http://127.0.0.1:%d/page.htmlclay", srv.port)

	sub := newSubscriber(f.AbsPath, laneLive)
	srv.hub.add(sub)

	stamped := `<!DOCTYPE html><html documentetag="somebody-elses-version">` +
		`<body><div documentetag="child-keeps">x</div></body></html>`
	body, err := json.Marshal(map[string]string{"snapshot": stamped, "sender": "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if w := postLiveSync(t, srv, pageURL, string(body)); w.Code != 200 {
		t.Fatalf("relay refused: %d (%s)", w.Code, w.Body.String())
	}

	html, _ := waitFrame(t, sub, time.Second)["html"].(string)
	if strings.Contains(html, "somebody-elses-version") {
		t.Errorf("the relayed frame still carries a document stamp: %q", html)
	}
	if !strings.Contains(html, `<div documentetag="child-keeps">`) {
		t.Errorf("the relay stripped the child's own attribute: %q", html)
	}
}
