package versions

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validUUID = "3f2a1b4c-5d6e-4f70-8a9b-0c1d2e3f4a5b"

func newStore(t *testing.T) *Store {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return New(filepath.Join(dir, "versions"))
}

func doc(body string) []byte {
	return []byte("<!DOCTYPE html>\n<html><body>" + body + "</body></html>")
}

func docWithID(id, body string) []byte {
	return []byte("<!DOCTYPE html>\n<html htmlclayid=\"" + id + "\"><body>" + body + "</body></html>")
}

func TestIsCanonicalUUID(t *testing.T) {
	cases := map[string]bool{
		validUUID:                               true,
		strings.ToUpper(validUUID):              true,
		"":                                      false,
		"..":                                    false,
		"short":                                 false,
		"3f2a1b4c5d6e4f708a9b0c1d2e3f4a5b":      false,
		"3f2a1b4c-5d6e-4f70-8a9b-0c1d2e3f4a5":   false,
		"3f2a1b4c-5d6e-4f70-8a9b-0c1d2e3f4a5bb": false,
		"../../etc/passwd":                      false,
	}
	for id, want := range cases {
		if got := IsCanonicalUUID(id); got != want {
			t.Errorf("IsCanonicalUUID(%q) = %v, want %v", id, got, want)
		}
	}
}

// A .htmlclay file keys by its id only when that id is a canonical UUID. A
// hand-edited file carrying `..` or a short string must fall back to the
// path-derived key rather than being trusted.
func TestKeyMalformedIDFallsBackToPath(t *testing.T) {
	path := "/home/u/notes.htmlclay"
	for _, bad := range []string{"..", "x", "", "../../escape"} {
		key := Key(path, docWithID(bad, "hi"))
		if !strings.HasPrefix(key, "path:") {
			t.Errorf("id %q produced key %q, want a path: key", bad, key)
		}
		if strings.Contains(key, bad) && bad != "" {
			t.Errorf("key %q leaked the raw id %q", key, bad)
		}
	}
}

func TestKeyValidIDUsesID(t *testing.T) {
	key := Key("/home/u/notes.htmlclay", docWithID(validUUID, "hi"))
	if key != "id:"+validUUID {
		t.Fatalf("got %q, want id:%s", key, validUUID)
	}
	// The same id in a different location is the same history.
	other := Key("/home/u/elsewhere/notes.htmlclay", docWithID(validUUID, "hi"))
	if other != key {
		t.Fatalf("id key changed with path: %q vs %q", other, key)
	}
}

// A plain .html file never receives an injected id, so it keys by a hash of its
// absolute path even when the bytes happen to carry an htmlclayid.
func TestKeyPlainHTMLAlwaysUsesPath(t *testing.T) {
	withID := Key("/home/u/page.html", docWithID(validUUID, "hi"))
	withoutID := Key("/home/u/page.html", doc("hi"))
	if withID != withoutID {
		t.Fatalf("plain .html key depends on id: %q vs %q", withID, withoutID)
	}
	if !strings.HasPrefix(withID, "path:") {
		t.Fatalf("got %q, want a path: key", withID)
	}
	if same := Key("/home/u/other.html", doc("hi")); same == withID {
		t.Fatal("two different paths produced the same key")
	}
}

// The eight hex characters in a folder name are a display affordance only. Two
// distinct keys that share a display prefix must not share a history.
func TestDistinctKeysNeverShareHistory(t *testing.T) {
	s := newStore(t)
	keyA := Key("/home/u/notes.htmlclay", docWithID(validUUID, "a"))
	keyB := Key("/home/u/notes.htmlclay", doc("b"))

	if _, err := s.Backup(keyA, "/home/u/notes.htmlclay", doc("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Backup(keyB, "/home/u/notes.htmlclay", doc("b")); err != nil {
		t.Fatal(err)
	}

	a, err := s.List(keyA, "/home/u/notes.htmlclay")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.List(keyB, "/home/u/notes.htmlclay")
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("expected one version each, got %d and %d", len(a), len(b))
	}
}

func TestBackupDedupesIdenticalContent(t *testing.T) {
	s := newStore(t)
	key := Key("/home/u/a.html", nil)

	created, err := s.Backup(key, "/home/u/a.html", doc("one"))
	if err != nil || !created {
		t.Fatalf("first backup: created=%v err=%v", created, err)
	}
	created, err = s.Backup(key, "/home/u/a.html", doc("one"))
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("identical content was versioned twice")
	}
	entries, _ := s.List(key, "/home/u/a.html")
	if len(entries) != 1 {
		t.Fatalf("expected 1 version, got %d", len(entries))
	}
}

func TestEntryNameRoundTripAndZeroPadding(t *testing.T) {
	when := time.Date(2026, 7, 19, 14, 22, 8, 431*int(time.Millisecond), time.UTC)

	if got := entryName(when, 0); got != "2026-07-19-14-22-08-431Z.html" {
		t.Fatalf("got %q", got)
	}
	if got := entryName(when, 2); got != "2026-07-19-14-22-08-431Z-02.html" {
		t.Fatalf("got %q", got)
	}
	if got := entryName(when, 10); got != "2026-07-19-14-22-08-431Z-10.html" {
		t.Fatalf("got %q", got)
	}

	// Zero padding is what keeps -02 ordering before -10 by parsed sequence.
	_, seq2, err := ParseEntryName(entryName(when, 2))
	if err != nil {
		t.Fatal(err)
	}
	parsedTime, seq10, err := ParseEntryName(entryName(when, 10))
	if err != nil {
		t.Fatal(err)
	}
	if seq2 >= seq10 {
		t.Fatalf("seq ordering wrong: %d vs %d", seq2, seq10)
	}
	if !parsedTime.Equal(when) {
		t.Fatalf("timestamp round trip failed: %v vs %v", parsedTime, when)
	}
	if parsedTime.Location() != time.UTC {
		t.Fatal("parsed timestamp is not UTC")
	}
}

func TestParseEntryNameRejectsAnythingElse(t *testing.T) {
	bad := []string{
		"meta.json",
		"../../etc/passwd",
		"2026-07-19-14-22-08-431Z.html.bak",
		"2026-7-19-14-22-08-431Z.html",
		"2026-07-19-14-22-08-431.html",
		"2026-07-19-14-22-08-431Z-1.html",
		"",
		".",
		"2026-07-19-14-22-08-431Z/../x.html",
	}
	for _, name := range bad {
		if _, _, err := ParseEntryName(name); err == nil {
			t.Errorf("ParseEntryName(%q) accepted an invalid name", name)
		}
	}
}

// A tight loop lands several backups inside one millisecond, which is exactly the
// timestamp-collision path. Every name must be distinct, parse, and sort strictly
// after the one before it.
func TestTimestampCollisionsStayOrdered(t *testing.T) {
	s := newStore(t)
	path := "/home/u/burst.html"
	key := Key(path, nil)

	const n = 40
	for i := 0; i < n; i++ {
		if _, err := s.Backup(key, path, doc(fmt.Sprintf("v%d", i))); err != nil {
			t.Fatalf("backup %d: %v", i, err)
		}
	}

	entries, err := s.List(key, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != n {
		t.Fatalf("expected %d versions, got %d", n, len(entries))
	}

	seen := make(map[string]bool, n)
	// List returns newest first; walk backwards for ascending order.
	for i := len(entries) - 1; i > 0; i-- {
		older, newer := entries[i], entries[i-1]
		if seen[older.Name] {
			t.Fatalf("duplicate version name %q", older.Name)
		}
		seen[older.Name] = true
		if !older.before(newer) {
			t.Fatalf("ordering broken: %q not before %q", older.Name, newer.Name)
		}
	}
}

// UTC removes DST repetition but not a system clock rollback. When the clock
// would produce a name at or before the newest existing entry, the new version is
// appended after that entry instead of being trusted.
func TestClockRollbackAppendsAfterNewest(t *testing.T) {
	s := newStore(t)
	path := "/home/u/rollback.html"
	key := Key(path, nil)

	if _, err := s.Backup(key, path, doc("first")); err != nil {
		t.Fatal(err)
	}

	// Stand in for a clock that has since been rolled back: plant an entry dated
	// far in the future, so "now" is behind the newest entry.
	dir, err := s.historyDir(key, path, true)
	if err != nil {
		t.Fatal(err)
	}
	future := time.Now().UTC().Add(72 * time.Hour)
	futureName := entryName(future, 0)
	if err := os.WriteFile(filepath.Join(dir, futureName), doc("future"), 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Backup(key, path, doc("after rollback")); err != nil {
		t.Fatal(err)
	}

	entries, err := s.List(key, path)
	if err != nil {
		t.Fatal(err)
	}
	if entries[0].Name == futureName {
		t.Fatal("post-rollback version sorted before the newest existing entry")
	}
	got, err := s.Read(key, path, entries[0].Name)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(doc("after rollback")) {
		t.Fatalf("newest version is not the one just written: %q", got)
	}
}

// A renamed file keeps its single history rather than growing a second folder,
// because lookup is by key first.
func TestRenameKeepsOneHistory(t *testing.T) {
	s := newStore(t)
	key := "id:" + validUUID

	if _, err := s.Backup(key, "/home/u/before.htmlclay", doc("one")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Backup(key, "/home/u/after.htmlclay", doc("two")); err != nil {
		t.Fatal(err)
	}

	base, err := s.Dir()
	if err != nil {
		t.Fatal(err)
	}
	dirents, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	folders := 0
	var name string
	for _, d := range dirents {
		if d.IsDir() {
			folders++
			name = d.Name()
		}
	}
	if folders != 1 {
		t.Fatalf("expected 1 history folder after rename, got %d", folders)
	}
	if !strings.HasPrefix(name, "after-") {
		t.Fatalf("folder %q was not renamed to follow the file", name)
	}

	entries, _ := s.List(key, "/home/u/after.htmlclay")
	if len(entries) != 2 {
		t.Fatalf("expected 2 versions in the single history, got %d", len(entries))
	}
}

// Regression: the eight hex display characters can themselves be all digits, and
// a rule that strips a trailing -<digits> would read that as a collision suffix
// and rename the history folder on every single lookup.
func TestFolderMatchesBase(t *testing.T) {
	cases := []struct {
		folder, base string
		want         bool
	}{
		{"notes-a3f19c2b", "notes-a3f19c2b", true},
		{"notes-a3f19c2b-2", "notes-a3f19c2b", true},
		{"notes-12345678", "notes-12345678", true},
		{"notes-12345678-3", "notes-12345678", true},
		{"other-a3f19c2b", "notes-a3f19c2b", false},
		{"notes-a3f19c2b", "notes-99999999", false},
		{"notes-a3f19c2b-x", "notes-a3f19c2b", false},
		{"notes-a3f19c2b-", "notes-a3f19c2b", false},
	}
	for _, c := range cases {
		if got := folderMatchesBase(c.folder, c.base); got != c.want {
			t.Errorf("folderMatchesBase(%q, %q) = %v, want %v", c.folder, c.base, got, c.want)
		}
	}
}

func TestLookupDoesNotRenameStableFolder(t *testing.T) {
	s := newStore(t)
	path := "/home/u/big.html"
	key := Key(path, nil)

	dir, err := s.historyDir(key, path, true)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, err := s.historyDir(key, path, false)
		if err != nil {
			t.Fatal(err)
		}
		if again != dir {
			t.Fatalf("history folder moved on lookup %d: %q -> %q", i, dir, again)
		}
	}
}

func TestBoundPathAndRebind(t *testing.T) {
	s := newStore(t)
	key := "id:" + validUUID

	if _, ok := s.BoundPath(key); ok {
		t.Fatal("unknown key reported a bound path")
	}
	if _, err := s.Backup(key, "/home/u/one.htmlclay", doc("x")); err != nil {
		t.Fatal(err)
	}
	if p, ok := s.BoundPath(key); !ok || p != "/home/u/one.htmlclay" {
		t.Fatalf("BoundPath = %q, %v", p, ok)
	}
	if err := s.Rebind(key, "/home/u/two.htmlclay"); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.BoundPath(key); p != "/home/u/two.htmlclay" {
		t.Fatalf("Rebind did not take effect, got %q", p)
	}
}

func TestReadRejectsNonGeneratedNames(t *testing.T) {
	s := newStore(t)
	path := "/home/u/a.html"
	key := Key(path, nil)
	if _, err := s.Backup(key, path, doc("x")); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"meta.json", "../meta.json", "../../../etc/passwd", "x.html"} {
		if _, err := s.Read(key, path, name); err == nil {
			t.Errorf("Read accepted %q", name)
		}
	}
}

func TestReadRejectsSymlinkedVersion(t *testing.T) {
	s := newStore(t)
	path := "/home/u/a.html"
	key := Key(path, nil)
	if _, err := s.Backup(key, path, doc("x")); err != nil {
		t.Fatal(err)
	}
	dir, err := s.historyDir(key, path, true)
	if err != nil {
		t.Fatal(err)
	}

	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("classified"), 0600); err != nil {
		t.Fatal(err)
	}
	link := entryName(time.Now().UTC().Add(time.Hour), 0)
	if err := os.Symlink(secret, filepath.Join(dir, link)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if data, err := s.Read(key, path, link); err == nil {
		t.Fatalf("Read followed a symlink out of the history: %q", data)
	}
}

func TestPruneKeepsNewestAndRecentUnion(t *testing.T) {
	s := newStore(t)
	path := "/home/u/big.html"
	key := Key(path, nil)
	dir, err := s.historyDir(key, path, true)
	if err != nil {
		t.Fatal(err)
	}

	// 30 ancient versions plus 3 recent ones.
	ancient := time.Now().UTC().Add(-90 * 24 * time.Hour)
	var ancientNames []string
	for i := 0; i < 30; i++ {
		name := entryName(ancient.Add(time.Duration(i)*time.Minute), 0)
		ancientNames = append(ancientNames, name)
		if err := os.WriteFile(filepath.Join(dir, name), doc(fmt.Sprintf("old%d", i)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	recent := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 3; i++ {
		name := entryName(recent.Add(time.Duration(i)*time.Minute), 0)
		if err := os.WriteFile(filepath.Join(dir, name), doc(fmt.Sprintf("new%d", i)), 0600); err != nil {
			t.Fatal(err)
		}
	}

	pruneDir(dir)

	entries, err := s.List(key, path)
	if err != nil {
		t.Fatal(err)
	}
	// Union: the 3 recent ones plus enough ancient ones to reach the 20 floor.
	if len(entries) != MinKeep {
		t.Fatalf("expected %d versions retained, got %d", MinKeep, len(entries))
	}
	// The oldest ancient versions are the ones that went.
	if _, err := os.Lstat(filepath.Join(dir, ancientNames[0])); err == nil {
		t.Fatal("oldest ancient version survived pruning")
	}
	// Every recent version survived.
	for i := 0; i < 3; i++ {
		name := entryName(recent.Add(time.Duration(i)*time.Minute), 0)
		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("recent version %q was pruned", name)
		}
	}
}

func TestPruneNeverDeletesWhenUnderFloor(t *testing.T) {
	s := newStore(t)
	path := "/home/u/small.html"
	key := Key(path, nil)
	dir, err := s.historyDir(key, path, true)
	if err != nil {
		t.Fatal(err)
	}
	ancient := time.Now().UTC().Add(-365 * 24 * time.Hour)
	for i := 0; i < 5; i++ {
		name := entryName(ancient.Add(time.Duration(i)*time.Minute), 0)
		if err := os.WriteFile(filepath.Join(dir, name), doc(fmt.Sprintf("v%d", i)), 0600); err != nil {
			t.Fatal(err)
		}
	}

	pruneDir(dir)

	entries, _ := s.List(key, path)
	if len(entries) != 5 {
		t.Fatalf("pruning deleted below the %d-version floor: %d left", MinKeep, len(entries))
	}
}

func TestHasHistory(t *testing.T) {
	s := newStore(t)
	path := "/home/u/a.html"
	key := Key(path, nil)
	if s.HasHistory(key, path) {
		t.Fatal("empty store reported a history")
	}
	if _, err := s.Backup(key, path, doc("x")); err != nil {
		t.Fatal(err)
	}
	if !s.HasHistory(key, path) {
		t.Fatal("store did not report the history it just created")
	}
}

func TestContainsRejectsOutsidePaths(t *testing.T) {
	s := newStore(t)
	base, err := s.Dir()
	if err != nil {
		t.Fatal(err)
	}
	if !s.Contains(filepath.Join(base, "notes-abcd1234", "x.html")) {
		t.Fatal("a path inside the versions dir was not recognized")
	}
	if s.Contains(base) {
		t.Fatal("the versions dir itself is not strictly inside itself")
	}
	if s.Contains(filepath.Join(filepath.Dir(base), "other", "x.html")) {
		t.Fatal("a path outside the versions dir was recognized as inside")
	}
	if s.Contains(filepath.Join(base, "..", "escape.html")) {
		t.Fatal("a traversal escaped the containment check")
	}
}

func TestStoreSurvivesReload(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(dir, "versions")
	path := "/home/u/notes.htmlclay"
	key := "id:" + validUUID

	first := New(base)
	if _, err := first.Backup(key, path, doc("one")); err != nil {
		t.Fatal(err)
	}

	// A fresh process rebuilds the key index from meta.json on disk.
	second := New(base)
	if p, ok := second.BoundPath(key); !ok || p != path {
		t.Fatalf("index did not survive a reload: %q %v", p, ok)
	}
	if _, err := second.Backup(key, path, doc("two")); err != nil {
		t.Fatal(err)
	}
	entries, _ := second.List(key, path)
	if len(entries) != 2 {
		t.Fatalf("expected 2 versions in one history, got %d", len(entries))
	}
}
