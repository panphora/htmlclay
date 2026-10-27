package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/panphora/htmlclay/internal/specwire"
	"github.com/panphora/htmlclay/internal/versions"
)

func getPageState(t *testing.T, srv *Server, token, ifMatch string) *httptest.ResponseRecorder {
	t.Helper()
	headers := map[string]string{}
	if ifMatch != "" {
		headers["If-Match"] = ifMatch
	}
	return serve(t, srv, "/_/page-state/"+token, headers)
}

func diskEtag(t *testing.T, path string) (string, []byte) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return specwire.Etag(data), data
}

func isHex32(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

func pageStateMetaDocument(t *testing.T, srv *Server, token string) map[string]any {
	t.Helper()
	w := getThroughMux(t, srv, "/_/meta/"+token)
	if w.Code != 200 {
		t.Fatalf("meta: %d %s", w.Code, w.Body.String())
	}
	document, ok := decode(t, w)["document"].(map[string]any)
	if !ok {
		t.Fatalf("meta answer carries no document block: %s", w.Body.String())
	}
	return document
}

func TestPageStateResourceReturnsTheStateForTheseBytes(t *testing.T) {
	fx := newPageStateFixture(t)
	fx.write(t, "resource.htmlclay", pageStateDocument("", "one"))
	f := fx.open(t, "resource.htmlclay")
	if w := fx.serve(t, "resource.htmlclay", nil); w.Code != 200 {
		t.Fatalf("serve: %d %s", w.Code, w.Body.String())
	}

	tag := pageStateYJS("state-1")
	if w := fx.save(t, f, pageStateDocument(tag, "two")); w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	etag, _ := diskEtag(t, f.AbsPath)

	w := getPageState(t, fx.srv, f.Token, etag)
	if w.Code != 200 {
		t.Fatalf("page state: %d %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("ETag"); got != etag {
		t.Fatalf("page state ETag = %q, want %q", got, etag)
	}
	body := decode(t, w)
	if body["tag"] != tag {
		t.Fatalf("tag = %v, want %q", body["tag"], tag)
	}
	if body["etag"] != etag {
		t.Fatalf("body etag = %v, want %q", body["etag"], etag)
	}
	if generation, _ := body["generation"].(string); !isHex32(generation) {
		t.Fatalf("generation = %v, want 32 hex characters", body["generation"])
	}
	if body["stateHash"] != versions.Hash([]byte(tag)) {
		t.Fatalf("stateHash = %v, want the hash of the tag", body["stateHash"])
	}
	document := pageStateMetaDocument(t, fx.srv, f.Token)
	if document["documentid"] == "" {
		t.Fatalf("the meta answer names no documentid: %v", document)
	}
	if body["documentId"] != document["documentid"] {
		t.Fatalf("documentId = %v, want %v", body["documentId"], document["documentid"])
	}
}

func TestPageStateResourceRefusesAForeignEtag(t *testing.T) {
	fx := newPageStateFixture(t)
	fx.write(t, "eta.htmlclay", pageStateDocument("", "one"))
	f := fx.open(t, "eta.htmlclay")
	fx.serve(t, "eta.htmlclay", nil)
	if w := fx.save(t, f, pageStateDocument(pageStateYJS("state-1"), "two")); w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	etag, _ := diskEtag(t, f.AbsPath)
	foreign := specwire.Etag([]byte("bytes this document never held"))

	for _, test := range []struct{ name, ifMatch string }{
		{"foreign etag", foreign},
		{"wildcard", "*"},
		{"a list", foreign + ", " + etag},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := getPageState(t, fx.srv, f.Token, test.ifMatch)
			if w.Code != http.StatusPreconditionFailed {
				t.Fatalf("If-Match %q = %d %s", test.ifMatch, w.Code, w.Body.String())
			}
			body := decode(t, w)
			if body["code"] != "etag-mismatch" || body["etag"] != etag {
				t.Fatalf("refusal = %v, want etag-mismatch naming %q", body, etag)
			}
			if _, ok := body["tag"]; ok {
				t.Fatalf("a refused request was handed the state: %v", body)
			}
		})
	}

	w := getPageState(t, fx.srv, f.Token, "")
	if w.Code != http.StatusPreconditionRequired || decode(t, w)["code"] != "precondition-required" {
		t.Fatalf("no If-Match = %d %s", w.Code, w.Body.String())
	}
}

func TestPageStateResourceHasNothingForUntrackedBytes(t *testing.T) {
	fx := newPageStateFixture(t)
	fx.write(t, "untracked.htmlclay", pageStateDocument("", "one"))
	f := fx.open(t, "untracked.htmlclay")
	if w := fx.serve(t, "untracked.htmlclay", nil); w.Code != 200 {
		t.Fatalf("serve: %d %s", w.Code, w.Body.String())
	}
	etag, _ := diskEtag(t, f.AbsPath)

	w := getPageState(t, fx.srv, f.Token, etag)
	if w.Code != http.StatusNotFound || decode(t, w)["code"] != "no-page-state" {
		t.Fatalf("never-saved page = %d %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("ETag"); got != etag {
		t.Fatalf("404 ETag = %q, want the disk etag %q", got, etag)
	}

	unknown := getPageState(t, fx.srv, "no-such-token", etag)
	meta := getThroughMux(t, fx.srv, "/_/meta/no-such-token")
	if unknown.Code != meta.Code || unknown.Body.String() != meta.Body.String() {
		t.Fatalf("unknown token = %d %s, but meta answers %d %s",
			unknown.Code, unknown.Body.String(), meta.Code, meta.Body.String())
	}
}

func TestPageStateExternalReplacementNeverExposesStaleState(t *testing.T) {
	fx := newPageStateFixture(t)
	fx.write(t, "external.htmlclay", pageStateDocument("", "one"))
	f := fx.open(t, "external.htmlclay")
	if w := fx.serve(t, "external.htmlclay", nil); w.Code != 200 {
		t.Fatalf("serve: %d %s", w.Code, w.Body.String())
	}
	if w := fx.save(t, f, pageStateDocument(pageStateYJS("state-1"), "two")); w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	oldEtag, _ := diskEtag(t, f.AbsPath)

	fx.srv.watcher.poll = 10 * time.Millisecond
	fx.srv.watcher.quiet = 40 * time.Millisecond
	fx.srv.hub.add(newSubscriber(f.AbsPath, laneLive))
	fx.srv.hub.add(newSubscriber(f.AbsPath, laneSaved))
	fx.srv.watcher.watch(f)

	replacement := pageStateDocument("", "replaced elsewhere")
	if err := os.WriteFile(f.AbsPath, []byte(replacement), 0644); err != nil {
		t.Fatal(err)
	}
	want := versions.Hash([]byte(replacement))
	deadline := time.Now().Add(10 * time.Second)
	for {
		f.Lock()
		stable := f.LastStableObservation()
		f.Unlock()
		if stable == want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the watcher never adopted the external replacement")
		}
		time.Sleep(10 * time.Millisecond)
	}

	newEtag, disk := diskEtag(t, f.AbsPath)
	if string(disk) != replacement {
		t.Fatalf("disk does not hold the replacement: %q", disk)
	}
	if served := fx.serve(t, "external.htmlclay", nil).Body.String(); strings.Contains(served, `merge="page"`) {
		t.Fatalf("serving after an external replacement carried a page tag:\n%s", served)
	}
	if w := getPageState(t, fx.srv, f.Token, newEtag); w.Code != http.StatusNotFound || decode(t, w)["code"] != "no-page-state" {
		t.Fatalf("state for the replaced bytes = %d %s", w.Code, w.Body.String())
	}
	if w := getPageState(t, fx.srv, f.Token, oldEtag); w.Code != http.StatusPreconditionFailed {
		t.Fatalf("state for the old etag = %d %s", w.Code, w.Body.String())
	}
}

func TestPageStateRestoreNeverExposesStaleState(t *testing.T) {
	fx := newPageStateFixture(t)
	fx.write(t, "restored.htmlclay", pageStateDocument("", "one"))
	f := fx.open(t, "restored.htmlclay")
	fx.serve(t, "restored.htmlclay", nil)

	first := pageStateYJS("state-first")
	second := pageStateYJS("state-second")
	if w := fx.save(t, f, pageStateDocument(first, "alpha")); w.Code != 200 {
		t.Fatalf("first save: %d %s", w.Code, w.Body.String())
	}
	if w := fx.save(t, f, pageStateDocument(second, "beta")); w.Code != 200 {
		t.Fatalf("second save: %d %s", w.Code, w.Body.String())
	}

	entries, err := fx.srv.versions.List(fx.key(t, f), f.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	target := ""
	for _, entry := range entries {
		body, rErr := fx.srv.versions.Read(fx.key(t, f), f.AbsPath, entry.Name)
		if rErr != nil {
			t.Fatal(rErr)
		}
		if strings.Contains(string(body), "alpha") {
			target = entry.Name
		}
	}
	if target == "" {
		t.Fatalf("no version holds the first article: %v", entries)
	}

	req := httptest.NewRequest("POST", "/_/restore/"+f.Token+"/"+target, nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", fx.srv.port)
	sameOriginHeaders(req)
	rw := httptest.NewRecorder()
	fx.srv.httpServer.Handler.ServeHTTP(rw, req)
	if rw.Code != 200 {
		t.Fatalf("restore: %d %s", rw.Code, rw.Body.String())
	}

	served := fx.serve(t, "restored.htmlclay", nil).Body.String()
	if strings.Contains(served, first) || strings.Contains(served, second) || strings.Contains(served, `merge="page"`) {
		t.Fatalf("serving after a restore carried a page tag:\n%s", served)
	}
	etag, _ := diskEtag(t, f.AbsPath)
	if w := getPageState(t, fx.srv, f.Token, etag); w.Code != http.StatusNotFound || decode(t, w)["code"] != "no-page-state" {
		t.Fatalf("state after a restore = %d %s", w.Code, w.Body.String())
	}
}

func pageStateDataDocument(tag, text string) string {
	doc := "<!DOCTYPE html>\n<html>\n<head>\n" +
		`  <script type="application/json" data-rules-name="api" data-rules-version="1">{"title":"h1"}</script>` + "\n" +
		"</head>\n<body>\n  <h1>Old</h1>\n"
	if tag != "" {
		doc += "  " + tag + "\n"
	}
	return doc + `  <article data-page data-rev="1"><p>` + text + `</p></article>` + "\n</body>\n</html>"
}

func TestPageStateDataWriteDropsState(t *testing.T) {
	fx := newPageStateFixture(t)
	fx.write(t, "data-state.htmlclay", pageStateDataDocument("", "one"))
	f := fx.open(t, "data-state.htmlclay")
	fx.serve(t, "data-state.htmlclay", nil)

	if w := fx.save(t, f, pageStateDataDocument(pageStateYJS("state-1"), "two")); w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	if w := postDataWrite(t, fx.srv, "/_/api/data-state.htmlclay", `{"title":"New"}`, nil); w.Code != 200 {
		t.Fatalf("data write: %d %s", w.Code, w.Body.String())
	}
	onDisk, err := os.ReadFile(f.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(onDisk), "<h1>New</h1>") {
		t.Fatalf("the data write did not change the document: %s", onDisk)
	}
	if served := fx.serve(t, "data-state.htmlclay", nil).Body.String(); strings.Contains(served, `merge="page"`) {
		t.Fatalf("serving after a data API write carried a page tag:\n%s", served)
	}
}

func TestPageStateDownloadsCarryNoTag(t *testing.T) {
	fx := newPageStateFixture(t)
	fx.write(t, "downloaded.htmlclay", pageStateDocument("", "one"))
	f := fx.open(t, "downloaded.htmlclay")
	fx.serve(t, "downloaded.htmlclay", nil)
	if w := fx.save(t, f, pageStateDocument(pageStateYJS("state-1"), "two")); w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	if served := fx.serve(t, "downloaded.htmlclay", nil).Body.String(); !strings.Contains(served, `merge="page"`) {
		t.Fatal("the normal serve lost the page tag, so the export check proves nothing")
	}

	out, err := ExportDocumentZip(f.AbsPath, t.TempDir(), "")
	if err != nil {
		t.Fatalf("ExportDocumentZip: %v", err)
	}
	entries := zipEntries(t, out)
	if len(entries) == 0 {
		t.Fatalf("the export is empty: %s", out)
	}
	for name, content := range entries {
		if strings.Contains(content, `merge="page"`) {
			t.Fatalf("the export entry %q carries the page tag:\n%s", name, content)
		}
	}
}

func TestPageStateMetaNamesTheResourceAndKeepsCapabilityOff(t *testing.T) {
	fx := newPageStateFixture(t)
	fx.write(t, "named.htmlclay", pageStateDocument("", "one"))
	f := fx.open(t, "named.htmlclay")
	fx.serve(t, "named.htmlclay", nil)

	document := pageStateMetaDocument(t, fx.srv, f.Token)
	pageState, ok := document["pageState"].(map[string]any)
	if !ok {
		t.Fatalf("the document block names no page state: %v", document)
	}
	if want := "/_/page-state/" + f.Token; pageState["href"] != want {
		t.Fatalf("pageState.href = %v, want %q", pageState["href"], want)
	}

	answers := []*httptest.ResponseRecorder{
		getThroughMux(t, fx.srv, "/_/meta/"+f.Token),
		getThroughMux(t, fx.srv, "/_/meta"),
	}
	for _, w := range answers {
		if strings.Contains(w.Body.String(), "page-recovery-v1") {
			t.Fatalf("a meta answer announces page-recovery-v1: %s", w.Body.String())
		}
	}

	host := decode(t, answers[1])
	if _, ok := host["document"]; ok {
		t.Fatalf("the host-scope answer carries a document block: %s", answers[1].Body.String())
	}
	extensions, _ := host["extensions"].([]any)
	for _, extension := range extensions {
		if extension == "page-recovery-v1" {
			t.Fatalf("the host extension list carries page-recovery-v1: %v", extensions)
		}
	}
}
