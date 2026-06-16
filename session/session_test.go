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
