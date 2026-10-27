package server

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/panphora/htmlclay/internal/htmlutil"
	"github.com/panphora/htmlclay/internal/logging"
	"github.com/panphora/htmlclay/internal/session"
	"github.com/panphora/htmlclay/internal/specwire"
	"github.com/panphora/htmlclay/internal/versions"
)

func pageStateYJS(seed string) string {
	return `<script type="application/json" merge="page" clay="live">{"v":1,"y":{"seed":"` + seed + `"}}</script>`
}

func pageStateDocument(tag, text string) string {
	doc := "<!DOCTYPE html>\n<html>\n<body>\n  <p>outside</p>\n"
	if tag != "" {
		doc += "  " + tag + "\n"
	}
	return doc + `  <article data-page data-rev="1"><p>` + text + `</p></article>` + "\n</body>\n</html>"
}

// pageStateFixture is one home directory and one version store, with a server
// that can be replaced by a fresh one over the same two directories, the way
// quitting and opening the file again does.
type pageStateFixture struct {
	home  string
	store string
	srv   *Server
}

func newPageStateFixture(t *testing.T) *pageStateFixture {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fx := &pageStateFixture{home: home, store: t.TempDir()}
	fx.start(t)
	return fx
}

func (fx *pageStateFixture) start(t *testing.T) {
	t.Helper()
	mgr := newTestManager(t, fx.home)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	srv := New(ln, mgr, logging.NewStdout(), versions.New(fx.store))
	t.Cleanup(func() { srv.hub.shutdown(); srv.watcher.shutdown() })
	fx.srv = srv
}

func (fx *pageStateFixture) write(t *testing.T, relPath, body string) string {
	t.Helper()
	p := filepath.Join(fx.home, relPath)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

func (fx *pageStateFixture) open(t *testing.T, relPath string) *session.File {
	t.Helper()
	f, err := fx.srv.sessions.Register(filepath.Join(fx.home, relPath), session.ViaOsOpen)
	if err != nil {
		t.Fatalf("register %s: %v", relPath, err)
	}
	return f
}

func (fx *pageStateFixture) serve(t *testing.T, relPath string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return serve(t, fx.srv, "/"+relPath, headers)
}

// stamp puts the file's tracked documentid onto the body the way a browser
// round trip does.
func (fx *pageStateFixture) stamp(t *testing.T, f *session.File, body string) string {
	t.Helper()
	f.Lock()
	id, ok := versions.IDFromKey(f.HistoryKey())
	f.Unlock()
	if ok && htmlutil.ReadHTMLClayID([]byte(body)) == "" {
		body = string(htmlutil.SetHTMLClayID([]byte(body), id))
	}
	return body
}

func (fx *pageStateFixture) save(t *testing.T, f *session.File, body string) *httptest.ResponseRecorder {
	t.Helper()
	return savePageThroughMux(t, fx.srv, f, fx.stamp(t, f, body), "", "", "")
}

func (fx *pageStateFixture) key(t *testing.T, f *session.File) string {
	t.Helper()
	f.Lock()
	defer f.Unlock()
	if k := f.HistoryKey(); k != "" {
		return k
	}
	data, err := os.ReadFile(f.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	return versions.Key(f.AbsPath, data)
}

func (fx *pageStateFixture) stateFor(t *testing.T, f *session.File) versions.PageStateRecord {
	t.Helper()
	data, err := os.ReadFile(f.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	record, ok, err := fx.srv.versions.PageStateFor(fx.key(t, f), f.AbsPath, versions.Hash(data))
	if err != nil {
		t.Fatalf("reading page state: %v", err)
	}
	if !ok {
		t.Fatal("no page state stored for the bytes on disk")
	}
	return record
}

func servedPageTag(t *testing.T, served, article string) string {
	t.Helper()
	at := strings.Index(served, article)
	if at < 0 {
		t.Fatalf("the served document lost its article: %s", served)
	}
	if !strings.Contains(served, `merge="page"`) {
		return ""
	}
	tag := pageadmissionPageTag(t, served)
	if tag == "" {
		t.Fatalf("the served document carries a tag that is not the page tag: %s", served)
	}
	if strings.Index(served, tag) > at {
		t.Fatalf("the page tag was placed after the article: %s", served)
	}
	if strings.Count(served, tag) != 1 {
		t.Fatalf("the served document carries the page tag %d times: %s", strings.Count(served, tag), served)
	}
	return tag
}

func pageadmissionPageTag(t *testing.T, served string) string {
	t.Helper()
	start := strings.Index(served, `<script type="application/json" merge="page"`)
	if start < 0 {
		return ""
	}
	end := strings.Index(served[start:], "</script>")
	if end < 0 {
		return ""
	}
	return served[start : start+end+len("</script>")]
}

func TestPageStateSurvivesSaveAndReopen(t *testing.T) {
	fx := newPageStateFixture(t)
	fx.write(t, "kept.htmlclay", pageStateDocument("", "one"))
	f := fx.open(t, "kept.htmlclay")
	if w := fx.serve(t, "kept.htmlclay", nil); w.Code != 200 {
		t.Fatalf("first serve: %d %s", w.Code, w.Body.String())
	}

	tag := pageStateYJS("state-1")
	if w := fx.save(t, f, pageStateDocument(tag, "two")); w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	disk, err := os.ReadFile(f.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(disk), `merge="page"`) {
		t.Fatalf("the file on disk carries the page tag: %s", disk)
	}

	fx.start(t)
	reopened := fx.open(t, "kept.htmlclay")
	w := fx.serve(t, "kept.htmlclay", nil)
	if w.Code != 200 {
		t.Fatalf("serve after reopen: %d %s", w.Code, w.Body.String())
	}
	served := w.Body.String()
	if got := servedPageTag(t, served, `<article data-page`); got != tag {
		t.Fatalf("served page tag = %q, want %q", got, tag)
	}
	if got, want := decodedDocumentETag(t, w.Body.Bytes()), specwire.Etag(disk); got != want {
		t.Fatalf("documentetag = %q, want %q", got, want)
	}
	if reopened.AbsPath != f.AbsPath {
		t.Fatalf("reopened %q, want %q", reopened.AbsPath, f.AbsPath)
	}
}

func TestPageStateKeepsItsLineage(t *testing.T) {
	fx := newPageStateFixture(t)
	fx.write(t, "lineage.htmlclay", pageStateDocument("", "one"))
	f := fx.open(t, "lineage.htmlclay")
	fx.serve(t, "lineage.htmlclay", nil)

	if w := fx.save(t, f, pageStateDocument(pageStateYJS("state-1"), "two")); w.Code != 200 {
		t.Fatalf("first save: %d %s", w.Code, w.Body.String())
	}
	first := fx.stateFor(t, f)

	if w := fx.save(t, f, pageStateDocument(pageStateYJS("state-2"), "three")); w.Code != 200 {
		t.Fatalf("second save: %d %s", w.Code, w.Body.String())
	}
	second := fx.stateFor(t, f)

	if first.Generation == "" || second.Generation != first.Generation {
		t.Fatalf("generation %q became %q across a save", first.Generation, second.Generation)
	}
	if second.StateHash != versions.Hash([]byte(pageStateYJS("state-2"))) {
		t.Fatal("the sidecar does not hold the tag the second save carried")
	}
}

func TestPageStateConditionalRefusalLeavesState(t *testing.T) {
	fx := newPageStateFixture(t)
	fx.write(t, "conditional.htmlclay", pageStateDocument("", "one"))
	f := fx.open(t, "conditional.htmlclay")
	fx.serve(t, "conditional.htmlclay", nil)

	tag := pageStateYJS("state-1")
	if w := fx.save(t, f, pageStateDocument(tag, "two")); w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	disk, err := os.ReadFile(f.AbsPath)
	if err != nil {
		t.Fatal(err)
	}

	w := savePageThroughMux(t, fx.srv, f, pageStateDocument(pageStateYJS("state-2"), "three"),
		specwire.Etag([]byte("<html></html>")), "", "")
	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale conditional save = %d %s", w.Code, w.Body.String())
	}
	if on, _ := os.ReadFile(f.AbsPath); string(on) != string(disk) {
		t.Fatal("a refused conditional save wrote the file")
	}
	record, ok, err := fx.srv.versions.PageStateFor(fx.key(t, f), f.AbsPath, versions.Hash(disk))
	if err != nil || !ok || record.Tag != tag {
		t.Fatalf("page state after a refusal = %+v, %v, %v", record, ok, err)
	}
}

func TestPageStateSameBytesAdvancesState(t *testing.T) {
	fx := newPageStateFixture(t)
	fx.write(t, "same.htmlclay", pageStateDocument("", "one"))
	f := fx.open(t, "same.htmlclay")
	fx.serve(t, "same.htmlclay", nil)

	if w := fx.save(t, f, pageStateDocument(pageStateYJS("state-1"), "two")); w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	disk, err := os.ReadFile(f.AbsPath)
	if err != nil {
		t.Fatal(err)
	}

	tag2 := pageStateYJS("state-2")
	w := savePageWithWriteThroughMux(t, fx.srv, f, fx.stamp(t, f, pageStateDocument(tag2, "two")), "", "v1")
	body := decode(t, w)
	if w.Code != http.StatusConflict || body["reason"] != "nothing-to-write" {
		t.Fatalf("nothing-to-write save = %d %s", w.Code, w.Body.String())
	}
	if on, _ := os.ReadFile(f.AbsPath); string(on) != string(disk) {
		t.Fatal("the nothing-to-write save wrote the file")
	}
	served := fx.serve(t, "same.htmlclay", nil).Body.String()
	if got := servedPageTag(t, served, `<article data-page`); got != tag2 {
		t.Fatalf("served page tag = %q, want the one the nothing-to-write save carried", got)
	}
}

func TestPageStateFailedFileWriteKeepsOldState(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("read-only folders do not stop writes on Windows")
	}
	fx := newPageStateFixture(t)
	fx.write(t, "failed.htmlclay", pageStateDocument("", "one"))
	f := fx.open(t, "failed.htmlclay")
	fx.serve(t, "failed.htmlclay", nil)

	tag1 := pageStateYJS("state-1")
	if w := fx.save(t, f, pageStateDocument(tag1, "two")); w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	disk, err := os.ReadFile(f.AbsPath)
	if err != nil {
		t.Fatal(err)
	}

	dir := filepath.Dir(f.AbsPath)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	restore := func() {
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(restore)

	w := fx.save(t, f, pageStateDocument(pageStateYJS("state-2"), "three"))
	if w.Code != http.StatusInternalServerError || decode(t, w)["error"] != "write error" {
		restore()
		t.Fatalf("failed write = %d %s", w.Code, w.Body.String())
	}
	restore()

	if on, _ := os.ReadFile(f.AbsPath); string(on) != string(disk) {
		t.Fatal("a failed write changed the file")
	}
	served := fx.serve(t, "failed.htmlclay", nil).Body.String()
	if got := servedPageTag(t, served, `<article data-page`); got != tag1 {
		t.Fatalf("served page tag after a failed write = %q, want %q", got, tag1)
	}

	tag3 := pageStateYJS("state-3")
	if w := fx.save(t, f, pageStateDocument(tag3, "four")); w.Code != 200 {
		t.Fatalf("save after the failure: %d %s", w.Code, w.Body.String())
	}
	served = fx.serve(t, "failed.htmlclay", nil).Body.String()
	if got := servedPageTag(t, served, `<article data-page`); got != tag3 {
		t.Fatalf("served page tag after recovery = %q, want %q", got, tag3)
	}
}

func TestPageStateSidecarFailureRefusesBeforeTheFileWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("read-only folders do not stop writes on Windows")
	}
	fx := newPageStateFixture(t)
	fx.write(t, "sidecar.htmlclay", pageStateDocument("", "one"))
	f := fx.open(t, "sidecar.htmlclay")
	fx.serve(t, "sidecar.htmlclay", nil)

	if w := fx.save(t, f, pageStateDocument(pageStateYJS("state-1"), "two")); w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	disk, err := os.ReadFile(f.AbsPath)
	if err != nil {
		t.Fatal(err)
	}

	folders, err := filepath.Glob(filepath.Join(fx.store, "*", versions.PageStateName))
	if err != nil || len(folders) == 0 {
		t.Fatalf("no sidecar under %s: %v", fx.store, err)
	}
	var restored []string
	for _, sidecar := range folders {
		dir := filepath.Dir(sidecar)
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		restored = append(restored, dir)
	}
	restore := func() {
		for _, dir := range restored {
			if err := os.Chmod(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Cleanup(restore)

	w := fx.save(t, f, pageStateDocument(pageStateYJS("state-2"), "three"))
	restore()
	if w.Code != http.StatusInternalServerError || decode(t, w)["error"] != "cannot store page state" {
		t.Fatalf("sidecar failure = %d %s", w.Code, w.Body.String())
	}
	if on, _ := os.ReadFile(f.AbsPath); string(on) != string(disk) {
		t.Fatal("a save whose page state could not be stored wrote the file")
	}
}

func TestPageStateRawSourceCarriesNoTag(t *testing.T) {
	fx := newPageStateFixture(t)
	fx.write(t, "raw.htmlclay", pageStateDocument("", "one"))
	f := fx.open(t, "raw.htmlclay")
	fx.serve(t, "raw.htmlclay", nil)

	if w := fx.save(t, f, pageStateDocument(pageStateYJS("state-1"), "two")); w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	if served := fx.serve(t, "raw.htmlclay", nil).Body.String(); !strings.Contains(served, `merge="page"`) {
		t.Fatal("the normal serve lost the page tag, so the source check proves nothing")
	}

	source := fx.serve(t, "raw.htmlclay", map[string]string{"Clay-Source": "1"}).Body.String()
	if strings.Contains(source, `merge="page"`) {
		t.Fatalf("a source read carried the page tag:\n%s", source)
	}
	read := getThroughMux(t, fx.srv, "/_/read/"+f.Token)
	if read.Code != 200 {
		t.Fatalf("read route: %d %s", read.Code, read.Body.String())
	}
	if strings.Contains(read.Body.String(), `merge="page"`) {
		t.Fatalf("the read route carried the page tag:\n%s", read.Body.String())
	}
}

func TestPageStateForgedBodyIDStaysWithTheTrackedDocument(t *testing.T) {
	fx := newPageStateFixture(t)
	fx.write(t, "a.htmlclay", pageStateDocument("", "one"))
	fx.write(t, "b.htmlclay", pageStateDocument("", "other"))
	a := fx.open(t, "a.htmlclay")
	b := fx.open(t, "b.htmlclay")
	fx.serve(t, "a.htmlclay", nil)
	fx.serve(t, "b.htmlclay", nil)

	b.Lock()
	bID, ok := versions.IDFromKey(b.HistoryKey())
	b.Unlock()
	if !ok {
		t.Fatal("the second document resolved no identity")
	}

	tag := pageStateYJS("state-1")
	body := htmlutil.SetHTMLClayID([]byte(pageStateDocument(tag, "two")), bID)
	if w := fx.save(t, a, string(body)); w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	if served := fx.serve(t, "a.htmlclay", nil).Body.String(); !strings.Contains(served, `merge="page"`) {
		t.Fatalf("the saved document did not get its page tag back:\n%s", served)
	}
	if served := fx.serve(t, "b.htmlclay", nil).Body.String(); strings.Contains(served, `merge="page"`) {
		t.Fatalf("a body's forged id moved the page state onto another document:\n%s", served)
	}
}

func TestPageStateCopyStartsFreshAndMoveKeeps(t *testing.T) {
	fx := newPageStateFixture(t)
	fx.write(t, "original.htmlclay", pageStateDocument("", "one"))
	f := fx.open(t, "original.htmlclay")
	fx.serve(t, "original.htmlclay", nil)

	tag := pageStateYJS("state-1")
	if w := fx.save(t, f, pageStateDocument(tag, "two")); w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	disk, err := os.ReadFile(f.AbsPath)
	if err != nil {
		t.Fatal(err)
	}

	copyPath := filepath.Join(fx.home, "copied.htmlclay")
	if err := os.WriteFile(copyPath, disk, 0644); err != nil {
		t.Fatal(err)
	}
	fx.open(t, "copied.htmlclay")
	served := fx.serve(t, "copied.htmlclay", nil).Body.String()
	if strings.Contains(served, `merge="page"`) {
		t.Fatalf("a copy inherited the page state of the file it was copied from:\n%s", served)
	}

	if err := os.Rename(f.AbsPath, filepath.Join(fx.home, "moved.htmlclay")); err != nil {
		t.Fatal(err)
	}
	fx.open(t, "moved.htmlclay")
	served = fx.serve(t, "moved.htmlclay", nil).Body.String()
	if got := servedPageTag(t, served, `<article data-page`); got != tag {
		t.Fatalf("a moved document lost its page state: %q", served)
	}
}
