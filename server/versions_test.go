package server

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/panphora/htmlclay/htmlutil"
	"github.com/panphora/htmlclay/logging"
	"github.com/panphora/htmlclay/session"
	"github.com/panphora/htmlclay/versions"
)

const testUUID = "3f2a1b4c-5d6e-4f70-8a9b-0c1d2e3f4a5b"

func page(body string) string {
	return "<!DOCTYPE html>\n<html><body>" + body + "</body></html>"
}

func pageWithID(id, body string) string {
	return "<!DOCTYPE html>\n<html htmlclayid=\"" + id + "\"><body>" + body + "</body></html>"
}

type fileFixture struct {
	srv  *Server
	file *session.File
	home string
}

func setupFileTest(t *testing.T, name, content string) *fileFixture {
	t.Helper()
	homeDir, _ := filepath.EvalSymlinks(t.TempDir())
	filePath := filepath.Join(homeDir, name)
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	mgr := session.NewManagerWithHome(homeDir)
	f, err := mgr.Register(filePath)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	srv := New(ln, mgr, logging.NewStdout(), versions.New(t.TempDir()))
	t.Cleanup(func() { srv.hub.shutdown(); srv.watcher.shutdown() })
	return &fileFixture{srv: srv, file: f, home: homeDir}
}

func (fx *fileFixture) serve(t *testing.T, relPath string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/"+relPath, nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", fx.srv.port)
	req.SetPathValue("path", relPath)
	w := httptest.NewRecorder()
	fx.srv.handleServeFile(w, req)
	return w
}

func (fx *fileFixture) save(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	// A real client serializes the whole document, htmlclayid included, so a save
	// never silently drops the file's identity.
	body = fx.withCurrentID(body)
	req := httptest.NewRequest("POST", "/_/save/"+fx.file.Token, strings.NewReader(body))
	req.Host = fmt.Sprintf("127.0.0.1:%d", fx.srv.port)
	req.SetPathValue("token", fx.file.Token)
	w := httptest.NewRecorder()
	fx.srv.handleSave(w, req)
	return w
}

// withCurrentID stamps the file's live htmlclayid onto body, the way a browser
// round trip does.
func (fx *fileFixture) withCurrentID(body string) string {
	current, err := os.ReadFile(fx.file.AbsPath)
	if err != nil {
		return body
	}
	id := htmlutil.ReadHTMLClayID(current)
	if id == "" || htmlutil.ReadHTMLClayID([]byte(body)) != "" {
		return body
	}
	return string(htmlutil.SetHTMLClayID([]byte(body), id))
}

// key reads the file's resolved backup identity, the same one every handler uses.
// Re-deriving it from current disk bytes is exactly the bug the stored key fixes,
// so the helper must not do it either.
func (fx *fileFixture) key(t *testing.T) string {
	t.Helper()
	fx.file.Lock()
	defer fx.file.Unlock()
	if k := fx.file.HistoryKey(); k != "" {
		return k
	}
	data, _ := os.ReadFile(fx.file.AbsPath)
	return versions.Key(fx.file.AbsPath, data)
}

func (fx *fileFixture) history(t *testing.T) []versions.Entry {
	t.Helper()
	entries, err := fx.srv.versions.List(fx.key(t), fx.file.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func (fx *fileFixture) versionBody(t *testing.T, name string) string {
	t.Helper()
	body, err := fx.srv.versions.Read(fx.key(t), fx.file.AbsPath, name)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func decodeJSON(t *testing.T, w *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var out map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, w.Body.String())
	}
	return out
}

// B1a: a file that has never been saved still has something to restore.
func TestFirstOpenSnapshot(t *testing.T) {
	fx := setupFileTest(t, "notes.htmlclay", page("original"))

	if w := fx.serve(t, "notes.htmlclay"); w.Code != 200 {
		t.Fatalf("serve: %d", w.Code)
	}

	entries := fx.history(t)
	if len(entries) != 1 {
		t.Fatalf("expected 1 snapshot, got %d", len(entries))
	}
	if !strings.Contains(fx.versionBody(t, entries[0].Name), "original") {
		t.Fatal("snapshot does not hold the served content")
	}

	// Serving again does not pile up snapshots.
	fx.serve(t, "notes.htmlclay")
	if entries := fx.history(t); len(entries) != 1 {
		t.Fatalf("re-serving created %d snapshots", len(entries))
	}
}

// B1: the INCOMING save bytes are versioned before the write. Versioning the
// outgoing pre-write bytes would leave the newest successful save as the one
// state never written to history.
func TestSaveVersionsIncomingBytes(t *testing.T) {
	fx := setupFileTest(t, "notes.htmlclay", page("original"))
	fx.serve(t, "notes.htmlclay")

	if w := fx.save(t, page("second")); w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}

	entries := fx.history(t)
	if len(entries) != 2 {
		t.Fatalf("expected 2 versions, got %d", len(entries))
	}
	newest := fx.versionBody(t, entries[0].Name)
	if !strings.Contains(newest, "second") {
		t.Fatalf("newest version is not the bytes just saved: %q", newest)
	}
	onDisk, _ := os.ReadFile(fx.file.AbsPath)
	if versions.Hash([]byte(newest)) != versions.Hash(onDisk) {
		t.Fatal("newest version does not match what landed on disk")
	}
}

// The pre-Hyperclay state survives the first save even when the file was never
// served, matching hyperclay-local's isFirstSave branch.
func TestFirstSaveVersionsExistingContent(t *testing.T) {
	fx := setupFileTest(t, "notes.htmlclay", page("pre-hyperclay"))

	if w := fx.save(t, page("first save")); w.Code != 200 {
		t.Fatalf("save: %d", w.Code)
	}

	entries := fx.history(t)
	if len(entries) != 2 {
		t.Fatalf("expected the existing content plus the incoming one, got %d", len(entries))
	}
	if !strings.Contains(fx.versionBody(t, entries[1].Name), "pre-hyperclay") {
		t.Fatal("the pre-Hyperclay state was not versioned")
	}
	if !strings.Contains(fx.versionBody(t, entries[0].Name), "first save") {
		t.Fatal("the incoming bytes were not versioned")
	}
}

// The htmlclayid injection is a server write, so it advances lastServerWrite, and
// the first save of a new .htmlclay does not false-positive against the server's
// own edit.
func TestIDInjectionDoesNotTriggerStaleWarning(t *testing.T) {
	fx := setupFileTest(t, "notes.htmlclay", page("fresh"))

	w := fx.serve(t, "notes.htmlclay")
	if w.Code != 200 {
		t.Fatalf("serve: %d", w.Code)
	}
	onDisk, _ := os.ReadFile(fx.file.AbsPath)
	if !versions.IsCanonicalUUID(htmlutil.ReadHTMLClayID(onDisk)) {
		t.Fatal("serving did not inject a canonical htmlclayid")
	}

	body := decodeJSON(t, fx.save(t, page("first")))
	if body["msgType"] != "success" {
		t.Fatalf("first save after injection warned: %v", body)
	}
}

// B5: two tabs on one path share one session.File, so the notice is per file. A
// mismatch against lastServerWrite means an outside writer touched the file; the
// write still lands and the response carries msgType "warning".
func TestStaleWriteWarnsAfterExternalChange(t *testing.T) {
	fx := setupFileTest(t, "notes.htmlclay", page("original"))
	fx.serve(t, "notes.htmlclay")

	if body := decodeJSON(t, fx.save(t, page("tab A"))); body["msgType"] != "success" {
		t.Fatalf("clean save warned: %v", body)
	}

	// An outside writer, not this server.
	if err := os.WriteFile(fx.file.AbsPath, []byte(fx.withCurrentID(page("an editor"))), 0644); err != nil {
		t.Fatal(err)
	}

	w := fx.save(t, page("tab A again"))
	if w.Code != 200 {
		t.Fatalf("expected the write to land with 200, got %d", w.Code)
	}
	body := decodeJSON(t, w)
	if body["ok"] != true {
		t.Fatalf("stale write did not return ok:true: %v", body)
	}
	if body["msgType"] != "warning" {
		t.Fatalf("msgType = %v, want warning", body["msgType"])
	}
	if msg, _ := body["msg"].(string); !strings.Contains(msg, fx.file.Name) {
		t.Fatalf("warning does not name the file: %q", msg)
	}

	// The write landed anyway: last-write-wins, we don't fight the write.
	onDisk, _ := os.ReadFile(fx.file.AbsPath)
	if !strings.Contains(string(onDisk), "tab A again") {
		t.Fatal("the save did not land")
	}
	// And the clobbered outside edit is recoverable.
	found := false
	for _, e := range fx.history(t) {
		if strings.Contains(fx.versionBody(t, e.Name), "an editor") {
			found = true
		}
	}
	if !found {
		t.Fatal("the clobbered external content was not versioned")
	}
}

// The accepted limitation, stated as a test rather than left implicit: the notice
// cannot tell two tabs apart, because lastServerWrite advanced on the first
// tab's write. Backups are the safety net; this is a warning, not a guard.
func TestStaleWriteCannotDistinguishTwoTabs(t *testing.T) {
	fx := setupFileTest(t, "notes.htmlclay", page("original"))
	fx.serve(t, "notes.htmlclay")

	if body := decodeJSON(t, fx.save(t, page("tab A"))); body["msgType"] != "success" {
		t.Fatalf("tab A: %v", body)
	}
	// Tab B saves from a stale in-memory DOM. Both tabs share one session.File,
	// and neither client sends a base revision, so this is not flagged.
	body := decodeJSON(t, fx.save(t, page("tab B")))
	if body["msgType"] != "success" {
		t.Fatalf("tab B was flagged, which per-file detection cannot actually do: %v", body)
	}
}

// A save advances both records, so the watcher does not then report the browser's
// own write back as a foreign change.
func TestSaveAdvancesBothRecords(t *testing.T) {
	fx := setupFileTest(t, "notes.htmlclay", page("original"))
	fx.serve(t, "notes.htmlclay")
	fx.save(t, page("written"))

	onDisk, _ := os.ReadFile(fx.file.AbsPath)
	want := versions.Hash(onDisk)

	fx.file.Lock()
	gotWrite, gotStable := fx.file.LastServerWrite(), fx.file.LastStableObservation()
	fx.file.Unlock()

	if gotWrite != want {
		t.Fatal("save did not advance lastServerWrite")
	}
	if gotStable != want {
		t.Fatal("save did not advance lastStableObservation")
	}
}

// Serving never advances lastServerWrite. If it did, tab A could load H0, an
// editor write H1, tab B load H1 and advance the record, and tab A's later save
// would find no mismatch and silently destroy H1.
func TestServingNeverAdvancesLastServerWrite(t *testing.T) {
	fx := setupFileTest(t, "notes.htmlclay", page("original"))
	fx.serve(t, "notes.htmlclay")

	fx.file.Lock()
	seeded := fx.file.LastServerWrite()
	fx.file.Unlock()

	// An outside writer, then a second tab loads the new content.
	if err := os.WriteFile(fx.file.AbsPath, []byte(fx.withCurrentID(page("an editor"))), 0644); err != nil {
		t.Fatal(err)
	}
	fx.serve(t, "notes.htmlclay")

	fx.file.Lock()
	after := fx.file.LastServerWrite()
	fx.file.Unlock()
	if after != seeded {
		t.Fatal("serving advanced lastServerWrite, hiding the stale write from the next save")
	}

	if body := decodeJSON(t, fx.save(t, page("tab A"))); body["msgType"] != "warning" {
		t.Fatalf("the stale write was not detected after a second tab loaded: %v", body)
	}
}

// A hand-edited file can carry `..` or a short string in htmlclayid. The id is
// used only after it validates, so a malformed one falls back to the path key and
// never reaches the filesystem.
func TestMalformedHTMLClayIDFallsBackToPathKey(t *testing.T) {
	fx := setupFileTest(t, "notes.htmlclay", pageWithID("../../escape", "hi"))

	if w := fx.serve(t, "notes.htmlclay"); w.Code != 200 {
		t.Fatalf("serve: %d", w.Code)
	}

	onDisk, _ := os.ReadFile(fx.file.AbsPath)
	if id := htmlutil.ReadHTMLClayID(onDisk); id != "../../escape" {
		t.Fatalf("serving rewrote a present-but-invalid id to %q", id)
	}

	key := versions.Key(fx.file.AbsPath, onDisk)
	if !strings.HasPrefix(key, "path:") {
		t.Fatalf("malformed id produced key %q", key)
	}
	if entries, _ := fx.srv.versions.List(key, fx.file.AbsPath); len(entries) != 1 {
		t.Fatalf("expected the snapshot under the path key, got %d", len(entries))
	}

	base, _ := fx.srv.versions.Dir()
	dirents, _ := os.ReadDir(base)
	for _, d := range dirents {
		if strings.Contains(d.Name(), "..") || strings.Contains(d.Name(), "escape") {
			t.Fatalf("the raw id reached the filesystem as %q", d.Name())
		}
	}
}

// A plain .html file never receives an injected id and keys by its path hash.
func TestPlainHTMLNeverGetsAnID(t *testing.T) {
	fx := setupFileTest(t, "page.html", page("plain"))

	if w := fx.serve(t, "page.html"); w.Code != 200 {
		t.Fatalf("serve: %d", w.Code)
	}

	onDisk, _ := os.ReadFile(fx.file.AbsPath)
	if id := htmlutil.ReadHTMLClayID(onDisk); id != "" {
		t.Fatalf("a plain .html file was given an htmlclayid: %q", id)
	}
	if string(onDisk) != page("plain") {
		t.Fatalf("a plain .html file was modified on disk: %q", onDisk)
	}

	key := versions.Key(fx.file.AbsPath, onDisk)
	if !strings.HasPrefix(key, "path:") {
		t.Fatalf("plain .html key is %q", key)
	}
	if entries, _ := fx.srv.versions.List(key, fx.file.AbsPath); len(entries) != 1 {
		t.Fatal("plain .html was not snapshotted")
	}
}

// Copying a .htmlclay file duplicates its id. On first open, when the id belongs
// to a history bound to a different absolute path that STILL EXISTS, the copy is
// a clone and gets a fresh id, so the fork survives a reopen.
func TestCopiedHTMLClayIDForksAFreshID(t *testing.T) {
	fx := setupFileTest(t, "original.htmlclay", pageWithID(testUUID, "original"))

	// Bind the id to the original path.
	fx.serve(t, "original.htmlclay")

	copyPath := filepath.Join(fx.home, "copy.htmlclay")
	if err := os.WriteFile(copyPath, []byte(pageWithID(testUUID, "original")), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.srv.sessions.Register(copyPath); err != nil {
		t.Fatal(err)
	}

	if w := fx.serve(t, "copy.htmlclay"); w.Code != 200 {
		t.Fatalf("serve copy: %d", w.Code)
	}

	copyBytes, _ := os.ReadFile(copyPath)
	copyID := htmlutil.ReadHTMLClayID(copyBytes)
	if copyID == testUUID {
		t.Fatal("the clone kept the original id, so both files share one history")
	}
	if !versions.IsCanonicalUUID(copyID) {
		t.Fatalf("the clone was given a non-canonical id %q", copyID)
	}

	originalBytes, _ := os.ReadFile(fx.file.AbsPath)
	if htmlutil.ReadHTMLClayID(originalBytes) != testUUID {
		t.Fatal("the original file's id was changed")
	}

	// Two separate histories, one version each.
	origHistory, _ := fx.srv.versions.List("id:"+testUUID, fx.file.AbsPath)
	cloneHistory, _ := fx.srv.versions.List("id:"+copyID, copyPath)
	if len(origHistory) != 1 || len(cloneHistory) != 1 {
		t.Fatalf("histories did not fork: original=%d clone=%d", len(origHistory), len(cloneHistory))
	}
}

// When the old path no longer exists, the same id is a rename rather than a
// clone: keep the id and update the stored path.
func TestRenamedHTMLClayIDKeepsItsIdentity(t *testing.T) {
	fx := setupFileTest(t, "before.htmlclay", pageWithID(testUUID, "content"))
	fx.serve(t, "before.htmlclay")

	if entries, _ := fx.srv.versions.List("id:"+testUUID, fx.file.AbsPath); len(entries) != 1 {
		t.Fatalf("expected the first-open snapshot, got %d", len(entries))
	}

	afterPath := filepath.Join(fx.home, "after.htmlclay")
	if err := os.Rename(fx.file.AbsPath, afterPath); err != nil {
		t.Fatal(err)
	}
	renamed, err := fx.srv.sessions.Register(afterPath)
	if err != nil {
		t.Fatal(err)
	}

	if w := fx.serve(t, "after.htmlclay"); w.Code != 200 {
		t.Fatalf("serve renamed: %d", w.Code)
	}

	afterBytes, _ := os.ReadFile(afterPath)
	if got := htmlutil.ReadHTMLClayID(afterBytes); got != testUUID {
		t.Fatalf("a rename forked the id to %q", got)
	}
	if bound, ok := fx.srv.versions.BoundPath("id:" + testUUID); !ok || bound != renamed.AbsPath {
		t.Fatalf("history was not rebound to the new path: %q", bound)
	}
	if entries, _ := fx.srv.versions.List("id:"+testUUID, afterPath); len(entries) != 1 {
		t.Fatalf("the rename split the history: %d versions", len(entries))
	}
}

// Backups are internal state and are never served on the app's own origin.
func TestInternalVersionsDirectoryIsDenied(t *testing.T) {
	homeDir, _ := filepath.EvalSymlinks(t.TempDir())
	appDir := filepath.Join(homeDir, "app")
	os.MkdirAll(appDir, 0755)
	pagePath := filepath.Join(appDir, "index.htmlclay")
	os.WriteFile(pagePath, []byte(page("app")), 0644)

	// The config directory sits under the user's home on every platform, so put
	// the versions store inside the served tree, which is the exposed shape.
	store := versions.New(filepath.Join(appDir, "versions"))
	if _, err := store.Backup("path:deadbeef", filepath.Join(appDir, "secret.htmlclay"), []byte(page("private"))); err != nil {
		t.Fatal(err)
	}

	mgr := session.NewManagerWithHome(homeDir)
	if _, err := mgr.Register(pagePath); err != nil {
		t.Fatal(err)
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	t.Cleanup(func() { ln.Close() })
	srv := New(ln, mgr, logging.NewStdout(), store)
	t.Cleanup(func() { srv.hub.shutdown(); srv.watcher.shutdown() })

	base, _ := store.Dir()
	dirents, _ := os.ReadDir(base)
	if len(dirents) == 0 {
		t.Fatal("no history folder to probe")
	}
	folder := dirents[0].Name()
	entries, _ := os.ReadDir(filepath.Join(base, folder))

	probes := []string{
		"app/versions",
		"app/versions/" + folder,
		"app/versions/" + folder + "/meta.json",
	}
	for _, e := range entries {
		probes = append(probes, "app/versions/"+folder+"/"+e.Name())
	}

	for _, rel := range probes {
		req := httptest.NewRequest("GET", "/"+rel, nil)
		req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
		req.SetPathValue("path", rel)
		w := httptest.NewRecorder()
		srv.handleServeFile(w, req)

		if w.Code != 404 {
			t.Errorf("%s returned %d, want 404", rel, w.Code)
		}
		if strings.Contains(w.Body.String(), "private") {
			t.Errorf("%s leaked backup content", rel)
		}
	}
}

// B2: restore keeps the target file's canonical htmlclayid rather than adopting
// the one stored inside the version.
func TestRestorePreservesCanonicalID(t *testing.T) {
	fx := setupFileTest(t, "notes.htmlclay", pageWithID(testUUID, "v1"))
	fx.serve(t, "notes.htmlclay")

	// A version whose bytes carry someone else's id, which is what a version
	// taken before a clone fork looks like.
	foreignID := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	if _, err := fx.srv.versions.Backup("id:"+testUUID, fx.file.AbsPath,
		[]byte(pageWithID(foreignID, "restored body"))); err != nil {
		t.Fatal(err)
	}
	name := fx.history(t)[0].Name

	req := httptest.NewRequest("POST", "/_/restore/"+fx.file.Token+"/"+name, nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", fx.srv.port)
	req.SetPathValue("token", fx.file.Token)
	req.SetPathValue("name", name)
	w := httptest.NewRecorder()
	fx.srv.handleRestoreVersion(w, req)

	if w.Code != 200 {
		t.Fatalf("restore: %d %s", w.Code, w.Body.String())
	}

	onDisk, _ := os.ReadFile(fx.file.AbsPath)
	if !strings.Contains(string(onDisk), "restored body") {
		t.Fatalf("content was not restored: %q", onDisk)
	}
	if got := htmlutil.ReadHTMLClayID(onDisk); got != testUUID {
		t.Fatalf("restore adopted the version's id: got %q, want the file's %q", got, testUUID)
	}
	if strings.Contains(string(onDisk), "htmlclaytoken") {
		t.Fatal("restore left a session token in the file")
	}
}

// A plain .html file has no identity of its own, so a version carrying one must
// not donate it.
func TestRestoreStripsForeignIDFromPlainHTML(t *testing.T) {
	fx := setupFileTest(t, "page.html", page("v1"))
	fx.serve(t, "page.html")

	key := versions.Key(fx.file.AbsPath, []byte(page("v1")))
	if _, err := fx.srv.versions.Backup(key, fx.file.AbsPath,
		[]byte(pageWithID(testUUID, "restored body"))); err != nil {
		t.Fatal(err)
	}
	name := fx.history(t)[0].Name

	req := httptest.NewRequest("POST", "/_/restore/"+fx.file.Token+"/"+name, nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", fx.srv.port)
	req.SetPathValue("token", fx.file.Token)
	req.SetPathValue("name", name)
	w := httptest.NewRecorder()
	fx.srv.handleRestoreVersion(w, req)

	if w.Code != 200 {
		t.Fatalf("restore: %d %s", w.Code, w.Body.String())
	}
	onDisk, _ := os.ReadFile(fx.file.AbsPath)
	if id := htmlutil.ReadHTMLClayID(onDisk); id != "" {
		t.Fatalf("restore donated an id %q to a plain .html file", id)
	}
	if !strings.Contains(string(onDisk), "restored body") {
		t.Fatalf("content was not restored: %q", onDisk)
	}
}

// The safety backup before a restore is mandatory, unlike a normal save, or a
// read-only versions directory would allow a destructive restore with no
// recovery copy.
func TestRestoreMakesASafetyBackup(t *testing.T) {
	fx := setupFileTest(t, "notes.htmlclay", page("v1"))
	fx.serve(t, "notes.htmlclay")
	v1 := fx.history(t)[0].Name

	fx.save(t, page("v2"))
	if err := os.WriteFile(fx.file.AbsPath, []byte(fx.withCurrentID(page("unsaved outside edit"))), 0644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/_/restore/"+fx.file.Token+"/"+v1, nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", fx.srv.port)
	req.SetPathValue("token", fx.file.Token)
	req.SetPathValue("name", v1)
	w := httptest.NewRecorder()
	fx.srv.handleRestoreVersion(w, req)
	if w.Code != 200 {
		t.Fatalf("restore: %d %s", w.Code, w.Body.String())
	}

	found := false
	for _, e := range fx.history(t) {
		if strings.Contains(fx.versionBody(t, e.Name), "unsaved outside edit") {
			found = true
		}
	}
	if !found {
		t.Fatal("restore destroyed the pre-restore content with no recovery copy")
	}
}

// A restore advances both records, so it participates in save suppression just
// like a save.
func TestRestoreAdvancesBothRecords(t *testing.T) {
	fx := setupFileTest(t, "notes.htmlclay", page("v1"))
	fx.serve(t, "notes.htmlclay")
	v1 := fx.history(t)[0].Name
	fx.save(t, page("v2"))

	req := httptest.NewRequest("POST", "/_/restore/"+fx.file.Token+"/"+v1, nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", fx.srv.port)
	req.SetPathValue("token", fx.file.Token)
	req.SetPathValue("name", v1)
	fx.srv.handleRestoreVersion(httptest.NewRecorder(), req)

	onDisk, _ := os.ReadFile(fx.file.AbsPath)
	want := versions.Hash(onDisk)

	fx.file.Lock()
	gotWrite, gotStable := fx.file.LastServerWrite(), fx.file.LastStableObservation()
	fx.file.Unlock()
	if gotWrite != want || gotStable != want {
		t.Fatal("restore did not advance both per-file records")
	}

	// And the save immediately after a restore is not flagged as stale.
	if body := decodeJSON(t, fx.save(t, page("after restore"))); body["msgType"] != "success" {
		t.Fatalf("the save after a restore warned: %v", body)
	}
}

// HasHTMLTag is not sufficient: it accepts `<html><body>partial`. A restore
// requires a complete document.
func TestRestoreRejectsIncompleteDocument(t *testing.T) {
	fx := setupFileTest(t, "notes.htmlclay", page("v1"))
	fx.serve(t, "notes.htmlclay")

	data, _ := os.ReadFile(fx.file.AbsPath)
	key := versions.Key(fx.file.AbsPath, data)
	if _, err := fx.srv.versions.Backup(key, fx.file.AbsPath, []byte("<html><body>partial")); err != nil {
		t.Fatal(err)
	}
	truncated := fx.history(t)[0].Name

	req := httptest.NewRequest("POST", "/_/restore/"+fx.file.Token+"/"+truncated, nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", fx.srv.port)
	req.SetPathValue("token", fx.file.Token)
	req.SetPathValue("name", truncated)
	w := httptest.NewRecorder()
	fx.srv.handleRestoreVersion(w, req)

	if w.Code != 400 {
		t.Fatalf("expected 400 for a truncated version, got %d", w.Code)
	}
	onDisk, _ := os.ReadFile(fx.file.AbsPath)
	if !strings.Contains(string(onDisk), "v1") {
		t.Fatalf("the truncated version overwrote the file: %q", onDisk)
	}
}

func TestVersionRoutesRejectCraftedNames(t *testing.T) {
	fx := setupFileTest(t, "notes.htmlclay", page("v1"))
	fx.serve(t, "notes.htmlclay")

	for _, name := range []string{"meta.json", "../meta.json", "../../../etc/passwd", "x.html", ""} {
		readReq := httptest.NewRequest("GET", "/_/version/"+fx.file.Token+"/x", nil)
		readReq.Host = fmt.Sprintf("127.0.0.1:%d", fx.srv.port)
		readReq.SetPathValue("token", fx.file.Token)
		readReq.SetPathValue("name", name)
		rw := httptest.NewRecorder()
		fx.srv.handleReadVersion(rw, readReq)
		if rw.Code == 200 {
			t.Errorf("read accepted %q", name)
		}

		restoreReq := httptest.NewRequest("POST", "/_/restore/"+fx.file.Token+"/x", nil)
		restoreReq.Host = fmt.Sprintf("127.0.0.1:%d", fx.srv.port)
		restoreReq.SetPathValue("token", fx.file.Token)
		restoreReq.SetPathValue("name", name)
		sw := httptest.NewRecorder()
		fx.srv.handleRestoreVersion(sw, restoreReq)
		if sw.Code == 200 {
			t.Errorf("restore accepted %q", name)
		}
	}
}

func TestVersionRoutesRequireAValidToken(t *testing.T) {
	fx := setupFileTest(t, "notes.htmlclay", page("v1"))
	fx.serve(t, "notes.htmlclay")
	name := fx.history(t)[0].Name

	listReq := httptest.NewRequest("GET", "/_/versions/bogus", nil)
	listReq.Host = fmt.Sprintf("127.0.0.1:%d", fx.srv.port)
	listReq.SetPathValue("token", "bogus")
	lw := httptest.NewRecorder()
	fx.srv.handleListVersions(lw, listReq)
	if lw.Code != 401 {
		t.Errorf("list with a bogus token returned %d", lw.Code)
	}

	readReq := httptest.NewRequest("GET", "/_/version/bogus/"+name, nil)
	readReq.Host = fmt.Sprintf("127.0.0.1:%d", fx.srv.port)
	readReq.SetPathValue("token", "bogus")
	readReq.SetPathValue("name", name)
	rw := httptest.NewRecorder()
	fx.srv.handleReadVersion(rw, readReq)
	if rw.Code != 401 {
		t.Errorf("read with a bogus token returned %d", rw.Code)
	}
}

func TestListVersionsIsNewestFirstAndNoStore(t *testing.T) {
	fx := setupFileTest(t, "notes.htmlclay", page("v1"))
	fx.serve(t, "notes.htmlclay")
	fx.save(t, page("v2"))
	fx.save(t, page("v3"))

	req := httptest.NewRequest("GET", "/_/versions/"+fx.file.Token, nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", fx.srv.port)
	req.SetPathValue("token", fx.file.Token)
	w := httptest.NewRecorder()
	fx.srv.handleListVersions(w, req)

	if w.Code != 200 {
		t.Fatalf("list: %d", w.Code)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("token-bearing response Cache-Control = %q", got)
	}

	var out struct {
		Versions []versions.Entry `json:"versions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Versions) != 3 {
		t.Fatalf("expected 3 versions, got %d", len(out.Versions))
	}
	newest := fx.versionBody(t, out.Versions[0].Name)
	if !strings.Contains(newest, "v3") {
		t.Fatalf("list is not newest first: %q", newest)
	}
}

func TestRedactPathHidesTokensOnVersionRoutes(t *testing.T) {
	cases := map[string]string{
		"/_/versions/SECRET":          "/_/versions/<redacted>",
		"/_/version/SECRET/2026.html": "/_/version/<redacted>",
		"/_/restore/SECRET/2026.html": "/_/restore/<redacted>",
		"/_/save/SECRET":              "/_/save/<redacted>",
		"/_/live-sync/stream":         "/_/live-sync/stream",
		"/notes.htmlclay":             "/notes.htmlclay",
	}
	for in, want := range cases {
		if got := redactPath(in); got != want {
			t.Errorf("redactPath(%q) = %q, want %q", in, got, want)
		}
	}
}
