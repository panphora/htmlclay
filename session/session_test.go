package session

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func setupManager(t *testing.T) (*Manager, string) {
	t.Helper()
	homeDir := t.TempDir()
	// On macOS, t.TempDir() returns /var/... which is a symlink to /private/var/...
	// EvalSymlinks in Register resolves to /private/var/..., so homeDir must match.
	resolved, err := filepath.EvalSymlinks(homeDir)
	if err != nil {
		t.Fatal(err)
	}
	mgr := NewManagerWithHome(resolved)
	return mgr, resolved
}

func createTestFile(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("<html></html>"), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRegisterReturnsToken(t *testing.T) {
	mgr, home := setupManager(t)
	path := createTestFile(t, home, "test.htmlclay")

	f, err := mgr.Register(path)
	if err != nil {
		t.Fatalf("Register error: %v", err)
	}
	if len(f.Token) != 43 {
		t.Errorf("expected 43-char token, got %d chars: %q", len(f.Token), f.Token)
	}
	if f.AbsPath != path {
		t.Errorf("expected AbsPath %q, got %q", path, f.AbsPath)
	}
	if f.Name != "test.htmlclay" {
		t.Errorf("expected Name 'test.htmlclay', got %q", f.Name)
	}
}

func TestRegisterSamePathReturnsSameFile(t *testing.T) {
	mgr, home := setupManager(t)
	path := createTestFile(t, home, "test.htmlclay")

	f1, _ := mgr.Register(path)
	f2, _ := mgr.Register(path)

	if f1.Token != f2.Token {
		t.Error("expected same token for same path")
	}
	if f1 != f2 {
		t.Error("expected same *File pointer")
	}
}

func TestRegisterDifferentPathsDifferentTokens(t *testing.T) {
	mgr, home := setupManager(t)
	p1 := createTestFile(t, home, "a.htmlclay")
	p2 := createTestFile(t, home, "b.htmlclay")

	f1, _ := mgr.Register(p1)
	f2, _ := mgr.Register(p2)

	if f1.Token == f2.Token {
		t.Error("expected different tokens for different paths")
	}
}

func TestRegisterOutsideHomeDir(t *testing.T) {
	mgr, _ := setupManager(t)
	outside := filepath.Join(os.TempDir(), "outside.htmlclay")
	os.WriteFile(outside, []byte("<html></html>"), 0644)
	defer os.Remove(outside)

	_, err := mgr.Register(outside)
	if err == nil {
		t.Error("expected error for path outside home dir")
	}
}

func TestLookupValid(t *testing.T) {
	mgr, home := setupManager(t)
	path := createTestFile(t, home, "test.htmlclay")
	f, _ := mgr.Register(path)

	found, ok := mgr.Lookup(f.Token)
	if !ok {
		t.Fatal("Lookup returned false for valid token")
	}
	if found.AbsPath != path {
		t.Errorf("wrong AbsPath: %q", found.AbsPath)
	}
}

func TestLookupInvalid(t *testing.T) {
	mgr, _ := setupManager(t)
	_, ok := mgr.Lookup("nonexistent-token")
	if ok {
		t.Error("Lookup should return false for invalid token")
	}
}

func TestLookupByPathRegistered(t *testing.T) {
	mgr, home := setupManager(t)
	path := createTestFile(t, home, "test.htmlclay")
	f, _ := mgr.Register(path)

	found, ok := mgr.LookupByPath(path)
	if !ok {
		t.Fatal("LookupByPath returned false for registered path")
	}
	if found.Token != f.Token {
		t.Error("wrong token")
	}
}

func TestLookupByPathUnregistered(t *testing.T) {
	mgr, _ := setupManager(t)
	_, ok := mgr.LookupByPath("/nonexistent")
	if ok {
		t.Error("LookupByPath should return false for unregistered path")
	}
}

func TestRevokeAll(t *testing.T) {
	mgr, home := setupManager(t)
	path := createTestFile(t, home, "test.htmlclay")
	f, _ := mgr.Register(path)

	mgr.RevokeAll()

	_, ok := mgr.Lookup(f.Token)
	if ok {
		t.Error("Lookup should return false after RevokeAll")
	}
}

func TestConcurrentAccess(t *testing.T) {
	mgr, home := setupManager(t)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		name := filepath.Join(home, "file"+string(rune('A'+i%26))+".htmlclay")
		os.WriteFile(name, []byte("<html></html>"), 0644)

		wg.Add(2)
		go func(p string) {
			defer wg.Done()
			mgr.Register(p)
		}(name)
		go func(p string) {
			defer wg.Done()
			mgr.LookupByPath(p)
		}(name)
	}
	wg.Wait()
}

func TestContainWithinHome(t *testing.T) {
	sep := string(os.PathSeparator)
	home := filepath.Join(t.TempDir(), "home")

	// A path strictly inside home is accepted and returned unchanged.
	child := filepath.Join(home, "Documents", "f.htmlclay")
	if got, ok := ContainWithinHome(home, child); !ok || got != child {
		t.Errorf("inside: got (%q,%v), want (%q,true)", got, ok, child)
	}

	// The home dir itself is not strictly inside home.
	if _, ok := ContainWithinHome(home, home); ok {
		t.Error("home dir itself should not be reported as inside home")
	}

	// A sibling that only shares the name prefix is rejected (the trailing
	// separator guards against the home+"-evil" class of escape).
	sibling := home + "-evil" + sep + "secret"
	if _, ok := ContainWithinHome(home, sibling); ok {
		t.Errorf("sibling prefix %q should be rejected", sibling)
	}

	// A differently-cased home prefix names the same dir on case-insensitive
	// filesystems (Windows/macOS) and a different one on Linux.
	mixed := filepath.Join(strings.ToUpper(home), "Documents", "f.htmlclay")
	got, ok := ContainWithinHome(home, mixed)
	if caseInsensitiveFS() {
		if !ok || got != child {
			t.Errorf("case-insensitive: got (%q,%v), want (%q,true) with prefix recased to home", got, ok, child)
		}
	} else if ok {
		t.Errorf("case-sensitive: %q must not be inside %q", mixed, home)
	}
}

func TestAssetRoot(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	os.MkdirAll(filepath.Join(home, "site", "img"), 0755)
	os.MkdirAll(filepath.Join(home, "other"), 0755)
	pagePath := filepath.Join(home, "site", "page.html")
	os.WriteFile(pagePath, []byte("<html></html>"), 0644)
	assetPath := filepath.Join(home, "site", "img", "logo.png")
	os.WriteFile(assetPath, []byte("png"), 0644)
	outsidePath := filepath.Join(home, "other", "secret.txt")
	os.WriteFile(outsidePath, []byte("x"), 0644)

	m := NewManagerWithHome(home)
	if _, _, ok := m.AssetRoot(assetPath); ok {
		t.Fatal("no files opened, nothing should be allowed")
	}
	if _, err := m.Register(pagePath); err != nil {
		t.Fatalf("register: %v", err)
	}
	root, rel, ok := m.AssetRoot(assetPath)
	if !ok {
		t.Error("asset under opened file's dir should be allowed")
	}
	if root != filepath.Join(home, "site") || rel != filepath.Join("img", "logo.png") {
		t.Errorf("AssetRoot = %q, %q", root, rel)
	}
	if _, _, ok := m.AssetRoot(outsidePath); ok {
		t.Error("file outside opened dirs should not be allowed")
	}

	m.RevokeAll()
	if _, _, ok := m.AssetRoot(assetPath); ok {
		t.Error("RevokeAll should clear asset roots")
	}
}

func TestHomeDirNeverBecomesAssetRoot(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	pagePath := filepath.Join(home, "loose.html")
	os.WriteFile(pagePath, []byte("<html></html>"), 0644)
	sibling := filepath.Join(home, "secret.txt")
	os.WriteFile(sibling, []byte("secret"), 0644)

	m := NewManagerWithHome(home)
	if _, err := m.Register(pagePath); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, _, ok := m.AssetRoot(sibling); ok {
		t.Error("file opened in home root must not expose home as an asset root")
	}
}

func TestGrantReadRoot(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	os.MkdirAll(filepath.Join(home, "review", "fable"), 0755)
	asset := filepath.Join(home, "review", "redpen.js")
	os.WriteFile(asset, []byte("//"), 0644)

	m := NewManagerWithHome(home)
	if _, _, ok := m.AssetRoot(asset); ok {
		t.Fatal("nothing granted yet")
	}
	if err := m.GrantReadRoot(filepath.Join(home, "review"), false); err != nil {
		t.Fatalf("grant: %v", err)
	}
	root, rel, ok := m.AssetRoot(asset)
	if !ok || root != filepath.Join(home, "review") || rel != "redpen.js" {
		t.Errorf("after grant AssetRoot = %q, %q, %v", root, rel, ok)
	}

	m.RevokeReadRoot(filepath.Join(home, "review"))
	if _, _, ok := m.AssetRoot(asset); ok {
		t.Error("RevokeReadRoot should remove the grant")
	}
}

func TestGrantReadRootRejects(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	os.MkdirAll(filepath.Join(home, ".ssh"), 0755)
	os.MkdirAll(filepath.Join(home, "config", "versions"), 0755)

	m := NewManagerWithHome(home)
	m.SetGuard(func(dir string) bool { return dir == filepath.Join(home, "config", "versions") })

	if err := m.GrantReadRoot(home, false); err == nil {
		t.Error("granting the home directory must be refused")
	}
	if err := m.GrantReadRoot(filepath.Join(home, ".ssh"), false); err == nil {
		t.Error("granting a hidden directory must be refused")
	}
	if err := m.GrantReadRoot(filepath.Join(home, "config", "versions"), false); err == nil {
		t.Error("guard-vetoed directory must be refused")
	}
	if err := m.GrantReadRoot(filepath.Dir(home), false); err == nil {
		t.Error("granting outside home must be refused")
	}
}

func TestAssetRootMostSpecific(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	os.MkdirAll(filepath.Join(home, "site", "img"), 0755)
	page := filepath.Join(home, "site", "page.html")
	os.WriteFile(page, []byte("<html></html>"), 0644)
	asset := filepath.Join(home, "site", "img", "logo.png")
	os.WriteFile(asset, []byte("png"), 0644)

	m := NewManagerWithHome(home)
	if _, err := m.Register(page); err != nil { // opened root = home/site
		t.Fatalf("register: %v", err)
	}
	if err := m.GrantReadRoot(filepath.Join(home, "site", "img"), false); err != nil {
		t.Fatalf("grant: %v", err)
	}
	root, rel, ok := m.AssetRoot(asset)
	if !ok || root != filepath.Join(home, "site", "img") || rel != "logo.png" {
		t.Errorf("most-specific root should win: got %q, %q, %v", root, rel, ok)
	}
}

// There are exactly two per-file records. lastServerWrite is set by save,
// restore, htmlclayid injection, and the first observation of a file, and never
// by serving a page or by the watcher.
func TestRecordServerWriteAdvancesBoth(t *testing.T) {
	f := &File{}

	f.Lock()
	defer f.Unlock()

	if f.Observed() {
		t.Fatal("a fresh file reports itself observed")
	}
	if f.LastServerWrite() != "" || f.LastStableObservation() != "" {
		t.Fatal("a fresh file has non-empty records")
	}

	f.RecordServerWrite("aaa")
	if f.LastServerWrite() != "aaa" || f.LastStableObservation() != "aaa" {
		t.Fatal("RecordServerWrite did not advance both records")
	}
	if !f.Observed() {
		t.Fatal("RecordServerWrite did not mark the file observed")
	}
}

// The watcher advances only lastStableObservation, so an external change never
// masquerades as a write this server performed.
func TestRecordStableObservationLeavesServerWriteAlone(t *testing.T) {
	f := &File{}

	f.Lock()
	defer f.Unlock()

	f.RecordServerWrite("aaa")
	f.RecordStableObservation("bbb")

	if f.LastServerWrite() != "aaa" {
		t.Fatalf("lastServerWrite = %q, want it untouched", f.LastServerWrite())
	}
	if f.LastStableObservation() != "bbb" {
		t.Fatalf("lastStableObservation = %q", f.LastStableObservation())
	}
}

// The first observation seeds both records, so the first save of a file this
// server has never written is not a false-positive stale write. It happens once.
func TestNoteFirstObservationHappensOnce(t *testing.T) {
	f := &File{}

	f.Lock()
	defer f.Unlock()

	if !f.NoteFirstObservation("aaa") {
		t.Fatal("the first observation was not reported as first")
	}
	if f.LastServerWrite() != "aaa" || f.LastStableObservation() != "aaa" {
		t.Fatal("the first observation did not seed both records")
	}

	if f.NoteFirstObservation("bbb") {
		t.Fatal("a later observation was reported as first")
	}
	if f.LastServerWrite() != "aaa" || f.LastStableObservation() != "aaa" {
		t.Fatal("a later observation overwrote the seeded records")
	}
}

func TestRecordsAreGuardedByTheFileLock(t *testing.T) {
	f := &File{}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			f.Lock()
			defer f.Unlock()
			if i%2 == 0 {
				f.RecordServerWrite("write")
			} else {
				f.RecordStableObservation("observe")
			}
			_ = f.LastServerWrite()
			_ = f.LastStableObservation()
			_ = f.Observed()
		}(i)
	}
	wg.Wait()

	f.Lock()
	defer f.Unlock()
	if f.LastServerWrite() != "write" {
		t.Fatalf("lastServerWrite = %q", f.LastServerWrite())
	}
}

// Blocker 4a. observed is derived from lastServerWrite rather than stored, so the
// watcher is structurally unable to mark a file observed. As a stored flag it was
// a third per-file record, and an origin-trusted SSE subscription naming a
// never-served file let the watcher set it: that file's first real GET then
// skipped both clone resolution and its opening snapshot.
func TestWatcherObservationDoesNotMarkAFileObserved(t *testing.T) {
	f := &File{}

	f.Lock()
	defer f.Unlock()

	f.RecordStableObservation("watcher-saw-this")

	if f.Observed() {
		t.Fatal("a watcher observation marked the file observed, which suppresses " +
			"clone resolution and the first-open snapshot on the first real GET")
	}
	if !f.NoteFirstObservation("first-real-read") {
		t.Fatal("the first real read was not reported as the first observation")
	}
	if f.LastServerWrite() != "first-real-read" {
		t.Fatalf("lastServerWrite = %q", f.LastServerWrite())
	}
}

// The history key is resolved once and then immovable.
func TestHistoryKeyIsResolvedOnce(t *testing.T) {
	f := &File{}

	f.Lock()
	defer f.Unlock()

	if f.HistoryKey() != "" {
		t.Fatal("a fresh file already has a history key")
	}
	f.SetHistoryKey("id:first")
	f.SetHistoryKey("path:second")
	if f.HistoryKey() != "id:first" {
		t.Fatalf("history key moved to %q; it must never be re-derived", f.HistoryKey())
	}
}
