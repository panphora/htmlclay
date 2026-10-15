package session

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A restored registration answers to exactly the token it carried, and the
// snapshot that produced it round-trips path, provenance and token.
func TestRestoreRegistrationKeepsTheToken(t *testing.T) {
	old, home := setupManager(t)
	path := createTestFile(t, home, "doc.htmlclay")
	f, err := old.Register(path, ViaOsOpen)
	if err != nil {
		t.Fatal(err)
	}
	snap := old.Snapshot()
	if len(snap.Registrations) != 1 {
		t.Fatalf("snapshot has %d registrations, want 1", len(snap.Registrations))
	}
	r := snap.Registrations[0]
	if r.Path != path || r.Via != ViaOsOpen || r.Token != f.Token {
		t.Fatalf("snapshot = %+v, want path %s via %d token %s", r, path, ViaOsOpen, f.Token)
	}

	fresh := newTestManager(t, home)
	got, err := fresh.RestoreRegistration(r.Path, r.Via, r.Token)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got.Token != f.Token {
		t.Fatalf("restored token %q, want %q", got.Token, f.Token)
	}
	if looked, ok := fresh.Lookup(f.Token); !ok || looked.AbsPath != path {
		t.Fatal("the restored token does not look up the file")
	}
	if _, _, ok := fresh.AssetRoot(filepath.Join(home, "sibling.png")); ok {
		t.Fatal("a file loose in home must not install home as a read root")
	}
}

// A ViaOsOpen restore reinstalls the opened root, and a granted root comes back
// through the snapshot, so a page's sibling assets and approved folders still
// read after the restart.
func TestSnapshotCarriesGrantedRoots(t *testing.T) {
	old, home := setupManager(t)
	dir := filepath.Join(home, "proj")
	assets := filepath.Join(home, "assets")
	for _, d := range []string{dir, assets} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	path := createTestFile(t, dir, "doc.htmlclay")
	if _, err := old.Register(path, ViaOsOpen); err != nil {
		t.Fatal(err)
	}
	if err := old.GrantCanonicalRoot(assets); err != nil {
		t.Fatal(err)
	}
	snap := old.Snapshot()
	if len(snap.GrantedRoots) != 1 || snap.GrantedRoots[0] != assets {
		t.Fatalf("granted roots = %v, want [%s]", snap.GrantedRoots, assets)
	}

	fresh := newTestManager(t, home)
	r := snap.Registrations[0]
	if _, err := fresh.RestoreRegistration(r.Path, r.Via, r.Token); err != nil {
		t.Fatal(err)
	}
	if _, _, opened, ok := fresh.AssetRootOpened(filepath.Join(dir, "img.png")); !ok || !opened {
		t.Fatal("restoring an opened file must reinstall its opened root")
	}
}

func TestRestoreRegistrationRejectsAMalformedToken(t *testing.T) {
	m, home := setupManager(t)
	path := createTestFile(t, home, "doc.htmlclay")
	for _, tok := range []string{
		"",
		"short",
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA+",
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAB",
	} {
		if _, err := m.RestoreRegistration(path, ViaOsOpen, tok); err == nil {
			t.Errorf("token %q was accepted", tok)
		}
	}
	if len(m.Registrations()) != 0 {
		t.Fatal("a refused restore left a registration behind")
	}
}

func TestRestoreRegistrationRejectsADuplicateToken(t *testing.T) {
	m, home := setupManager(t)
	first := createTestFile(t, home, "first.htmlclay")
	second := createTestFile(t, home, "second.htmlclay")
	f, err := m.Register(first, ViaOsOpen)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.RestoreRegistration(second, ViaOsOpen, f.Token); err == nil {
		t.Fatal("a token already in use was installed for a second file")
	}
	if _, ok := m.LookupByPath(second); ok {
		t.Fatal("the refused file was registered anyway")
	}

	other, err := generateToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.RestoreRegistration(first, ViaOsOpen, other); err == nil {
		t.Fatal("a registered file accepted a second token")
	}
	if again, err := m.RestoreRegistration(first, ViaOsOpen, f.Token); err != nil || again != f {
		t.Fatalf("restoring the token a file already holds should be a no-op, got %v", err)
	}
}

func TestRestoreRegistrationRejectsAHiddenPath(t *testing.T) {
	m, home := setupManager(t)
	hidden := filepath.Join(home, ".secret")
	if err := os.MkdirAll(hidden, 0755); err != nil {
		t.Fatal(err)
	}
	path := createTestFile(t, hidden, "doc.htmlclay")
	tok, _ := generateToken()
	if _, err := m.RestoreRegistration(path, ViaOsOpen, tok); err == nil {
		t.Fatal("a file under a hidden folder was restored")
	}
}

func TestRestoreRegistrationRejectsAMissingFileAndAnInvalidProvenance(t *testing.T) {
	m, home := setupManager(t)
	tok, _ := generateToken()
	if _, err := m.RestoreRegistration(filepath.Join(home, "gone.htmlclay"), ViaOsOpen, tok); err == nil {
		t.Fatal("a missing file was restored")
	}
	path := createTestFile(t, home, "doc.htmlclay")
	for _, via := range []Provenance{0, 1 << 5} {
		if _, err := m.RestoreRegistration(path, via, tok); err == nil {
			t.Errorf("provenance %d was accepted", via)
		}
	}
	if err := os.MkdirAll(filepath.Join(home, "dir.htmlclay"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RestoreRegistration(filepath.Join(home, "dir.htmlclay"), ViaOsOpen, tok); err == nil {
		t.Fatal("a directory was restored as a file")
	}
}

// A path that resolves somewhere else now (a component swapped for a symlink
// while the app was down) is refused, not followed.
func TestRestoreRegistrationRejectsAPathThatNowResolvesElsewhere(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	m, home := setupManager(t)
	real := filepath.Join(home, "real")
	if err := os.MkdirAll(real, 0755); err != nil {
		t.Fatal(err)
	}
	createTestFile(t, real, "doc.htmlclay")
	link := filepath.Join(home, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	tok, _ := generateToken()
	if _, err := m.RestoreRegistration(filepath.Join(link, "doc.htmlclay"), ViaOsOpen, tok); err == nil {
		t.Fatal("a path through a symlink was restored")
	}
}

func TestRestoreRegistrationRefusedAfterRevokeAll(t *testing.T) {
	m, home := setupManager(t)
	path := createTestFile(t, home, "doc.htmlclay")
	m.RevokeAll()
	tok, _ := generateToken()
	if _, err := m.RestoreRegistration(path, ViaOsOpen, tok); err == nil {
		t.Fatal("a revoked manager restored a registration")
	}
	if snap := m.Snapshot(); len(snap.Registrations) != 0 {
		t.Fatal("a revoked manager returned registrations")
	}
}
