package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/panphora/htmlclay/internal/session"
	"github.com/panphora/htmlclay/internal/specwire"
	"github.com/panphora/htmlclay/internal/versions"
)

func saveThroughMux(t *testing.T, srv *Server, f *session.File, body, ifMatch string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/_/save/"+f.Token, strings.NewReader(body))
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	sameOriginHeaders(req)
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)
	return w
}

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %v (%q)", err, w.Body.String())
	}
	return out
}

func metaOf(t *testing.T, srv *Server, f *session.File) map[string]any {
	t.Helper()
	return decode(t, getThroughMux(t, srv, "/_/meta/"+f.Token))
}

// Reads the file's history through the same route a client would, so the count is
// what the person can actually see rather than what the store happens to hold.
func listVersionNames(t *testing.T, srv *Server, f *session.File) []string {
	t.Helper()
	w := getThroughMux(t, srv, "/_/versions/"+f.Token)
	if w.Code != 200 {
		t.Fatalf("listing versions: got %d (%q)", w.Code, w.Body.String())
	}
	var out struct {
		Versions []struct {
			Name string `json:"name"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("version listing is not JSON: %v (%q)", err, w.Body.String())
	}
	names := make([]string, 0, len(out.Versions))
	for _, v := range out.Versions {
		names = append(names, v.Name)
	}
	return names
}

func docString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const doc = `<!DOCTYPE html><html lang="en"><body><p>v1</p></body></html>`

// --- discovery -------------------------------------------------------------

// §6: a host that advertises `conditional` must honour it. The announcement is
// what makes a client start sending If-Match at all, so announcing without
// enforcing would tell every client it is protected when it is not.
func TestConditionalIsAnnounced(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)
	for _, m := range []map[string]any{metaOf(t, srv, f), decode(t, getThroughMux(t, srv, "/_/meta"))} {
		exts, _ := m["extensions"].([]any)
		found := false
		for _, e := range exts {
			if e == "conditional" {
				found = true
			}
		}
		if !found {
			t.Errorf("conditional not announced: %v", exts)
		}
	}
}

// The stamp lives in the `document` block (§5), because it is the one genuinely
// per-document thing in a discovery answer and the only part a host ever withholds.
func TestMetaCarriesTheStampOfTheBytesOnDisk(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	f := registerPageWithContent(t, srv, "stamped.htmlclay", doc)

	block, ok := metaOf(t, srv, f)["document"].(map[string]any)
	if !ok {
		t.Fatalf("no document block in the per-file meta answer")
	}
	if block["etag"] != specwire.Etag([]byte(doc)) {
		t.Errorf("etag %v does not stamp the bytes on disk (want %q)", block["etag"], specwire.Etag([]byte(doc)))
	}
}

// --- the refusal -----------------------------------------------------------

// The whole point: a stale stamp is refused and the newer copy survives.
func TestAStaleStampIsRefusedAndNothingIsWritten(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	f := registerPageWithContent(t, srv, "guarded.htmlclay", doc)

	stale := specwire.Etag([]byte("something this file never held"))
	w := saveThroughMux(t, srv, f, `<!DOCTYPE html><html><body><p>clobber</p></body></html>`, stale)

	if w.Code != 412 {
		t.Fatalf("got %d, want 412 (%q)", w.Code, w.Body.String())
	}
	got := decode(t, w)
	if got["code"] != "conflict" {
		t.Errorf(`code = %v, want "conflict"`, got["code"])
	}
	if got["ok"] != false {
		t.Errorf("ok = %v, want false", got["ok"])
	}
	if on := docString(t, f.AbsPath); on != doc {
		t.Errorf("a refused save changed the file: %q", on)
	}
}

// "Writing nothing" means nothing at all, not just the document. A refusal that
// still recorded a backup would fill history with versions of a save that never
// happened, and on a first save it would also claim an identity for the file.
func TestARefusedSaveRecordsNoVersion(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	f := registerPageWithContent(t, srv, "untouched.htmlclay", doc)

	before := len(listVersionNames(t, srv, f))
	if w := saveThroughMux(t, srv, f, doc+"<!--x-->", "0000000000000000"); w.Code != 412 {
		t.Fatalf("expected a refusal, got %d", w.Code)
	}
	if after := len(listVersionNames(t, srv, f)); after != before {
		t.Errorf("a refused save wrote %d version(s)", after-before)
	}
}

// A client that computed its stamp wrong is refused rather than dropped back to
// last-write-wins, which is the failure mode that would make the capability a lie.
func TestAnUnparseableStampIsRefused(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	f := registerPageWithContent(t, srv, "garbled.htmlclay", doc)

	for _, field := range []string{"not-a-stamp", `""`, "W/", ", ,"} {
		w := saveThroughMux(t, srv, f, `<html><body>x</body></html>`, field)
		if w.Code != 412 {
			t.Errorf("If-Match %q: got %d, want 412", field, w.Code)
		}
	}
	if on := docString(t, f.AbsPath); on != doc {
		t.Errorf("the file changed under a refused stamp: %q", on)
	}
}

// --- the acceptance --------------------------------------------------------

func TestAMatchingStampSavesAndReturnsTheNewOne(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	f := registerPageWithContent(t, srv, "ok.htmlclay", doc)

	next := `<!DOCTYPE html><html lang="en"><body><p>v2</p></body></html>`
	w := saveThroughMux(t, srv, f, next, specwire.Etag([]byte(doc)))
	if w.Code != 200 {
		t.Fatalf("got %d, want 200 (%q)", w.Code, w.Body.String())
	}
	if got := decode(t, w)["etag"]; got != specwire.Etag([]byte(next)) {
		t.Errorf("etag %v does not stamp what was stored", got)
	}
	if on := docString(t, f.AbsPath); on != next {
		t.Errorf("the save did not land: %q", on)
	}
}

// `*` asks only that the document exist, which is how a client says "I know there
// is something there and I mean to replace it" without holding a stamp.
func TestStarMatchesAnyExistingDocument(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	f := registerPageWithContent(t, srv, "star.htmlclay", doc)

	if w := saveThroughMux(t, srv, f, `<html><body>replaced</body></html>`, "*"); w.Code != 200 {
		t.Fatalf("got %d, want 200 (%q)", w.Code, w.Body.String())
	}
}

// The core save is untouched. This is what makes announcing `conditional` safe for
// every client that never asks for it, including every document already saved.
func TestASaveWithoutIfMatchStillWins(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	f := registerPageWithContent(t, srv, "plain.htmlclay", doc)

	next := `<html><body>last write wins</body></html>`
	if w := saveThroughMux(t, srv, f, next, ""); w.Code != 200 {
		t.Fatalf("got %d, want 200 (%q)", w.Code, w.Body.String())
	}
	if on := docString(t, f.AbsPath); on != next {
		t.Errorf("an unconditional save was refused or lost: %q", on)
	}
}

// The loop a real client runs: seed from discovery, save, keep the stamp the save
// returned, save again. If any link computes a stamp differently the second save
// is refused, so this is the test that catches a mismatch between what meta
// reports, what a save returns, and what the next save is judged against.
func TestAClientCanSaveRepeatedlyFromTheStampsItIsGiven(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	f := registerPageWithContent(t, srv, "loop.htmlclay", doc)

	block := metaOf(t, srv, f)["document"].(map[string]any)
	held, _ := block["etag"].(string)
	if held == "" {
		t.Fatal("discovery seeded no stamp")
	}

	for i := 0; i < 4; i++ {
		body := fmt.Sprintf(`<!DOCTYPE html><html lang="en"><body><p>v%d</p></body></html>`, i+2)
		w := saveThroughMux(t, srv, f, body, held)
		if w.Code != 200 {
			t.Fatalf("save %d refused with %d: %q", i+1, w.Code, w.Body.String())
		}
		held, _ = decode(t, w)["etag"].(string)
		if held == "" {
			t.Fatalf("save %d returned no stamp, so the next one goes out unprotected", i+1)
		}
	}
}

// §6: a stamp describes the bytes the host STORED, never the bytes it was sent.
// Here they genuinely differ, because a save arrives carrying a token attribute
// that is stripped before the write. Stamping what was sent would hand the client
// a value that never matches the file, and its very next conditional save would be
// refused for no reason.
func TestTheStampDescribesWhatWasStoredNotWhatWasSent(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	f := registerPageWithContent(t, srv, "stripped.htmlclay", doc)

	sent := `<!DOCTYPE html><html savetoken="` + f.Token + `" lang="en"><body><p>v2</p></body></html>`
	w := saveThroughMux(t, srv, f, sent, specwire.Etag([]byte(doc)))
	if w.Code != 200 {
		t.Fatalf("got %d (%q)", w.Code, w.Body.String())
	}

	returned, _ := decode(t, w)["etag"].(string)
	if returned == specwire.Etag([]byte(sent)) {
		t.Error("the stamp describes the bytes sent, which are not the bytes on disk")
	}
	if returned != specwire.Etag([]byte(docString(t, f.AbsPath))) {
		t.Errorf("the stamp does not describe the file: %q", returned)
	}

	// And it is usable: the returned stamp must satisfy the next save.
	if w := saveThroughMux(t, srv, f, `<html><body>v3</body></html>`, returned); w.Code != 200 {
		t.Errorf("the stamp a save returned did not satisfy the next save: %d (%q)", w.Code, w.Body.String())
	}
}

// --- attribution (§6 changedBy) --------------------------------------------

// The one value this host can know for certain: the bytes on disk are ones it
// wrote itself during this run, so whatever moved the document past the caller
// went through this same server. On a single-user desktop host that is the person's
// own other tab or device.
func TestAConflictWithOurOwnWriteNamesAnotherTab(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	f := registerPageWithContent(t, srv, "twotabs.htmlclay", doc)

	// Tab A saves, which moves the document past the stamp tab B still holds.
	if w := saveThroughMux(t, srv, f, `<html><body>tab A</body></html>`, ""); w.Code != 200 {
		t.Fatalf("first save: %d", w.Code)
	}

	w := saveThroughMux(t, srv, f, `<html><body>tab B</body></html>`, specwire.Etag([]byte(doc)))
	if w.Code != 412 {
		t.Fatalf("got %d, want 412", w.Code)
	}
	if got := decode(t, w)["changedBy"]; got != "another-tab" {
		t.Errorf("changedBy = %v, want %q", got, "another-tab")
	}
}

// A change this host did not make came from outside it, and nothing here
// distinguishes a text editor from a script from an agent. §6: a host that cannot
// tell omits the field, because the wrong answer available here is the reassuring
// one and a confident wrong attribution is worse than none.
func TestAnOutsideChangeNamesNobody(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	f := registerPageWithContent(t, srv, "outside.htmlclay", doc)

	// Something other than this server rewrites the file.
	if err := os.WriteFile(f.AbsPath, []byte(`<html><body>edited elsewhere</body></html>`), 0644); err != nil {
		t.Fatal(err)
	}

	w := saveThroughMux(t, srv, f, `<html><body>mine</body></html>`, specwire.Etag([]byte(doc)))
	if w.Code != 412 {
		t.Fatalf("got %d, want 412", w.Code)
	}
	if _, named := decode(t, w)["changedBy"]; named {
		t.Errorf("this host guessed at an outside change: %q", w.Body.String())
	}
}

// The record that makes the answer above honest. lastServerWrite alone cannot say
// "we wrote this": it is also seeded by the first observation of a file, so after
// a restart an edit made while htmlclay was closed would look identical to this
// host's own write and would be attributed to the person's own tab.
func TestAFirstObservationIsNotAWriteByThisHost(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	f := registerPageWithContent(t, srv, "restarted.htmlclay", doc)

	// Serving is what performs the first observation, exactly as a fresh process
	// would on the file it finds already on disk.
	if w := getThroughMux(t, srv, "/"+f.RelPath); w.Code != 200 {
		t.Fatalf("serve: %d", w.Code)
	}

	f.Lock()
	seeded := f.LastServerWrite()
	wrote := f.WrittenByThisHost()
	f.Unlock()

	if seeded == "" {
		t.Fatal("the first observation seeded no record; this test proves nothing")
	}
	if wrote {
		t.Error("an observation was recorded as a write by this host")
	}

	// So a conflict against those same bytes names nobody, rather than confidently
	// naming the person's own tab.
	w := saveThroughMux(t, srv, f, `<html><body>mine</body></html>`, specwire.Etag([]byte("a stamp from before the restart")))
	if w.Code != 412 {
		t.Fatalf("got %d, want 412", w.Code)
	}
	if _, named := decode(t, w)["changedBy"]; named {
		t.Errorf("a file this host never wrote was attributed: %q", w.Body.String())
	}
}

// Present-but-empty is not the same as absent, and http.Header.Get cannot tell them
// apart, which is why the handler reads Values instead. An empty field is a client
// whose stamp went wrong; treating it as an unconditional save would silently give
// back the last-write-wins behaviour it asked to be protected from. hyperclay and
// hyperclay-local both refuse it, and a document that saves against all three hosts
// has to get the same answer from each.
func TestAnEmptyIfMatchIsRefusedRatherThanTreatedAsAbsent(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	f := registerPageWithContent(t, srv, "empty-field.htmlclay", doc)

	req := httptest.NewRequest("POST", "/_/save/"+f.Token,
		strings.NewReader(`<html><body>should not land</body></html>`))
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	sameOriginHeaders(req)
	req.Header.Set("If-Match", "")
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("got %d, want 412 (%q)", w.Code, w.Body.String())
	}
	if on := docString(t, f.AbsPath); on != doc {
		t.Errorf("a refused save reached disk: %q", on)
	}
}

// §9 is explicit that a client which finds no `upload` object in the document block
// does not upload and does not probe the route to find out. So announcing `upload`
// while omitting the block is, to every conforming client, identical to not having
// the capability, except that the host looks like it does. The conformance page
// found this; nothing in the Go suite did, because every upload test here calls the
// route directly rather than discovering it first.
func TestMetaReportsTheUploadCapItEnforces(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	f := registerPageWithContent(t, srv, "uploads.htmlclay", doc)

	document, ok := metaOf(t, srv, f)["document"].(map[string]any)
	if !ok {
		t.Fatal("no document block in the discovery answer")
	}
	upload, ok := document["upload"].(map[string]any)
	if !ok {
		t.Fatal("a host announcing `upload` must report document.upload (§9)")
	}
	if allowed, _ := upload["allowed"].(bool); !allowed {
		t.Error("document.upload.allowed is false on a host where every served file is the owner's")
	}
	// The announced number must be the one the route enforces. Two copies drifting
	// apart is worse than announcing nothing: a client would refuse files this host
	// accepts, or send files it refuses.
	if got, _ := upload["maxBytes"].(float64); int64(got) != maxUploadSize {
		t.Errorf("announced maxBytes = %v, want %d (the value MaxBytesReader enforces)", got, maxUploadSize)
	}
}

// A read that fails for any reason other than "there is no file yet" leaves this host
// unable to compare anything, and answering as though the file were empty turns the
// failure into an authorization: the refusal hands back the empty-content etag as if
// it described the file, and a client doing the obvious thing, retrying with the stamp
// it was just given, is let through to replace bytes nobody could read. Nothing backs
// them up either, because the pre-write backup is guarded on the same failed read.
func TestAnUnreadableDocumentIsRefusedRatherThanReadAsEmpty(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	f := registerPageWithContent(t, srv, "locked.htmlclay", doc)

	if err := os.Chmod(f.AbsPath, 0o000); err != nil {
		t.Skipf("cannot deny reads here: %v", err)
	}
	t.Cleanup(func() { os.Chmod(f.AbsPath, 0o644) })
	if _, err := os.ReadFile(f.AbsPath); err == nil {
		t.Skip("mode bits do not deny reads here (running as root?)")
	}

	// The stamp the old code would have handed back, and then accepted on retry.
	empty := specwire.Etag(nil)

	for _, ifMatch := range []string{empty, "*", "some-other-stamp"} {
		w := saveThroughMux(t, srv, f, `<html><body>replacement</body></html>`, ifMatch)
		if w.Code != http.StatusInternalServerError {
			t.Errorf("If-Match %q: got %d, want 500 (a host that cannot read cannot compare)", ifMatch, w.Code)
		}
		if body := decode(t, w); body["etag"] != nil {
			t.Errorf("If-Match %q: the refusal handed back etag %v, which a retry would use", ifMatch, body["etag"])
		}
	}

	os.Chmod(f.AbsPath, 0o644)
	if got := docString(t, f.AbsPath); got != doc {
		t.Errorf("the unreadable document was replaced: %q", got)
	}
}

// RFC 9110 §5.3: a list field split across several physical lines means the same as
// one line joined by commas. Node joins duplicates that way, so hyperclay and
// hyperclay-local already see the whole list; reading only the first line here would
// refuse a save the other two hosts accept, for a request that is entirely legal.
func TestIfMatchIsReadAcrossEveryHeaderLine(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	f := registerPageWithContent(t, srv, "multiline.htmlclay", doc)
	current := specwire.Etag([]byte(doc))

	req := httptest.NewRequest("POST", "/_/save/"+f.Token,
		strings.NewReader(`<html><body><p>v2</p></body></html>`))
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	sameOriginHeaders(req)
	req.Header.Add("If-Match", "a-stamp-from-an-older-load")
	req.Header.Add("If-Match", current)
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("got %d, want 200: the matching tag was on the second line (%s)", w.Code, w.Body.String())
	}
	if got := docString(t, f.AbsPath); !strings.Contains(got, "<p>v2</p>") {
		t.Errorf("the save was accepted but not written: %q", got)
	}
}

// The same reading must not turn two stale lines into a match.
func TestSeveralStaleIfMatchLinesStillRefuse(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	f := registerPageWithContent(t, srv, "multistale.htmlclay", doc)

	req := httptest.NewRequest("POST", "/_/save/"+f.Token,
		strings.NewReader(`<html><body><p>v2</p></body></html>`))
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	sameOriginHeaders(req)
	req.Header.Add("If-Match", "one-stale-stamp")
	req.Header.Add("If-Match", "another-stale-stamp")
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("got %d, want 412", w.Code)
	}
	if got := docString(t, f.AbsPath); got != doc {
		t.Errorf("a refused conditional save still wrote: %q", got)
	}
}

// This route exists for the document that has no other way to learn anything about
// itself: served sandboxed, opaque origin, no cookie, so it presents nothing the host
// recognises. Its own inline script is the reader, and a page written against 1.7.0
// hardcoded `htmlclayid`. No library update reaches that script, so the old key stays
// forever, carrying the same value as the new one.
func TestMetaReportsTheDocumentIdUnderBothSpellings(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	f := registerPageWithContent(t, srv, "identified.htmlclay", doc)

	// Serving is what resolves and stamps the identity.
	if w := getThroughMux(t, srv, "/identified.htmlclay"); w.Code != 200 {
		t.Fatalf("serving the page: %d", w.Code)
	}

	m := metaOf(t, srv, f)
	current, _ := m["documentid"].(string)
	legacy, _ := m["htmlclayid"].(string)

	if current == "" {
		t.Fatal("no documentid in the meta answer, so this test could not compare the two")
	}
	if legacy != current {
		t.Errorf("htmlclayid is %q, want the same value as documentid %q", legacy, current)
	}
}

// §6: a host that cannot honestly say who moved the document omits `changedBy`,
// because the wrong answer available here is the reassuring one. Comparing digests
// alone cannot say it: a file can go B -> C -> B, and once it is back at B every hash
// this host holds matches its own last write again, so an external editor's undo is
// indistinguishable from another of this person's tabs.
func TestAnExternalUndoIsNotAttributedToAnotherTab(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	f := registerPageWithContent(t, srv, "undone.htmlclay", doc)

	// B: this host writes it, through the real save path.
	const b = `<!DOCTYPE html><html lang="en"><body><p>b</p></body></html>`
	if w := saveThroughMux(t, srv, f, b, ""); w.Code != 200 {
		t.Fatalf("seeding save: %d (%s)", w.Code, w.Body.String())
	}

	// C, then back to B, both from outside, both seen by the watcher.
	f.Lock()
	f.RecordStableObservation(versions.Hash([]byte(`<html><body><p>c</p></body></html>`)))
	f.RecordStableObservation(versions.Hash([]byte(b)))
	f.Unlock()

	// A caller still holding the stamp from before B saves.
	w := saveThroughMux(t, srv, f, `<html><body><p>d</p></body></html>`, specwire.Etag([]byte(doc)))
	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("got %d, want 412", w.Code)
	}
	if got := decode(t, w)["changedBy"]; got != nil {
		t.Errorf("changedBy is %q, but the write that moved the file back was external", got)
	}
}

// The other half, so the tightened test cannot pass by never attributing anything:
// with no external write in between, a second tab is still named.
func TestASecondTabIsStillNamed(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	f := registerPageWithContent(t, srv, "twotabs.htmlclay", doc)

	const b = `<!DOCTYPE html><html lang="en"><body><p>b</p></body></html>`
	if w := saveThroughMux(t, srv, f, b, ""); w.Code != 200 {
		t.Fatalf("first tab's save: %d (%s)", w.Code, w.Body.String())
	}

	w := saveThroughMux(t, srv, f, `<html><body><p>d</p></body></html>`, specwire.Etag([]byte(doc)))
	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("got %d, want 412", w.Code)
	}
	if got := decode(t, w)["changedBy"]; got != "another-tab" {
		t.Errorf("changedBy is %v, want another-tab", got)
	}
}
