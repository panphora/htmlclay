package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/panphora/htmlclay/config"
	"github.com/panphora/htmlclay/logging"
	"github.com/panphora/htmlclay/platform"
	"github.com/panphora/htmlclay/server"
	"github.com/panphora/htmlclay/session"
	"github.com/panphora/htmlclay/versions"
)

// No test may pop a real permission dialog. Every app built here defaults its
// confirm to deny, so a broker that parks an out-of-scope request resolves it
// without ever calling the native osascript prompt. A grant-flow test overrides
// a.rt.confirm before opening the site it wants to allow.
func denyConfirm(string, string) (platform.ConfirmChoice, error) {
	return platform.ConfirmDeny, nil
}

func allowOnceConfirm(string, string) (platform.ConfirmChoice, error) {
	return platform.ConfirmAllowOnce, nil
}

func trustFolderConfirm(string, string) (platform.ConfirmChoice, error) {
	return platform.ConfirmTrustFolder, nil
}

// countingDenyConfirm denies every prompt and counts how many were raised, so a
// test can assert that a refusal never reached the user at all.
func countingDenyConfirm(n *int32) func(string, string) (platform.ConfirmChoice, error) {
	return func(string, string) (platform.ConfirmChoice, error) {
		atomic.AddInt32(n, 1)
		return platform.ConfirmDeny, nil
	}
}

func newTestApp(t *testing.T, home string) *app {
	t.Helper()
	logger := logging.NewStdout()
	store := versions.New(t.TempDir())
	ls := server.NewLiveSync(server.SeqPath(store), logger)
	cfg, err := config.LoadFrom(t.TempDir())
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	configDir := filepath.Join(t.TempDir(), "htmlclay")
	a := &app{
		rt: &appRuntime{
			cfg:       cfg,
			logger:    logger,
			versions:  store,
			ls:        ls,
			home:      home,
			configDir: configDir,
			confirm:   denyConfirm,
			// Swallow notifications by default, for the same reason confirm is
			// stubbed: no test may put real UI on the user's screen. A test that
			// cares what the user was told overrides this with a recorder.
			notify: func(string, string) error { return nil },
			guard: func(dir string) bool {
				return session.EqualOrUnder(dir, configDir) || session.EqualOrUnder(configDir, dir)
			},
		},
	}
	t.Cleanup(func() {
		ls.Shutdown()
		a.mu.Lock()
		defer a.mu.Unlock()
		for _, s := range a.sites {
			s.close()
		}
	})
	return a
}

// openForTest drives the real routing path without launching a browser.
func (a *app) openForTest(t *testing.T, abs string) (*site, string) {
	t.Helper()
	resolved, err := resolveSymlinks(abs)
	if err != nil {
		t.Fatalf("resolve %s: %v", abs, err)
	}
	s, rel, ok := a.route(resolved)
	if !ok {
		t.Fatalf("route %s failed", resolved)
	}
	return s, rel
}

func fetch(t *testing.T, target string) (int, string) {
	t.Helper()
	resp, err := http.Get(target)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// fetchFull is fetch plus the response headers, for tests that compare two
// refusals field by field rather than by status alone.
func fetchFull(t *testing.T, target string) (int, http.Header, string) {
	t.Helper()
	resp, err := http.Get(target)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(body)
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// Two unrelated trees must land on different ports. That is what makes the
// browser treat them as separate origins, which blocks the cross-tree token read
// a single shared origin allows.
func TestUnrelatedTreesGetSeparateOrigins(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	fileA := filepath.Join(home, "work", "projA", "index.html")
	fileB := filepath.Join(home, "work", "projB", "index.html")
	writeTestFile(t, fileA, "<html><body>alpha</body></html>")
	writeTestFile(t, fileB, "<html><body>bravo</body></html>")

	a := newTestApp(t, home)
	siteA, relA := a.openForTest(t, fileA)
	siteB, relB := a.openForTest(t, fileB)

	if siteA.port == siteB.port {
		t.Fatalf("unrelated trees must not share a port (both on %d)", siteA.port)
	}

	code, body := fetch(t, fileURL(siteA.port, relA))
	if code != 200 || !strings.Contains(body, "alpha") {
		t.Errorf("site A should serve its own page: got %d, %q", code, body)
	}
	if !strings.Contains(body, `htmlclaytoken="`) {
		t.Error("served page should carry a save token")
	}

	if code, _ := fetch(t, fileURL(siteA.port, relB)); code == 200 {
		t.Error("site A must not serve site B's file")
	}
	if code, _ := fetch(t, fileURL(siteB.port, relA)); code == 200 {
		t.Error("site B must not serve site A's file")
	}
}

// A sibling in the SAME opened folder shares the site (same workspace). A file
// reached ONLY through a read-grant does NOT: an explicit open always anchors its own
// origin, so a read-only grant can never become write authority over a file opened
// later (Option B).
func TestSiblingSharesSiteButGrantedCousinIsIsolated(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	page := filepath.Join(home, "work", "review", "fable", "index.html")
	sibling := filepath.Join(home, "work", "review", "fable", "notes.html")
	cousin := filepath.Join(home, "work", "review", "codex", "index.html")
	writeTestFile(t, page, "<html><body>fable</body></html>")
	writeTestFile(t, sibling, "<html><body>notes</body></html>")
	writeTestFile(t, cousin, "<html><body>codex</body></html>")

	a := newTestApp(t, home)
	siteA, _ := a.openForTest(t, page)

	siteSib, _ := a.openForTest(t, sibling)
	if siteSib != siteA {
		t.Error("a sibling in the same opened folder should reuse the site")
	}

	if err := siteA.sessions.GrantReadRoot(filepath.Join(home, "work", "review")); err != nil {
		t.Fatalf("grant: %v", err)
	}

	// The cousin is covered only by the read-grant, not by any opened root, so it
	// must anchor its own origin rather than join (and become writable from) siteA.
	siteCousin, _ := a.openForTest(t, cousin)
	if siteCousin == siteA {
		t.Error("a grant-only file must NOT reuse the granting site (a read grant would become write)")
	}
	if len(a.sites) != 2 {
		t.Errorf("expected the cousin to get its own site, got %d sites", len(a.sites))
	}
}

// The security property Option B buys: a file opened under a read-grant is served
// with its save token on its OWN origin, so the granting page cannot fetch it and
// lift the token. The granting origin can still READ the cousin (via the grant) but
// only as a token-free asset.
func TestGrantedCousinTokenUnreachableFromGrantingOrigin(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	page := filepath.Join(home, "work", "review", "fable", "index.html")
	cousin := filepath.Join(home, "work", "review", "codex", "index.html")
	writeTestFile(t, page, "<html><body>fable</body></html>")
	writeTestFile(t, cousin, "<html><body>codex</body></html>")

	a := newTestApp(t, home)
	granting, _ := a.openForTest(t, page)
	if err := granting.sessions.GrantReadRoot(filepath.Join(home, "work", "review")); err != nil {
		t.Fatalf("grant: %v", err)
	}

	cousinSite, _ := a.openForTest(t, cousin)
	if cousinSite == granting {
		t.Fatal("cousin must not share the granting origin")
	}

	relCousin := filepath.Join("work", "review", "codex", "index.html")

	// From the granting origin: readable via the grant, but token-free.
	code, body := fetch(t, fileURL(granting.port, relCousin))
	if code != 200 {
		t.Fatalf("granting origin should still read the cousin via the grant: got %d", code)
	}
	if strings.Contains(body, "htmlclaytoken") {
		t.Error("the granting origin must never receive the cousin's save token")
	}

	// From the cousin's own origin: served self-saving, with a token.
	codeOwn, bodyOwn := fetch(t, fileURL(cousinSite.port, relCousin))
	if codeOwn != 200 || !strings.Contains(bodyOwn, "htmlclaytoken") {
		t.Errorf("the cousin's own origin should serve it self-saving: got %d", codeOwn)
	}
}

// A grant widens reads only. A file reached because of a grant is served
// read-only and never registered for saving.
func TestGrantedAssetIsServedWithoutToken(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	page := filepath.Join(home, "work", "review", "fable", "index.html")
	asset := filepath.Join(home, "work", "review", "redpen.js")
	writeTestFile(t, page, "<html><body>fable</body></html>")
	writeTestFile(t, asset, "console.log('redpen')")

	a := newTestApp(t, home)
	s, _ := a.openForTest(t, page)

	relAsset := filepath.Join("work", "review", "redpen.js")
	if code, _ := fetch(t, fileURL(s.port, relAsset)); code == 200 {
		t.Error("asset outside the opened folder should not be readable yet")
	}

	if err := s.sessions.GrantReadRoot(filepath.Join(home, "work", "review")); err != nil {
		t.Fatalf("grant: %v", err)
	}

	code, body := fetch(t, fileURL(s.port, relAsset))
	if code != 200 || !strings.Contains(body, "redpen") {
		t.Fatalf("granted asset should be readable: got %d, %q", code, body)
	}
	if _, ok := s.sessions.LookupByPath(asset); ok {
		t.Error("serving a granted asset must never register it for saving")
	}
}

// End to end: an out-of-scope asset request parks, the user allows, the grant
// widens reads, and the SAME request resumes with the asset served read-only.
// This is the whole point of the broker: a slow success instead of a reload.
func TestOutOfScopeAssetPromptsThenResumesOnAllow(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	page := filepath.Join(home, "work", "review", "fable", "index.html")
	asset := filepath.Join(home, "work", "review", "shared", "redpen.js")
	writeTestFile(t, page, "<html><body>fable</body></html>")
	writeTestFile(t, asset, "console.log('redpen')")

	a := newTestApp(t, home)
	a.rt.confirm = allowOnceConfirm
	s, _ := a.openForTest(t, page)

	relAsset := filepath.Join("work", "review", "shared", "redpen.js")
	code, body := fetch(t, fileURL(s.port, relAsset))
	if code != 200 || !strings.Contains(body, "redpen") {
		t.Fatalf("an allowed out-of-scope asset should resume with 200: got %d, %q", code, body)
	}
	if strings.Contains(body, "htmlclaytoken") {
		t.Error("a granted asset must be served without a save token")
	}
	if _, ok := s.sessions.LookupByPath(asset); ok {
		t.Error("serving a granted asset must never register it for saving")
	}
}

// The dialog must name the folder that is actually granted. With ~/alias a symlink
// to ~/Private Journal, the prompt used to say "alias" while the capability landed on
// the target, so the user approved one folder name and a different folder opened. The
// resolution happens once, after the decision to prompt, so the dialog, the log, the
// tray, and the installed root all name one directory.
func TestPromptNamesTheFolderItGrants(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skipf("symlinks require privileges on windows")
	}
	home, _ := filepath.EvalSymlinks(t.TempDir())
	page := filepath.Join(home, "site", "index.html")
	realDir := filepath.Join(home, "Private Journal")
	writeTestFile(t, page, "<html><body>page</body></html>")
	writeTestFile(t, filepath.Join(realDir, "notes.js"), "console.log('notes')")
	alias := filepath.Join(home, "alias")
	if err := os.Symlink(realDir, alias); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	var msgMu sync.Mutex
	var shown string
	a := newTestApp(t, home)
	a.rt.confirm = func(_ string, message string) (platform.ConfirmChoice, error) {
		msgMu.Lock()
		shown = message
		msgMu.Unlock()
		return platform.ConfirmAllowOnce, nil
	}
	s, _ := a.openForTest(t, page)

	if code, _ := fetch(t, fileURL(s.port, filepath.Join("alias", "notes.js"))); code != 200 {
		t.Fatalf("the aliased asset should resume with 200 after Allow, got %d", code)
	}

	grants := a.activeGrants()
	if len(grants) != 1 {
		t.Fatalf("expected exactly one grant, got %v", grants)
	}
	msgMu.Lock()
	defer msgMu.Unlock()
	if grants[0] != realDir {
		t.Errorf("the grant must land on the resolved folder: got %q, want %q", grants[0], realDir)
	}
	if !strings.Contains(shown, grants[0]) {
		t.Errorf("the dialog must name the folder it grants:\ndialog  = %q\ngranted = %q", shown, grants[0])
	}
	if strings.Contains(shown, alias) {
		t.Errorf("the dialog must not name the alias the page reached through: %q", shown)
	}
}

// The Temporary Access Granted tray submenu is backed by activeGrants (the
// runtime read grants) and revokeGrant. Allowing an out-of-scope asset installs a
// grant that appears in activeGrants; revoking it removes the grant and the asset
// is refused again. The confirm allows the first prompt and denies afterwards, so
// the post-revoke re-request 403s instead of re-granting.
func TestActiveGrantsListsAndRevokesRuntimeGrants(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	page := filepath.Join(home, "work", "review", "fable", "index.html")
	asset := filepath.Join(home, "work", "review", "shared", "redpen.js")
	writeTestFile(t, page, "<html><body>fable</body></html>")
	writeTestFile(t, asset, "console.log('redpen')")

	var confirmMu sync.Mutex
	allowed := false
	a := newTestApp(t, home)
	a.rt.confirm = func(string, string) (platform.ConfirmChoice, error) {
		confirmMu.Lock()
		defer confirmMu.Unlock()
		if !allowed {
			allowed = true
			return platform.ConfirmAllowOnce, nil
		}
		return platform.ConfirmDeny, nil
	}
	s, _ := a.openForTest(t, page)

	relAsset := filepath.Join("work", "review", "shared", "redpen.js")
	if code, _ := fetch(t, fileURL(s.port, relAsset)); code != 200 {
		t.Fatalf("an allowed out-of-scope asset should resume 200, got %d", code)
	}

	grantDir := filepath.Join(home, "work", "review", "shared")
	grants := a.activeGrants()
	if len(grants) != 1 || grants[0] != grantDir {
		t.Fatalf("activeGrants = %v, want [%s]", grants, grantDir)
	}

	after := a.revokeGrant(grantDir)
	if len(after) != 0 {
		t.Fatalf("revokeGrant should leave no grants, got %v", after)
	}

	if code, _ := fetch(t, fileURL(s.port, relAsset)); code != 403 {
		t.Errorf("a revoked grant must refuse the asset again, got %d", code)
	}
}

// Files loose in the home directory never install a read root (home must not
// become one), so each anchors a site at the same root. Keying sites by root
// dropped the earlier site from the map, orphaning its listener and letting the
// same file be registered twice with two tokens.
func TestFilesLooseInHomeKeepDistinctTrackedSites(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	fileA := filepath.Join(home, "a.html")
	fileB := filepath.Join(home, "b.html")
	writeTestFile(t, fileA, "<html><body>a</body></html>")
	writeTestFile(t, fileB, "<html><body>b</body></html>")

	a := newTestApp(t, home)
	siteA, _ := a.openForTest(t, fileA)
	siteB, _ := a.openForTest(t, fileB)

	if siteA == siteB {
		t.Fatal("two files loose in home should not share a session")
	}
	if len(a.sites) != 2 {
		t.Fatalf("both sites must stay tracked so shutdown can close them, got %d", len(a.sites))
	}

	again, _ := a.openForTest(t, fileA)
	if again != siteA {
		t.Error("reopening a file must reuse its original site, not mint a second token")
	}
	if len(a.sites) != 2 {
		t.Errorf("reopening must not create another site, got %d", len(a.sites))
	}
}

// A site is published only once registration succeeds, so an open that cannot be
// registered never leaves a listening server behind.
func TestFailedRegistrationLeavesNoSite(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	outside, _ := filepath.EvalSymlinks(t.TempDir())
	stray := filepath.Join(outside, "stray.html")
	writeTestFile(t, stray, "<html><body>stray</body></html>")

	a := newTestApp(t, home)
	if _, _, ok := a.route(stray); ok {
		t.Fatal("a file outside home must not register")
	}
	if len(a.sites) != 0 {
		t.Errorf("a refused open must leave no site behind, got %d", len(a.sites))
	}
}

// When a broad read grant and a narrow opened root both cover a path, the
// explicitly-opened site must win every time. Otherwise a page holding only a
// read grant could end up hosting a newly opened file and lift its save token.
func TestOpenPrefersOpenedRootOverGrantedRoot(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	fable := filepath.Join(home, "review", "fable", "index.html")
	codexPage := filepath.Join(home, "review", "codex", "index.html")
	notes := filepath.Join(home, "review", "codex", "notes.html")
	writeTestFile(t, fable, "<html><body>fable</body></html>")
	writeTestFile(t, codexPage, "<html><body>codex</body></html>")
	writeTestFile(t, notes, "<html><body>notes</body></html>")

	a := newTestApp(t, home)
	siteF, _ := a.openForTest(t, fable)
	siteC, _ := a.openForTest(t, codexPage)
	if siteF == siteC {
		t.Fatal("sibling trees should start as separate sites")
	}

	if err := siteF.sessions.GrantReadRoot(filepath.Join(home, "review")); err != nil {
		t.Fatalf("grant: %v", err)
	}

	// Loop to defeat map-iteration randomness: the old first-match rule passed
	// this roughly half the time.
	for i := 0; i < 25; i++ {
		a.mu.Lock()
		got := a.siteForLocked(notes)
		a.mu.Unlock()
		if got != siteC {
			t.Fatalf("iteration %d: the opened root must win over the broad grant", i)
		}
	}
}

// htmlclay's own config tree is refused on the serve path, so a read root that
// happens to cover it (here the opened page's own folder) still cannot expose it.
func TestConfigDirIsRefusedOnServePath(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	configDir := filepath.Join(home, "Library", "Application Support", "htmlclay")
	page := filepath.Join(home, "Library", "page.html")
	writeTestFile(t, page, "<html><body>p</body></html>")
	writeTestFile(t, filepath.Join(configDir, "config.json"), `{"mode":"app"}`)
	writeTestFile(t, filepath.Join(configDir, "htmlclay.log"), "log line")

	a := newTestApp(t, home)
	a.rt.configDir = configDir
	a.rt.guard = func(dir string) bool {
		return session.EqualOrUnder(dir, configDir) || session.EqualOrUnder(configDir, dir)
	}

	s, _ := a.openForTest(t, page) // opened root is ~/Library, which covers the config dir

	for _, name := range []string{"config.json", "htmlclay.log"} {
		rel := filepath.Join("Library", "Application Support", "htmlclay", name)
		if code, _ := fetch(t, fileURL(s.port, rel)); code != 404 {
			t.Errorf("%s must be refused on the serve path, got %d", name, code)
		}
	}
}

// The guard refuses a grant that covers the config tree from either direction,
// and is not fooled by a different spelling of the same directory.
func TestGuardRefusesConfigTreeBothDirections(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	configDir := filepath.Join(home, "Library", "Application Support", "htmlclay")
	writeTestFile(t, filepath.Join(configDir, "config.json"), "{}")
	page := filepath.Join(home, "Library", "page.html")
	writeTestFile(t, page, "<html><body>p</body></html>")

	a := newTestApp(t, home)
	a.rt.configDir = configDir
	a.rt.guard = func(dir string) bool {
		return session.EqualOrUnder(dir, configDir) || session.EqualOrUnder(configDir, dir)
	}
	s, _ := a.openForTest(t, page)

	for _, dir := range []string{
		configDir,                                    // exactly the config dir
		filepath.Join(configDir, "versions"),         // inside it
		filepath.Join(home, "Library"),               // an ancestor that swallows it
		filepath.Join(home, "Library", "Application Support"),
	} {
		if err := s.sessions.GrantReadRoot(dir); err == nil {
			t.Errorf("granting %q must be refused", dir)
		}
	}
}

// A file opened from inside a trusted folder serves its whole tree with no
// permission prompt (confirm defaults to deny, so any prompt would 403), while the
// opened file itself still self-saves. This is the point of Trusted Folders: run
// your own HTML with no hassle.
func TestFileOpenedFromTrustedFolderServesTreeWithoutPrompt(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	sitesDir := filepath.Join(home, "sites")
	page := filepath.Join(sitesDir, "app", "index.html")
	asset := filepath.Join(sitesDir, "shared", "lib.js")
	writeTestFile(t, page, "<html><body>app</body></html>")
	writeTestFile(t, asset, "console.log('lib')")

	a := newTestApp(t, home)
	if err := a.trustFolder(sitesDir); err != nil {
		t.Fatalf("trust: %v", err)
	}

	s, rel := a.openForTest(t, page)

	codePage, bodyPage := fetch(t, fileURL(s.port, rel))
	if codePage != 200 || !strings.Contains(bodyPage, "htmlclaytoken") {
		t.Errorf("a file opened from a trusted folder should still self-save: got %d", codePage)
	}

	relAsset := filepath.Join("sites", "shared", "lib.js")
	code, body := fetch(t, fileURL(s.port, relAsset))
	if code != 200 || !strings.Contains(body, "lib") {
		t.Fatalf("an asset anywhere in the trusted folder should serve with no prompt: got %d, %q", code, body)
	}
	if strings.Contains(body, "htmlclaytoken") {
		t.Error("a trusted-folder asset must be served read-only, without a save token")
	}
	if _, ok := s.sessions.LookupByPath(asset); ok {
		t.Error("serving a trusted asset must never register it for saving")
	}
}

// Trust is scoped to the anchor: a page opened from OUTSIDE every trusted folder
// gets no silent reads into one. Opening a Downloads file must never let it read a
// trusted ~/sites tree without a prompt.
func TestFileOpenedOutsideTrustedFoldersGetsNoTrustedReads(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	sitesDir := filepath.Join(home, "sites")
	trustedAsset := filepath.Join(sitesDir, "secret.js")
	page := filepath.Join(home, "Downloads", "sketchy", "index.html")
	writeTestFile(t, trustedAsset, "console.log('secret')")
	writeTestFile(t, page, "<html><body>sketchy</body></html>")

	a := newTestApp(t, home)
	if err := a.trustFolder(sitesDir); err != nil {
		t.Fatalf("trust: %v", err)
	}

	s, _ := a.openForTest(t, page)

	relTrusted := filepath.Join("sites", "secret.js")
	if code, body := fetch(t, fileURL(s.port, relTrusted)); code == 200 {
		t.Errorf("a page opened outside every trusted folder must not silently read one: got 200, %q", body)
	}
}

// Trusting a folder live-seeds the capability into a site that is already open,
// so the user does not have to reopen a page for a fresh trust to take effect.
func TestTrustFolderLiveSeedsAlreadyOpenSite(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	sitesDir := filepath.Join(home, "sites")
	page := filepath.Join(sitesDir, "app", "index.html")
	asset := filepath.Join(sitesDir, "shared", "lib.js")
	writeTestFile(t, page, "<html><body>app</body></html>")
	writeTestFile(t, asset, "console.log('lib')")

	a := newTestApp(t, home)
	s, _ := a.openForTest(t, page)

	relAsset := filepath.Join("sites", "shared", "lib.js")
	if code, _ := fetch(t, fileURL(s.port, relAsset)); code == 200 {
		t.Fatal("the asset should be out of scope before the folder is trusted")
	}

	if err := a.trustFolder(sitesDir); err != nil {
		t.Fatalf("trust: %v", err)
	}

	code, body := fetch(t, fileURL(s.port, relAsset))
	if code != 200 || !strings.Contains(body, "lib") {
		t.Fatalf("trusting a folder must live-seed the open site: got %d, %q", code, body)
	}
}

// Untrusting a folder live-revokes the seeded capability from open sites.
func TestUntrustFolderLiveRevokesFromOpenSite(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	sitesDir := filepath.Join(home, "sites")
	page := filepath.Join(sitesDir, "app", "index.html")
	asset := filepath.Join(sitesDir, "shared", "lib.js")
	writeTestFile(t, page, "<html><body>app</body></html>")
	writeTestFile(t, asset, "console.log('lib')")

	a := newTestApp(t, home)
	if err := a.trustFolder(sitesDir); err != nil {
		t.Fatalf("trust: %v", err)
	}
	s, _ := a.openForTest(t, page)

	relAsset := filepath.Join("sites", "shared", "lib.js")
	if code, _ := fetch(t, fileURL(s.port, relAsset)); code != 200 {
		t.Fatal("the trusted asset should serve while the folder is trusted")
	}

	if err := a.untrustFolder(sitesDir); err != nil {
		t.Fatalf("untrust: %v", err)
	}
	if code, _ := fetch(t, fileURL(s.port, relAsset)); code == 200 {
		t.Error("untrusting the folder must live-revoke the seeded read from the open site")
	}
}

// The permission dialog's "Trust this folder" choice must refuse the main personal
// folders: there the PAGE chose the folder, so one click could make the likeliest
// home of HTML the user never wrote permanently trusted. The tray's own picker
// still trusts the same folder, because that takes a deliberate act.
func TestPromptCannotTrustDownloads(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	downloads := filepath.Join(home, "Downloads")
	writeTestFile(t, filepath.Join(downloads, "sketchy", "index.html"), "<html><body>sketchy</body></html>")

	a := newTestApp(t, home)
	// The refusal notice is sent from its own goroutine (it must never block the
	// broker), so collect it through a channel rather than a slice read straight
	// after the call.
	told := make(chan string, 4)
	a.rt.notify = func(_, message string) error {
		told <- message
		return nil
	}

	if err := a.trustFromPrompt(downloads); err == nil {
		t.Error("a permission prompt must not be able to trust the Downloads folder")
	}
	if got := a.rt.cfg.TrustedFolderList(); len(got) != 0 {
		t.Errorf("a refused prompt-trust must leave the trusted list empty: %v", got)
	}
	// Refusing silently would be its own bug: the user asked for something durable
	// and got something that lasts until they quit, so they have to be told.
	select {
	case msg := <-told:
		if !strings.Contains(msg, downloads) {
			t.Errorf("the notice should name the folder it refused, got %q", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a refused prompt-trust must tell the user")
	}
	if extra := len(told); extra != 0 {
		t.Errorf("a refused prompt-trust must tell the user exactly once, got %d extra messages", extra)
	}

	// A page asking about a folder INSIDE Downloads is the same problem, since
	// trusting it would still cover files the user did not write.
	if err := a.trustFromPrompt(filepath.Join(downloads, "sketchy")); err == nil {
		t.Error("a permission prompt must not be able to trust a folder inside Downloads")
	}

	// Desktop and Documents are refused for the same reason: they are where
	// archives get extracted, and a page can inflate its ask to the whole tree.
	for _, dir := range []string{filepath.Join(home, "Desktop"), filepath.Join(home, "Documents", "evil")} {
		if err := a.trustFromPrompt(dir); err == nil {
			t.Errorf("a permission prompt must not be able to trust %s", dir)
		}
	}

	if err := a.trustFolder(downloads); err != nil {
		t.Fatalf("the tray route must still be able to trust Downloads: %v", err)
	}
	if !hasFolder(a.rt.cfg.TrustedFolderList(), downloads) {
		t.Errorf("the tray route should have trusted Downloads: %v", a.rt.cfg.TrustedFolderList())
	}
}

func hasFolder(list []string, want string) bool {
	for _, f := range list {
		if f == want {
			return true
		}
	}
	return false
}

// waitForTrusted waits for dir to reach the trusted list. The durable half of
// "Trust this folder" runs after the parked request has already resumed, so a 200
// does not prove the config write landed.
func waitForTrusted(t *testing.T, a *app, dir string) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if hasFolder(a.trustedFolders(), dir) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the prompt's Trust this folder choice must record %s: trusted list = %v", dir, a.trustedFolders())
}

// The production wiring, end to end. The broker's trust hook must be the app's own
// prompt route: a "Trust this folder" answer over an ordinary folder both resumes
// the read and records the folder. Without the wiring the read still resumes, so
// only the recorded folder proves the hook was ever called.
func TestPromptTrustChoiceRecordsAnOrdinaryFolder(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	page := filepath.Join(home, "work", "review", "fable", "index.html")
	asset := filepath.Join(home, "work", "review", "shared", "redpen.js")
	writeTestFile(t, page, "<html><body>fable</body></html>")
	writeTestFile(t, asset, "console.log('redpen')")

	a := newTestApp(t, home)
	a.rt.confirm = trustFolderConfirm
	s, _ := a.openForTest(t, page)

	relAsset := filepath.Join("work", "review", "shared", "redpen.js")
	code, body := fetch(t, fileURL(s.port, relAsset))
	if code != 200 || !strings.Contains(body, "redpen") {
		t.Fatalf("a trusted out-of-scope asset should resume 200: got %d, %q", code, body)
	}
	waitForTrusted(t, a, filepath.Join(home, "work", "review", "shared"))
}

// The same flow aimed at one of the main personal folders. The page picked the
// folder, by asking for an asset that sits directly in ~/Documents, so the read is
// allowed for this session and nothing durable is recorded. Wiring the tray's own
// permissive route here instead would trust the whole of Documents in one click.
func TestPromptTrustChoiceRefusesAPersonalFolder(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	page := filepath.Join(home, "Documents", "evil", "index.html")
	asset := filepath.Join(home, "Documents", "lib.js")
	writeTestFile(t, page, "<html><body>evil</body></html>")
	writeTestFile(t, asset, "console.log('lib')")

	a := newTestApp(t, home)
	a.rt.confirm = trustFolderConfirm
	told := make(chan string, 4)
	a.rt.notify = func(_, message string) error {
		told <- message
		return nil
	}
	s, _ := a.openForTest(t, page)

	code, body := fetch(t, fileURL(s.port, filepath.Join("Documents", "lib.js")))
	if code != 200 || !strings.Contains(body, "lib") {
		t.Fatalf("the read itself should still resume: got %d, %q", code, body)
	}

	// The notice is what proves the prompt route ran and refused; the user asked
	// for something durable and got something that lasts until they quit.
	documents := filepath.Join(home, "Documents")
	select {
	case msg := <-told:
		if !strings.Contains(msg, documents) {
			t.Errorf("the notice should name the folder it refused, got %q", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("a page-chosen personal folder must be refused and reported: trusted list = %v", a.trustedFolders())
	}
	if got := a.trustedFolders(); len(got) != 0 {
		t.Errorf("a page-chosen personal folder must never be remembered: %v", got)
	}
}

// Removing a folder from Trusted Folders takes back the read access that same
// click granted. One "Trust this folder" answer sets both provenance flags on a
// single read root, so clearing trust alone left the folder readable, and the tray
// offered no second handle: a trusted root is hidden from Temporary Access Granted.
func TestUntrustingAPromptTrustedFolderEndsTheRead(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	page := filepath.Join(home, "work", "review", "fable", "index.html")
	asset := filepath.Join(home, "work", "review", "shared", "redpen.js")
	writeTestFile(t, page, "<html><body>fable</body></html>")
	writeTestFile(t, asset, "console.log('redpen')")

	// Trust the first prompt and deny afterwards, so the re-request after the
	// untrust cannot quietly re-grant the folder it just lost.
	var confirmMu sync.Mutex
	asked := false
	a := newTestApp(t, home)
	a.rt.confirm = func(string, string) (platform.ConfirmChoice, error) {
		confirmMu.Lock()
		defer confirmMu.Unlock()
		if !asked {
			asked = true
			return platform.ConfirmTrustFolder, nil
		}
		return platform.ConfirmDeny, nil
	}
	s, _ := a.openForTest(t, page)

	relAsset := filepath.Join("work", "review", "shared", "redpen.js")
	if code, _ := fetch(t, fileURL(s.port, relAsset)); code != 200 {
		t.Fatalf("a trusted out-of-scope asset should resume 200, got %d", code)
	}
	shared := filepath.Join(home, "work", "review", "shared")
	waitForTrusted(t, a, shared)

	if err := a.untrustFolder(shared); err != nil {
		t.Fatalf("untrust: %v", err)
	}
	if code, _ := fetch(t, fileURL(s.port, relAsset)); code == 200 {
		t.Error("removing a folder from Trusted Folders must end the read access that same click granted")
	}
}

// A burst of out-of-scope subresource requests under one common dir, arriving
// together, produces exactly one prompt and one installed read root, and every
// request resumes with a token-free 200 on Allow. This is the end-to-end form of
// the broker's batch-to-one-prompt invariant, fired as concurrent real GETs.
func TestConcurrentOutOfScopeAssetsResumeTogetherOnAllow(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	page := filepath.Join(home, "work", "review", "fable", "index.html")
	writeTestFile(t, page, "<html><body>fable</body></html>")
	shared := filepath.Join(home, "work", "review", "shared")
	const n = 8
	names := make([]string, n)
	for i := 0; i < n; i++ {
		names[i] = fmt.Sprintf("a%d.js", i)
		writeTestFile(t, filepath.Join(shared, names[i]), fmt.Sprintf("console.log(%d)", i))
	}

	var prompts int32
	entered := make(chan struct{})
	var enterOnce sync.Once
	release := make(chan struct{})
	a := newTestApp(t, home)
	a.rt.confirm = func(string, string) (platform.ConfirmChoice, error) {
		atomic.AddInt32(&prompts, 1)
		enterOnce.Do(func() { close(entered) })
		<-release
		return platform.ConfirmAllowOnce, nil
	}
	s, _ := a.openForTest(t, page)

	start := make(chan struct{})
	var wg sync.WaitGroup
	codes := make([]int, n)
	tokens := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			rel := filepath.Join("work", "review", "shared", names[i])
			resp, err := http.Get(fileURL(s.port, rel))
			if err != nil {
				codes[i] = -1
				return
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			codes[i] = resp.StatusCode
			tokens[i] = strings.Contains(string(body), "htmlclaytoken")
		}(i)
	}
	// Release all N requests at once so they are genuinely concurrent, then hold the
	// one prompt open while they pile up behind it. With the prompt blocked no grant
	// exists yet, so every request must park rather than be served, and the single
	// Allow has to resolve the whole batch. A sequential run could never reach this
	// state, so this actually exercises batch-to-one-prompt.
	close(start)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("permission prompt was never raised")
	}
	time.Sleep(150 * time.Millisecond) // let the other waiters park behind the held prompt
	close(release)
	wg.Wait()

	for i := 0; i < n; i++ {
		if codes[i] != 200 {
			t.Errorf("waiter %d should resume 200 on allow, got %d", i, codes[i])
		}
		if tokens[i] {
			t.Errorf("granted asset %d must be served without a save token", i)
		}
	}
	if got := atomic.LoadInt32(&prompts); got != 1 {
		t.Errorf("a burst under one common dir must prompt exactly once, got %d", got)
	}
	grants := a.activeGrants()
	if len(grants) != 1 || grants[0] != shared {
		t.Errorf("exactly one read root should be installed: activeGrants = %v, want [%s]", grants, shared)
	}
}

// The deny half of the same invariant: a concurrent burst under one common dir
// prompts exactly once, installs no read root, and every request gets the fixed
// path-free 403. The 403 body carries no filesystem path, so a denied read is not
// an oracle for where the assets live.
func TestConcurrentOutOfScopeAssetsRefuseTogetherOnDeny(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	page := filepath.Join(home, "work", "review", "fable", "index.html")
	writeTestFile(t, page, "<html><body>fable</body></html>")
	shared := filepath.Join(home, "work", "review", "shared")
	const n = 8
	names := make([]string, n)
	for i := 0; i < n; i++ {
		names[i] = fmt.Sprintf("a%d.js", i)
		writeTestFile(t, filepath.Join(shared, names[i]), fmt.Sprintf("console.log(%d)", i))
	}

	var prompts int32
	entered := make(chan struct{})
	var enterOnce sync.Once
	release := make(chan struct{})
	a := newTestApp(t, home)
	a.rt.confirm = func(string, string) (platform.ConfirmChoice, error) {
		atomic.AddInt32(&prompts, 1)
		enterOnce.Do(func() { close(entered) })
		<-release
		return platform.ConfirmDeny, nil
	}
	s, _ := a.openForTest(t, page)

	start := make(chan struct{})
	var wg sync.WaitGroup
	codes := make([]int, n)
	leaked := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			rel := filepath.Join("work", "review", "shared", names[i])
			resp, err := http.Get(fileURL(s.port, rel))
			if err != nil {
				codes[i] = -1
				return
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			codes[i] = resp.StatusCode
			leaked[i] = strings.Contains(string(body), home)
		}(i)
	}
	// Same barrier as the allow case: fire all N together, hold the single prompt
	// open until the others have parked behind it, then let the one Deny resolve the
	// whole batch. Proves N concurrent parks collapse to one prompt, not N.
	close(start)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("permission prompt was never raised")
	}
	time.Sleep(150 * time.Millisecond)
	close(release)
	wg.Wait()

	for i := 0; i < n; i++ {
		if codes[i] != 403 {
			t.Errorf("waiter %d should refuse with 403 on deny, got %d", i, codes[i])
		}
		if leaked[i] {
			t.Errorf("the 403 body for waiter %d must carry no filesystem path", i)
		}
	}
	if got := atomic.LoadInt32(&prompts); got != 1 {
		t.Errorf("a denied burst must prompt exactly once, got %d", got)
	}
	if grants := a.activeGrants(); len(grants) != 0 {
		t.Errorf("a denied burst must install no read root, got %v", grants)
	}
}

// A symlink inside a served tree that resolves into htmlclay's own config tree is
// refused. The internal-path denial is structural on the serve path, so a page
// cannot reach the config/versions tree by planting a symlink to it, even from an
// opened root that covers the config directory.
func TestServeAssetSymlinkIntoConfigTreeRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require privileges on windows")
	}
	home, _ := filepath.EvalSymlinks(t.TempDir())
	configDir := filepath.Join(home, "Library", "Application Support", "htmlclay")
	writeTestFile(t, filepath.Join(configDir, "config.json"), `{"mode":"app"}`)
	page := filepath.Join(home, "Library", "page.html")
	writeTestFile(t, page, "<html><body>p</body></html>")
	if err := os.Symlink(filepath.Join(configDir, "config.json"), filepath.Join(home, "Library", "sneak.js")); err != nil {
		t.Fatal(err)
	}

	a := newTestApp(t, home)
	a.rt.configDir = configDir
	a.rt.guard = func(dir string) bool {
		return session.EqualOrUnder(dir, configDir) || session.EqualOrUnder(configDir, dir)
	}
	s, _ := a.openForTest(t, page) // opened root ~/Library covers the config dir

	code, body := fetch(t, fileURL(s.port, filepath.Join("Library", "sneak.js")))
	if code != 404 {
		t.Errorf("a symlink into the config tree must be refused with 404, got %d", code)
	}
	if strings.Contains(body, "mode") {
		t.Error("config-tree contents must never be served")
	}
}

// The racy sibling of the test above. os.Root follows relative in-root symlinks, so
// a directory component swapped for such a symlink BETWEEN the serve-path resolution
// check and the capability open could redirect the read into the config tree even
// though the pre-check saw a benign path. A flipper races `swap` between a benign
// real dir and a relative symlink into the config tree while requests hammer it; the
// served body must never carry the secret. Only a leak can fail this, so the timing
// race can surface the TOCTOU but never mask it.
func TestServeAssetSymlinkSwapRaceNeverLeaksConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require privileges on windows")
	}
	home, _ := filepath.EvalSymlinks(t.TempDir())
	configDir := filepath.Join(home, "Library", "Application Support", "htmlclay")
	writeTestFile(t, filepath.Join(configDir, "config.json"), `{"secret":"SECRETVALUE"}`)
	page := filepath.Join(home, "Library", "page.html")
	writeTestFile(t, page, "<html><body>p</body></html>")

	swap := filepath.Join(home, "Library", "swap")
	relTarget := filepath.Join("Application Support", "htmlclay") // relative: stays inside the ~/Library root
	benign := func() {
		os.RemoveAll(swap)
		os.MkdirAll(swap, 0755)
		os.WriteFile(filepath.Join(swap, "config.json"), []byte("benign"), 0644)
	}
	attack := func() {
		os.RemoveAll(swap)
		os.Symlink(relTarget, swap)
	}
	benign()

	a := newTestApp(t, home)
	a.rt.configDir = configDir
	a.rt.guard = func(dir string) bool {
		return session.EqualOrUnder(dir, configDir) || session.EqualOrUnder(configDir, dir)
	}
	s, _ := a.openForTest(t, page) // opened root ~/Library covers the config dir

	stop := make(chan struct{})
	var flipper sync.WaitGroup
	flipper.Add(1)
	go func() {
		defer flipper.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				attack()
			} else {
				benign()
			}
		}
	}()

	url := fileURL(s.port, filepath.Join("Library", "swap", "config.json"))
	for i := 0; i < 400; i++ {
		resp, err := http.Get(url)
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(body), "SECRETVALUE") {
			close(stop)
			flipper.Wait()
			t.Fatalf("serve path leaked the config tree through a symlink swap on iteration %d", i)
		}
	}
	close(stop)
	flipper.Wait()
}

// Untrusting a folder is complete: it drops the folder from the trusted list AND
// live-revokes the seeded read from every open site, so the tree 403s again. The
// two halves must move together, or a stale config entry could re-seed a revoked
// root on the next open.
func TestUntrustFolderClearsConfigAndRevokesServe(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	sitesDir := filepath.Join(home, "sites")
	page := filepath.Join(sitesDir, "app", "index.html")
	asset := filepath.Join(sitesDir, "shared", "lib.js")
	writeTestFile(t, page, "<html><body>app</body></html>")
	writeTestFile(t, asset, "console.log('lib')")

	cfgBase := t.TempDir()
	cfg, err := config.LoadFrom(cfgBase)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	a := newTestApp(t, home)
	a.rt.cfg = cfg // a config whose on-disk baseDir the test can reload
	if err := a.trustFolder(sitesDir); err != nil {
		t.Fatalf("trust: %v", err)
	}
	s, _ := a.openForTest(t, page)

	relAsset := filepath.Join("sites", "shared", "lib.js")
	if code, _ := fetch(t, fileURL(s.port, relAsset)); code != 200 {
		t.Fatal("the trusted asset should serve while the folder is trusted")
	}
	if !hasFolder(a.trustedFolders(), sitesDir) {
		t.Fatalf("config should list the trusted folder before untrust: %v", a.trustedFolders())
	}

	if err := a.untrustFolder(sitesDir); err != nil {
		t.Fatalf("untrust: %v", err)
	}

	if hasFolder(a.trustedFolders(), sitesDir) {
		t.Errorf("untrust must drop the folder from config: %v", a.trustedFolders())
	}
	// The removal must reach disk, not just memory, or a restart re-seeds the revoked
	// root from a stale config entry.
	reloaded, err := config.LoadFrom(cfgBase)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if hasFolder(reloaded.TrustedFolderList(), sitesDir) {
		t.Errorf("untrust must persist to disk: reloaded trusted list still has it: %v", reloaded.TrustedFolderList())
	}
	if code, _ := fetch(t, fileURL(s.port, relAsset)); code != 403 {
		t.Errorf("untrust must live-revoke the seeded read (403), got %d", code)
	}
}

// The existence-oracle regression. Two out-of-scope requests in the same folder,
// one for a file that is really there and one for a name that is not, must be
// indistinguishable: same status, same error header, same body. Before the fix the
// missing one answered 404 immediately while the present one parked and 403'd, so a
// page could map every file and folder in the non-hidden home tree with no grant and
// no user interaction.
// The two probes sit under DIFFERENT top-level segments below home on purpose.
// Suppression is per denied tree, so two probes in one tree would have the second
// short-circuit inside await without ever parking: both would still end at 403, but
// for different reasons, and an existence check added inside the broker would slip
// through. Under separate trees each request really parks and really prompts.
func TestOutOfScopeMissingAndPresentAreIndistinguishable(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	page := filepath.Join(home, "site", "index.html")
	present := filepath.Join(home, "present-tree", "present.txt")
	writeTestFile(t, page, "<html><body>page</body></html>")
	writeTestFile(t, present, "secret")

	var prompts int32
	a := newTestApp(t, home)
	a.rt.confirm = countingDenyConfirm(&prompts)
	s, _ := a.openForTest(t, page)

	codePresent, hdrPresent, bodyPresent := fetchFull(t, fileURL(s.port, filepath.Join("present-tree", "present.txt")))
	codeMissing, hdrMissing, bodyMissing := fetchFull(t, fileURL(s.port, filepath.Join("missing-tree", "missing.txt")))

	if got := atomic.LoadInt32(&prompts); got != 2 {
		t.Errorf("both out-of-scope reads must park and prompt, got %d prompts", got)
	}
	if codePresent != 403 || codeMissing != 403 {
		t.Fatalf("both out-of-scope reads must refuse with 403: present=%d missing=%d", codePresent, codeMissing)
	}
	if hdrPresent.Get("X-HTMLClay-Error") != "read-access-required" ||
		hdrMissing.Get("X-HTMLClay-Error") != "read-access-required" {
		t.Errorf("both refusals must carry the same error header: present=%q missing=%q",
			hdrPresent.Get("X-HTMLClay-Error"), hdrMissing.Get("X-HTMLClay-Error"))
	}
	if bodyPresent != bodyMissing {
		t.Errorf("the two refusals must be byte-identical:\npresent=%q\nmissing=%q", bodyPresent, bodyMissing)
	}
	if strings.Contains(bodyPresent, "secret") || strings.Contains(bodyPresent, home) {
		t.Error("the refusal body must carry neither content nor a filesystem path")
	}
}

// A missing asset INSIDE the opened root is an ordinary 404 and never prompts:
// the scope check passes on the lexical path, so the broker is never consulted and
// the user is not asked about a file their own page just mistyped.
func TestInScopeMissingAssetIs404(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	page := filepath.Join(home, "site", "index.html")
	writeTestFile(t, page, "<html><body>page</body></html>")

	var prompts int32
	a := newTestApp(t, home)
	a.rt.confirm = countingDenyConfirm(&prompts)
	s, _ := a.openForTest(t, page)

	if code, _ := fetch(t, fileURL(s.port, filepath.Join("site", "missing.css"))); code != 404 {
		t.Errorf("a missing in-scope asset must 404, got %d", code)
	}
	if got := atomic.LoadInt32(&prompts); got != 0 {
		t.Errorf("an in-scope path must never prompt, got %d prompts", got)
	}
}

// htmlclay's own state answers the same way whether or not the named file is there,
// and never prompts. isInternal runs on the lexical path, so the refusal beats the
// permission dialog and cannot be used to probe the config or versions tree.
//
// The page is anchored well away from the config tree on purpose: with the tree
// lexically in scope of the opened root the test proved nothing, because a present
// file was refused by a later resolved-path check and a missing one by a failed
// resolution, and neither answer needed the lexical test at all. Out of scope, a
// request that skipped the lexical test would park and 403 instead.
func TestInternalPathIs404WhetherPresentOrMissing(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	configDir := filepath.Join(home, "Library", "Application Support", "htmlclay")
	page := filepath.Join(home, "site", "index.html")
	writeTestFile(t, page, "<html><body>p</body></html>")
	writeTestFile(t, filepath.Join(configDir, "config.json"), `{"mode":"app"}`)

	var prompts int32
	a := newTestApp(t, home)
	a.rt.configDir = configDir
	a.rt.guard = func(dir string) bool {
		return session.EqualOrUnder(dir, configDir) || session.EqualOrUnder(configDir, dir)
	}
	a.rt.confirm = countingDenyConfirm(&prompts)
	s, _ := a.openForTest(t, page) // opened root ~/site; the config tree is out of scope

	base := filepath.Join("Library", "Application Support", "htmlclay")
	for _, name := range []string{"config.json", "nothing-here.json"} {
		code, body := fetch(t, fileURL(s.port, filepath.Join(base, name)))
		if code != 404 {
			t.Errorf("%s must be refused with 404, got %d", name, code)
		}
		if strings.Contains(body, "mode") {
			t.Errorf("%s: config-tree contents must never be served", name)
		}
	}
	if got := atomic.LoadInt32(&prompts); got != 0 {
		t.Errorf("an internal path must never prompt, got %d prompts", got)
	}
}

// serveAsset re-tests internal-ness on the FULL request path, which is not always
// the path handleServeFile judged. extractFilePath stops at the first ".html" so an
// asset inside a directory named like a page still resolves, and everything past that
// point is only ever seen by serveAsset. A percent-encoded traversal after the
// truncation lands in the config tree while the truncated path looks innocent
// (ServeMux cleans the escaped path but hands the handler the unescaped one), so
// serveAsset's own lexical check is what refuses it, before the scope gate and with
// no prompt. Without that check the request parks instead and 403s.
func TestEncodedTraversalIntoConfigTreeIs404NotAPrompt(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	configDir := filepath.Join(home, "Library", "Application Support", "htmlclay")
	page := filepath.Join(home, "site", "index.html")
	writeTestFile(t, page, "<html><body>p</body></html>")
	writeTestFile(t, filepath.Join(configDir, "config.json"), `{"mode":"app"}`)

	var prompts int32
	a := newTestApp(t, home)
	a.rt.configDir = configDir
	a.rt.guard = func(dir string) bool {
		return session.EqualOrUnder(dir, configDir) || session.EqualOrUnder(configDir, dir)
	}
	a.rt.confirm = countingDenyConfirm(&prompts)
	s, _ := a.openForTest(t, page)

	// Built by hand: fileURL runs url.JoinPath, which would collapse the traversal
	// before it was ever sent.
	target := fmt.Sprintf(
		"http://127.0.0.1:%d/site/nothing.html/%%2e%%2e/%%2e%%2e/Library/Application%%20Support/htmlclay/config.json",
		s.port)
	code, body := fetch(t, target)
	if code != 404 {
		t.Errorf("a traversal into the config tree must be refused with 404, got %d", code)
	}
	if strings.Contains(body, "mode") {
		t.Error("config-tree contents must never be served")
	}
	if got := atomic.LoadInt32(&prompts); got != 0 {
		t.Errorf("an internal path must never prompt, got %d prompts", got)
	}
}

// A hidden component answers the same way whether or not the file is there, and
// never prompts, even out of scope: the dot test is lexical, so it beats the
// permission dialog and cannot be used to probe for a .env or a .git.
func TestHiddenPathIs404WhetherPresentOrMissing(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	page := filepath.Join(home, "site", "index.html")
	writeTestFile(t, page, "<html><body>page</body></html>")
	writeTestFile(t, filepath.Join(home, "other", ".env"), "SECRET=1")
	writeTestFile(t, filepath.Join(home, "other", ".git", "config"), "[core]")

	var prompts int32
	a := newTestApp(t, home)
	a.rt.confirm = countingDenyConfirm(&prompts)
	s, _ := a.openForTest(t, page)

	for _, rel := range []string{
		filepath.Join("other", ".env"),           // present
		filepath.Join("other", ".git", "config"), // present, hidden directory
		filepath.Join("other", ".missing"),       // absent
	} {
		code, body := fetch(t, fileURL(s.port, rel))
		if code != 404 {
			t.Errorf("%s must be refused with 404, got %d", rel, code)
		}
		if strings.Contains(body, "SECRET") || strings.Contains(body, "core") {
			t.Errorf("%s: hidden contents must never be served", rel)
		}
	}
	if got := atomic.LoadInt32(&prompts); got != 0 {
		t.Errorf("a hidden path must never prompt, got %d prompts", got)
	}
}

// A lexically in-scope path that resolves, through a symlink, outside every read
// root is refused outright rather than turned into a prompt. The scope check decides
// only whether to ask; authorization still judges the resolved path, so the user is
// never asked to grant whatever a link inside their folder happens to point at.
func TestAssetResolvingOutsideItsRootIs404NotAPrompt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skipf("symlinks require privileges on windows")
	}
	home, _ := filepath.EvalSymlinks(t.TempDir())
	page := filepath.Join(home, "site", "index.html")
	target := filepath.Join(home, "other", "secret.txt")
	writeTestFile(t, page, "<html><body>page</body></html>")
	writeTestFile(t, target, "OUTSIDE-SECRET")
	if err := os.Symlink(target, filepath.Join(home, "site", "link.txt")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	var prompts int32
	a := newTestApp(t, home)
	a.rt.confirm = countingDenyConfirm(&prompts)
	s, _ := a.openForTest(t, page)

	code, body := fetch(t, fileURL(s.port, filepath.Join("site", "link.txt")))
	if code != 404 {
		t.Errorf("a symlink resolving outside every read root must 404, got %d", code)
	}
	if strings.Contains(body, "OUTSIDE-SECRET") {
		t.Error("the symlink target must never be served")
	}
	if got := atomic.LoadInt32(&prompts); got != 0 {
		t.Errorf("a symlink out of its root must not raise a prompt, got %d prompts", got)
	}
}
