package session

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// newTestManager builds a Manager whose held read-root handles are closed when the
// test ends. Windows refuses to remove a directory while a handle to it is open, so
// a Manager left holding its os.Root fails the t.TempDir cleanup and marks the test
// failed even though every assertion passed. POSIX unlink hides this everywhere else,
// which is why it only ever surfaced on the Windows CI runner.
//
// Construct managers in tests through this, not NewManagerWithHome.
func newTestManager(t *testing.T, home string) *Manager {
	t.Helper()
	m := NewManagerWithHome(home)
	t.Cleanup(m.RevokeAll)
	return m
}

func setupManager(t *testing.T) (*Manager, string) {
	t.Helper()
	homeDir := t.TempDir()
	// On macOS, t.TempDir() returns /var/... which is a symlink to /private/var/...
	// EvalSymlinks in Register resolves to /private/var/..., so homeDir must match.
	resolved, err := filepath.EvalSymlinks(homeDir)
	if err != nil {
		t.Fatal(err)
	}
	mgr := newTestManager(t, resolved)
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

	f, err := mgr.Register(path, ViaOsOpen)
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

	f1, _ := mgr.Register(path, ViaOsOpen)
	f2, _ := mgr.Register(path, ViaOsOpen)

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

	f1, _ := mgr.Register(p1, ViaOsOpen)
	f2, _ := mgr.Register(p2, ViaOsOpen)

	if f1.Token == f2.Token {
		t.Error("expected different tokens for different paths")
	}
}

func TestRegisterOutsideHomeDir(t *testing.T) {
	mgr, _ := setupManager(t)
	outside := filepath.Join(os.TempDir(), "outside.htmlclay")
	os.WriteFile(outside, []byte("<html></html>"), 0644)
	defer os.Remove(outside)

	_, err := mgr.Register(outside, ViaOsOpen)
	if err == nil {
		t.Error("expected error for path outside home dir")
	}
}

func TestLookupValid(t *testing.T) {
	mgr, home := setupManager(t)
	path := createTestFile(t, home, "test.htmlclay")
	f, _ := mgr.Register(path, ViaOsOpen)

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
	f, _ := mgr.Register(path, ViaOsOpen)

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
	f, _ := mgr.Register(path, ViaOsOpen)

	mgr.RevokeAll()

	_, ok := mgr.Lookup(f.Token)
	if ok {
		t.Error("Lookup should return false after RevokeAll")
	}
}

// Concurrent registrations and lookups of the same paths. Run under -race.
//
// The version that shipped first spawned 200 goroutines, discarded every return
// value, and asserted nothing, so it passed on a Manager that returned an error
// every time. What the overlap is actually worth checking is that Register is
// idempotent per path: the loop reuses 26 names across 100 iterations, so four
// goroutines register each path at once, and they must all end up with the one
// file rather than each minting its own token.
func TestConcurrentAccess(t *testing.T) {
	mgr, home := setupManager(t)

	const iters = 100
	paths := make([]string, iters)
	for i := 0; i < iters; i++ {
		paths[i] = filepath.Join(home, "file"+string(rune('A'+i%26))+".htmlclay")
		if err := os.WriteFile(paths[i], []byte("<html></html>"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// A path registered before the barrier, so the lookup half has something it must
	// ALWAYS find. Pointing every lookup at a path being registered concurrently
	// makes a miss legitimate, and a LookupByPath that returned nothing forever
	// would then satisfy the test.
	stable := filepath.Join(home, "stable.htmlclay")
	if err := os.WriteFile(stable, []byte("<html></html>"), 0644); err != nil {
		t.Fatal(err)
	}
	stableFile, err := mgr.Register(stable, ViaOsOpen)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	tokens := make(map[string]map[string]bool) // path -> tokens seen
	var registered, lookups, mismatched, stableMisses int
	var firstErr error

	// A start barrier, so the goroutines contend instead of trickling out behind
	// however long the spawn loop took.
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, name := range paths {
		wg.Add(2)
		go func(p string) {
			defer wg.Done()
			<-start
			f, err := mgr.Register(p, ViaOsOpen)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			registered++
			if tokens[p] == nil {
				tokens[p] = map[string]bool{}
			}
			tokens[p][f.Token] = true
		}(name)
		go func(p string) {
			defer wg.Done()
			<-start
			f, ok := mgr.LookupByPath(p)
			sf, stableOK := mgr.LookupByPath(stable)
			mu.Lock()
			defer mu.Unlock()
			lookups++
			// A miss on p is legitimate: the lookup may run before that path's
			// registration. A hit for a different file never is.
			if ok && f.AbsPath != p {
				mismatched++
			}
			// A miss on the stable path never is. It was registered before the
			// barrier, and concurrent registrations of other files must not be able
			// to hide it.
			if !stableOK || sf.Token != stableFile.Token {
				stableMisses++
			}
		}(name)
	}
	close(start)
	wg.Wait()

	if firstErr != nil {
		t.Fatalf("concurrent Register failed: %v", firstErr)
	}
	if registered != iters || lookups != iters {
		t.Fatalf("registered=%d lookups=%d, want %d of each", registered, lookups, iters)
	}
	if mismatched != 0 {
		t.Errorf("%d lookups returned a file registered under a different path", mismatched)
	}
	if stableMisses != 0 {
		t.Errorf("%d of %d lookups failed to find a file registered before the barrier", stableMisses, iters)
	}
	for p, seen := range tokens {
		if len(seen) != 1 {
			t.Errorf("%s was registered as %d distinct files; concurrent Register of one path must be idempotent", filepath.Base(p), len(seen))
		}
	}
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

	m := newTestManager(t, home)
	if _, _, ok := m.AssetRoot(assetPath); ok {
		t.Fatal("no files opened, nothing should be allowed")
	}
	if _, err := m.Register(pagePath, ViaOsOpen); err != nil {
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

	m := newTestManager(t, home)
	if _, err := m.Register(pagePath, ViaOsOpen); err != nil {
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

	m := newTestManager(t, home)
	if _, _, ok := m.AssetRoot(asset); ok {
		t.Fatal("nothing granted yet")
	}
	if err := m.GrantReadRoot(filepath.Join(home, "review")); err != nil {
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

	m := newTestManager(t, home)
	m.SetGuard(func(dir string) bool { return dir == filepath.Join(home, "config", "versions") })

	if err := m.GrantReadRoot(home); err == nil {
		t.Error("granting the home directory must be refused")
	}
	if err := m.GrantReadRoot(filepath.Join(home, ".ssh")); err == nil {
		t.Error("granting a hidden directory must be refused")
	}
	if err := m.GrantReadRoot(filepath.Join(home, "config", "versions")); err == nil {
		t.Error("guard-vetoed directory must be refused")
	}
	if err := m.GrantReadRoot(filepath.Dir(home)); err == nil {
		t.Error("granting outside home must be refused")
	}
}

// Revoking a grant must not take away the capability an explicit open created.
// Collapsing both into one "kind" field meant revoke deleted the whole entry and
// the opened page's own siblings started 404ing.
func TestRevokeGrantKeepsOpenedCapability(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	os.MkdirAll(filepath.Join(home, "site"), 0755)
	page := filepath.Join(home, "site", "page.html")
	os.WriteFile(page, []byte("<html></html>"), 0644)
	asset := filepath.Join(home, "site", "style.css")
	os.WriteFile(asset, []byte("body{}"), 0644)

	m := newTestManager(t, home)
	if _, err := m.Register(page, ViaOsOpen); err != nil {
		t.Fatalf("register: %v", err)
	}
	dir := filepath.Join(home, "site")

	if err := m.GrantReadRoot(dir); err != nil {
		t.Fatalf("grant: %v", err)
	}
	m.RevokeReadRoot(dir)

	if _, _, ok := m.AssetRoot(asset); !ok {
		t.Error("revoking a grant must not remove the opened file's own read root")
	}

	// A root that exists only because of a grant does disappear on revoke.
	other := filepath.Join(home, "other")
	os.MkdirAll(other, 0755)
	if err := m.GrantReadRoot(other); err != nil {
		t.Fatalf("grant other: %v", err)
	}
	m.RevokeReadRoot(other)
	if _, _, ok := m.AssetRoot(filepath.Join(other, "x.txt")); ok {
		t.Error("revoking a grant-only root must remove it")
	}
}

// A trusted root grants the same silent read capability a grant does, but survives
// a grant revoke and only disappears when its own trust is withdrawn.
func TestInstallTrustedRoot(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	os.MkdirAll(filepath.Join(home, "sites", "shared"), 0755)
	asset := filepath.Join(home, "sites", "shared", "lib.js")
	os.WriteFile(asset, []byte("//"), 0644)

	m := newTestManager(t, home)
	dir := filepath.Join(home, "sites")
	if err := m.InstallTrustedRoot(dir); err != nil {
		t.Fatalf("trust: %v", err)
	}
	if _, _, ok := m.AssetRoot(asset); !ok {
		t.Fatal("a trusted root should make its whole tree readable")
	}

	// A grant revoke must not remove a trusted root.
	m.RevokeReadRoot(dir)
	if _, _, ok := m.AssetRoot(asset); !ok {
		t.Error("revoking a grant must never take away a trusted root")
	}

	// Untrusting it does.
	m.RevokeTrustedRoot(dir)
	if _, _, ok := m.AssetRoot(asset); ok {
		t.Error("RevokeTrustedRoot should remove a trust-only root")
	}
}

func TestInstallTrustedRootRejects(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	os.MkdirAll(filepath.Join(home, ".hidden"), 0755)
	os.MkdirAll(filepath.Join(home, "config", "versions"), 0755)

	m := newTestManager(t, home)
	m.SetGuard(func(dir string) bool { return dir == filepath.Join(home, "config", "versions") })

	if err := m.InstallTrustedRoot(home); err == nil {
		t.Error("trusting the home directory must be refused")
	}
	if err := m.InstallTrustedRoot(filepath.Join(home, ".hidden")); err == nil {
		t.Error("trusting a hidden directory must be refused")
	}
	if err := m.InstallTrustedRoot(filepath.Join(home, "config", "versions")); err == nil {
		t.Error("a guard-vetoed directory must be refused")
	}
	if err := m.InstallTrustedRoot(filepath.Dir(home)); err == nil {
		t.Error("trusting outside home must be refused")
	}
}

// A directory that is both trusted and granted keeps its trusted capability when
// the grant is revoked: the provenances are independent flags on one entry.
func TestRevokeGrantKeepsTrustedCapability(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	dir := filepath.Join(home, "sites")
	os.MkdirAll(dir, 0755)
	asset := filepath.Join(dir, "x.js")
	os.WriteFile(asset, []byte("//"), 0644)

	m := newTestManager(t, home)
	if err := m.InstallTrustedRoot(dir); err != nil {
		t.Fatalf("trust: %v", err)
	}
	if err := m.GrantReadRoot(dir); err != nil {
		t.Fatalf("grant: %v", err)
	}
	m.RevokeReadRoot(dir)
	if _, _, ok := m.AssetRoot(asset); !ok {
		t.Error("revoking the grant must leave the trusted capability intact")
	}
}

func TestAssetRootOpenedReportsProvenance(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	os.MkdirAll(filepath.Join(home, "opened"), 0755)
	os.MkdirAll(filepath.Join(home, "granted"), 0755)
	page := filepath.Join(home, "opened", "page.html")
	os.WriteFile(page, []byte("<html></html>"), 0644)

	m := newTestManager(t, home)
	if _, err := m.Register(page, ViaOsOpen); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := m.GrantReadRoot(filepath.Join(home, "granted")); err != nil {
		t.Fatalf("grant: %v", err)
	}

	if _, _, opened, ok := m.AssetRootOpened(filepath.Join(home, "opened", "x.css")); !ok || !opened {
		t.Errorf("opened root should report opened=true (ok=%v opened=%v)", ok, opened)
	}
	if _, _, opened, ok := m.AssetRootOpened(filepath.Join(home, "granted", "x.css")); !ok || opened {
		t.Errorf("grant-only root should report opened=false (ok=%v opened=%v)", ok, opened)
	}
}

// Each installed root carries its provenance independently: an opened root and a
// granted root are readable but NOT trusted (trust is the one write-granting
// kind), and revoking the grant on a trusted-and-granted root leaves the trust
// standing. Collapsing the flags into one kind is what let a grant revoke take
// away a capability an open or a trust had created.
func TestRootProvenanceFlagsAreIndependent(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	os.MkdirAll(filepath.Join(home, "opened"), 0755)
	os.MkdirAll(filepath.Join(home, "granted"), 0755)
	os.MkdirAll(filepath.Join(home, "trusted"), 0755)
	page := filepath.Join(home, "opened", "page.html")
	os.WriteFile(page, []byte("<html></html>"), 0644)

	m := newTestManager(t, home)
	if _, err := m.Register(page, ViaOsOpen); err != nil { // opened root = home/opened
		t.Fatalf("register: %v", err)
	}
	if err := m.GrantReadRoot(filepath.Join(home, "granted")); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if err := m.InstallTrustedRoot(filepath.Join(home, "trusted")); err != nil {
		t.Fatalf("trust: %v", err)
	}

	opened := filepath.Join(home, "opened", "x.css")
	granted := filepath.Join(home, "granted", "x.css")
	trusted := filepath.Join(home, "trusted", "x.css")

	if _, _, isOpened, ok := m.AssetRootOpened(opened); !ok || !isOpened {
		t.Errorf("opened root should report opened=true (ok=%v opened=%v)", ok, isOpened)
	}
	if m.TrustedCovers(opened) {
		t.Error("an opened root must not be trusted: an open grants reads, never durable write scope")
	}

	if _, _, isOpened, ok := m.AssetRootOpened(granted); !ok || isOpened {
		t.Errorf("grant-only root should report opened=false (ok=%v opened=%v)", ok, isOpened)
	}
	if m.TrustedCovers(granted) {
		t.Error("a granted root must not be trusted: a read grant must never become write authority")
	}

	if _, _, isOpened, ok := m.AssetRootOpened(trusted); !ok || isOpened {
		t.Errorf("trusted root should be readable and report opened=false (ok=%v opened=%v)", ok, isOpened)
	}
	if !m.TrustedCovers(trusted) {
		t.Error("a trusted root must report itself trusted")
	}

	// A grant revoke aimed at each root touches only the granted flag.
	m.RevokeReadRoot(filepath.Join(home, "granted"))
	if _, _, ok := m.AssetRoot(granted); ok {
		t.Error("revoking a grant-only root must remove it")
	}
	m.RevokeReadRoot(filepath.Join(home, "opened"))
	if _, _, ok := m.AssetRoot(opened); !ok {
		t.Error("revoking a grant must not remove the opened file's own read root")
	}
	m.RevokeReadRoot(filepath.Join(home, "trusted"))
	if !m.TrustedCovers(trusted) {
		t.Error("revoking a grant must never take away a trusted root")
	}
}

// A path component swapped for a symlink AFTER a read root is installed cannot
// escape the pinned os.Root capability. Reads resolve through the handle opened at
// install time, so following a component that was replaced with a symlink pointing
// outside the root is refused, never served. The swap happens between install and
// read, so the containment is deterministic without any racing goroutine.
func TestReadThroughPinnedRootContainsComponentSwap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require privileges on windows")
	}
	home, _ := filepath.EvalSymlinks(t.TempDir())
	root := filepath.Join(home, "proj")
	sub := filepath.Join(root, "assets")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "ok.txt"), []byte("in-scope"), 0644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(home, "secret")
	if err := os.MkdirAll(outside, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "loot.txt"), []byte("loot"), 0644); err != nil {
		t.Fatal(err)
	}

	m := newTestManager(t, home)
	if err := m.GrantReadRoot(root); err != nil {
		t.Fatalf("grant: %v", err)
	}

	// The pinned root reads in-scope files before the swap, proving it is valid.
	f, authorized, err := m.OpenAsset(filepath.Join(sub, "ok.txt"))
	if !authorized || err != nil {
		t.Fatalf("in-scope read failed before swap: authorized=%v err=%v", authorized, err)
	}
	f.Close()

	// Replace the subdirectory with a symlink that leaves the root.
	if err := os.RemoveAll(sub); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, sub); err != nil {
		t.Fatal(err)
	}

	// A read through the swapped component must not escape: the pinned handle
	// refuses to follow a symlink that leaves the root.
	escaped, _, err := m.OpenAsset(filepath.Join(sub, "loot.txt"))
	if escaped != nil {
		data, _ := io.ReadAll(escaped)
		escaped.Close()
		if strings.Contains(string(data), "loot") {
			t.Fatal("the pinned root escaped through a swapped symlink component")
		}
	}
	if err == nil {
		t.Fatal("expected the pinned root to refuse the swapped-out component")
	}
}

// The pinned root capability is bound to the directory it opened, not to that
// directory's name. Replacing the whole root path with a symlink to a different
// tree after the handle is open must not redirect reads through it: this is the
// root-level companion to the component-swap test above, and it fails if the
// capability were resolved by path per request rather than held as an inode handle.
func TestReadThroughPinnedRootSurvivesRootReplacedBySymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require privileges on windows")
	}
	home, _ := filepath.EvalSymlinks(t.TempDir())
	root := filepath.Join(home, "proj")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ok.txt"), []byte("in-scope"), 0644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(home, "evil")
	if err := os.MkdirAll(outside, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "ok.txt"), []byte("loot"), 0644); err != nil {
		t.Fatal(err)
	}

	m := newTestManager(t, home)
	if err := m.GrantReadRoot(root); err != nil {
		t.Fatalf("grant: %v", err)
	}

	// Replace the root directory itself with a symlink to a sibling tree, after the
	// capability handle is already open.
	if err := os.Rename(root, filepath.Join(home, "proj.orig")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, root); err != nil {
		t.Fatal(err)
	}

	// The pinned handle still resolves ok.txt to the ORIGINAL inode, never the
	// swapped-in tree; the capability follows the directory it opened, not its name.
	f, authorized, err := m.OpenAsset(filepath.Join(root, "ok.txt"))
	if !authorized || err != nil {
		t.Fatalf("pinned read after root swap: authorized=%v err=%v", authorized, err)
	}
	data, _ := io.ReadAll(f)
	f.Close()
	if string(data) != "in-scope" {
		t.Fatalf("pinned root followed the swapped name, read %q", string(data))
	}
}

func TestEqualOrUnder(t *testing.T) {
	sep := string(os.PathSeparator)
	base := sep + "a" + sep + "b"

	if !EqualOrUnder(base, base) {
		t.Error("a directory must equal itself")
	}
	if !EqualOrUnder(base+sep+"c", base) {
		t.Error("a child must be under its parent")
	}
	if EqualOrUnder(base, base+sep+"c") {
		t.Error("a parent must not be under its child")
	}
	// A shared string prefix that is not a path boundary must not match.
	if EqualOrUnder(sep+"a"+sep+"bc", base) {
		t.Error("sibling with a shared prefix must not match")
	}
	// On case-insensitive platforms the same directory spelled differently is
	// the same directory; a byte-wise guard was the bug this closes.
	if caseInsensitiveFS() && !EqualOrUnder(strings.ToUpper(base)+sep+"c", base) {
		t.Error("case-insensitive platform must fold case")
	}
}

func TestAssetRootMostSpecific(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	os.MkdirAll(filepath.Join(home, "site", "img"), 0755)
	page := filepath.Join(home, "site", "page.html")
	os.WriteFile(page, []byte("<html></html>"), 0644)
	asset := filepath.Join(home, "site", "img", "logo.png")
	os.WriteFile(asset, []byte("png"), 0644)

	m := newTestManager(t, home)
	if _, err := m.Register(page, ViaOsOpen); err != nil { // opened root = home/site
		t.Fatalf("register: %v", err)
	}
	if err := m.GrantReadRoot(filepath.Join(home, "site", "img")); err != nil {
		t.Fatalf("grant: %v", err)
	}
	root, rel, ok := m.AssetRoot(asset)
	if !ok || root != filepath.Join(home, "site", "img") || rel != "logo.png" {
		t.Errorf("most-specific root should win: got %q, %q, %v", root, rel, ok)
	}
}

// There are exactly two per-file records. lastServerWrite is set by save,
// restore, documentid injection, and the first observation of a file, and never
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

// The probe must agree with what the volume actually does, measured
// independently on the same directory. This exercises the case-insensitive
// branch on macOS/Windows runners and the case-sensitive branch on Linux, so
// both outcomes are covered across CI without any platform assumption in the
// assertion itself.
func TestProbeCaseInsensitiveDirMatchesVolume(t *testing.T) {
	dir := t.TempDir()
	insensitive, err := probeCaseInsensitiveDir(dir)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}

	truthPath := filepath.Join(dir, "case-Truth")
	if err := os.WriteFile(truthPath, []byte("x"), 0644); err != nil {
		t.Fatalf("write ground-truth file: %v", err)
	}
	_, statErr := os.Stat(filepath.Join(dir, "CASE-tRUTH"))
	truth := statErr == nil

	if insensitive != truth {
		t.Fatalf("probe says insensitive=%v, the volume says %v", insensitive, truth)
	}
}

func TestProbeLeavesNoFileBehind(t *testing.T) {
	dir := t.TempDir()
	if _, err := probeCaseInsensitiveDir(dir); err != nil {
		t.Fatalf("probe: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("probe left %d file(s) behind: %v", len(entries), entries)
	}
}

func TestFlipASCIICase(t *testing.T) {
	for in, want := range map[string]string{
		".htmlclay-CaseProbe-12345": ".HTMLCLAY-cASEpROBE-12345",
		"abcXYZ":                    "ABCxyz",
		"1234-_.":                   "1234-_.",
		"":                          "",
	} {
		if got := flipASCIICase(in); got != want {
			t.Fatalf("flipASCIICase(%q) = %q, want %q", in, got, want)
		}
	}
}

// The scenario T2.3 names: a genuinely case-sensitive volume on macOS, where
// the old GOOS-keyed answer was wrong. Built with a throwaway APFS disk image;
// skipped when hdiutil cannot run (sandboxed CI).
func TestProbeOnCaseSensitiveVolume(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("hdiutil is macOS-only")
	}
	img := filepath.Join(t.TempDir(), "probe.dmg")
	if out, err := exec.Command("hdiutil", "create", "-size", "4m",
		"-fs", "Case-sensitive APFS", "-volname", "htmlclay-case-probe", img).CombinedOutput(); err != nil {
		t.Skipf("cannot create disk image: %v: %s", err, out)
	}
	out, err := exec.Command("hdiutil", "attach", "-nobrowse", "-readwrite", img).CombinedOutput()
	if err != nil {
		t.Skipf("cannot attach disk image: %v: %s", err, out)
	}
	mount := ""
	for _, line := range strings.Split(string(out), "\n") {
		if i := strings.Index(line, "/Volumes/"); i >= 0 {
			mount = strings.TrimSpace(line[i:])
		}
	}
	if mount == "" {
		t.Skipf("no mountpoint in hdiutil output: %s", out)
	}
	defer exec.Command("hdiutil", "detach", mount, "-force").Run()

	insensitive, err := probeCaseInsensitiveDir(mount)
	if err != nil {
		t.Fatalf("probe on case-sensitive volume: %v", err)
	}
	if insensitive {
		t.Fatal("a case-sensitive volume probed as case-insensitive")
	}
}
