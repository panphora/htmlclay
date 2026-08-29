package server

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/panphora/htmlclay/internal/htmlutil"
	"github.com/panphora/htmlclay/internal/session"
)

// forBrowser owes two things, and the reason to test it directly is that they are
// separable: the strip has always been here and the stamp was missing from every
// caller, so a change that keeps one and loses the other is the realistic mistake.
func TestForBrowserStripsTheTokenAndStampsTheIdentity(t *testing.T) {
	in := []byte(`<html savetoken="secret" lang="en"><body>hi</body></html>`)

	got := forBrowser(in, "id:"+stableUUID)
	if strings.Contains(got, "secret") || strings.Contains(got, "savetoken") {
		t.Errorf("a save token reached a browser through the broadcast: %q", got)
	}
	if htmlutil.ReadHTMLClayID([]byte(got)) != stableUUID {
		t.Errorf("no tracked identity in the broadcast bytes: %q", got)
	}
}

// A file whose history is keyed by path rather than by an id has no identity to
// stamp, and inventing one would be worse than sending none: it would be adopted
// by the receiving tab and written to disk on its next save.
func TestForBrowserInventsNoIdentity(t *testing.T) {
	in := []byte(`<html lang="en"><body>hi</body></html>`)
	for _, key := range []string{"", "path:deadbeef"} {
		got := forBrowser(in, key)
		// The ATTRIBUTE must be absent, not merely empty. Reading the id back is
		// not enough: a stamp written unconditionally puts `documentid=""` on the
		// root, which reads as no id while still being a host-authored attribute
		// fanned out to every tab and morphed onto their live documents.
		if strings.Contains(got, "documentid") {
			t.Errorf("key %q stamped an empty identity onto the document: %q", key, got)
		}
		if htmlutil.ReadHTMLClayID([]byte(got)) != "" {
			t.Errorf("key %q produced an identity out of nothing: %q", key, got)
		}
	}
}

// listenSaved attaches a subscriber to the saved lane and returns a function that
// waits for the next full document published on it.
func listenSaved(t *testing.T, srv *Server, f *session.File) func() string {
	t.Helper()
	sub := newSubscriber(f.AbsPath, laneSaved)
	srv.hub.add(sub)
	t.Cleanup(func() { srv.hub.remove(sub) })
	return func() string {
		t.Helper()
		html, _ := waitFrame(t, sub, 2*time.Second)["html"].(string)
		return html
	}
}

func trackedID(t *testing.T, srv *Server, f *session.File) string {
	t.Helper()
	id, _ := metaOf(t, srv, f)["documentid"].(string)
	if id == "" {
		t.Fatal("this file has no tracked identity; the test would prove nothing")
	}
	return id
}

// A save fans the new document out to every other tab, and those tabs morph it
// onto their live document. Without the identity in it, a tab whose client does
// not know to protect the attribute loses its own copy, and the next save from
// that tab writes a file with no id at all, orphaning its whole version history.
func TestASaveBroadcastCarriesTheDocumentIdentity(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	f := registerPageWithContent(t, srv, "shared.htmlclay", doc)

	// Serving is what resolves the identity, exactly as opening the file does.
	if w := getThroughMux(t, srv, "/"+f.RelPath); w.Code != 200 {
		t.Fatalf("serve: %d", w.Code)
	}
	want := trackedID(t, srv, f)

	next := listenSaved(t, srv, f)
	if w := saveThroughMux(t, srv, f, `<!DOCTYPE html><html lang="en"><body>v2</body></html>`, ""); w.Code != 200 {
		t.Fatalf("save: %d (%q)", w.Code, w.Body.String())
	}

	html := next()
	if got := htmlutil.ReadHTMLClayID([]byte(html)); got != want {
		t.Errorf("broadcast identity = %q, want %q (%q)", got, want, html)
	}
	if strings.Contains(html, "savetoken") || strings.Contains(html, f.Token) {
		t.Errorf("the broadcast carries a save token: %q", html)
	}
}

// The sharpest case, because a restore deliberately strips the identity from the
// bytes it writes: the host never writes an id to disk (spec §4), and the version
// being restored may carry an id from a clone or a rename that must not be adopted.
// So the bytes going out are guaranteed to have none unless the broadcast puts the
// tracked one back.
func TestARestoreBroadcastCarriesTheDocumentIdentity(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	f := registerPageWithContent(t, srv, "restorable.htmlclay", doc)

	if w := getThroughMux(t, srv, "/"+f.RelPath); w.Code != 200 {
		t.Fatalf("serve: %d", w.Code)
	}
	want := trackedID(t, srv, f)

	// One save, so there is a version to restore.
	if w := saveThroughMux(t, srv, f, `<!DOCTYPE html><html lang="en"><body>v2</body></html>`, ""); w.Code != 200 {
		t.Fatalf("save: %d", w.Code)
	}
	names := listVersionNames(t, srv, f)
	if len(names) == 0 {
		t.Fatal("no version to restore; the test would prove nothing")
	}

	next := listenSaved(t, srv, f)
	req := httptest.NewRequest("POST", "/_/restore/"+f.Token+"/"+names[0], nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	sameOriginHeaders(req)
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("restore: %d (%q)", w.Code, w.Body.String())
	}

	html := next()
	if got := htmlutil.ReadHTMLClayID([]byte(html)); got != want {
		t.Errorf("broadcast identity = %q, want %q (%q)", got, want, html)
	}
	// And the file on disk still carries none, which is the invariant the restore
	// path is protecting and which the broadcast must not have changed.
	if htmlutil.ReadHTMLClayID([]byte(docString(t, f.AbsPath))) != "" {
		t.Error("the restore wrote an identity to disk")
	}
}
