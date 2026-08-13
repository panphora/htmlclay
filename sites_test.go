package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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
			// The write-granting dialogs deny by default through their own
			// distinct seams, so a test that allows read grants does not
			// accidentally approve opens or workspaces.
			confirmOpen:      func(string, string) (bool, error) { return false, nil },
			confirmWorkspace: func(string, string) (bool, error) { return false, nil },
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
	s, rel, ok := a.route(resolved, session.ViaOsOpen)
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
	if _, _, ok := a.route(stray, session.ViaOsOpen); ok {
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

// ---- Workspace trust: Feature A (open the file you're viewing) and Feature B
// (workspace folders) integration tests. Server-level unit tests live in
// server/workspace_trust_test.go; these drive real sites through the app seam.

var bannerNonceRe = regexp.MustCompile(`\{nonce:'([A-Za-z0-9_-]+)'\}`)
var tokenAttrRe = regexp.MustCompile(`htmlclaytoken="([^"]+)"`)

// fetchNav is fetch with the headers of a real user navigation, which is what
// arms the read-only banner and the auto-register branch.
func fetchNav(t *testing.T, target string) (int, string) {
	t.Helper()
	req, err := http.NewRequest("GET", target, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-User", "?1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// fetchAsSubresource mimics a page's silent fetch(): Sec-Fetch-Dest empty.
func fetchAsSubresource(t *testing.T, target string) (int, string) {
	t.Helper()
	req, err := http.NewRequest("GET", target, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Origin", "http://"+req.URL.Host)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// postSameOrigin posts with the browser's same-origin attestation, which every
// mutating /_/ route requires.
func postSameOrigin(t *testing.T, target, contentType, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest("POST", target, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Origin", "http://"+req.URL.Host)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", target, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// The whole Feature A flow on one origin: a linked sibling serves read-only
// with a banner, approval opens it in place (same site, same origin), the page
// reloads editable, and the tray can revoke exactly that grant while the
// OS-opened page keeps its own session.
func TestOpenBannerFlowOpensSiblingInPlace(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	page := filepath.Join(home, "site", "index.htmlclay")
	sibling := filepath.Join(home, "site", "week.htmlclay")
	writeTestFile(t, page, "<html><body>index</body></html>")
	writeTestFile(t, sibling, "<html><body>week</body></html>")

	a := newTestApp(t, home)
	openCalls := 0
	a.rt.confirmOpen = func(title, message string) (bool, error) {
		openCalls++
		if !strings.Contains(message, sibling) {
			t.Errorf("dialog must name the full path %s, got %q", sibling, message)
		}
		return true, nil
	}
	s, _ := a.openForTest(t, page)

	relSibling := filepath.Join("site", "week.htmlclay")
	target := fileURL(s.port, relSibling)

	code, body := fetchNav(t, target)
	if code != 200 || !strings.Contains(body, "htmlclay-banner") {
		t.Fatalf("sibling navigation should serve the banner: %d", code)
	}
	if strings.Contains(body, "htmlclaytoken") {
		t.Fatal("read-only serve leaked a token")
	}
	m := bannerNonceRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no nonce in banner: %q", body)
	}

	code, out := postSameOrigin(t, fmt.Sprintf("http://127.0.0.1:%d/_/open-request", s.port),
		"application/json", `{"nonce":"`+m[1]+`"}`)
	if code != 200 || !strings.Contains(out, `"ok":true`) {
		t.Fatalf("open-request failed: %d %s", code, out)
	}
	if openCalls != 1 {
		t.Fatalf("dialog fired %d times, want 1", openCalls)
	}
	var resp struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.URL != target {
		t.Fatalf("sibling should open in place: url=%q, want %q", resp.URL, target)
	}
	if len(a.sites) != 1 {
		t.Fatalf("sibling open created a new site: %d sites", len(a.sites))
	}
	if via := s.sessions.Via(sibling); via != session.ViaOpenRequest {
		t.Fatalf("sibling provenance = %v, want ViaOpenRequest", via)
	}

	code, body = fetch(t, resp.URL)
	if code != 200 || !strings.Contains(body, "htmlclaytoken") {
		t.Fatal("approved sibling should serve editable")
	}

	// The tray lists exactly this grant, and revoking it ends the session
	// without touching the OS-opened page.
	if list := a.openedForEditing(); len(list) != 1 || list[0] != sibling {
		t.Fatalf("openedForEditing = %v", list)
	}
	tok := tokenAttrRe.FindStringSubmatch(body)
	if tok == nil {
		t.Fatal("no token in editable serve")
	}
	a.revokeOpened(sibling)
	if list := a.openedForEditing(); len(list) != 0 {
		t.Fatalf("grant survived revoke: %v", list)
	}
	code, _ = postSameOrigin(t, fmt.Sprintf("http://127.0.0.1:%d/_/save/%s", s.port, tok[1]),
		"text/html", "<html><body>worm</body></html>")
	if code != 401 {
		t.Fatalf("save after revoke = %d, want 401", code)
	}
	if _, ok := s.sessions.LookupByPath(page); !ok {
		t.Fatal("revoking the sibling killed the OS-opened page's session")
	}
	if code, body := fetchNav(t, target); code != 200 || strings.Contains(body, "htmlclaytoken") {
		t.Fatal("revoked sibling should serve read-only again")
	}
}

// T0.2's regression: a file covered only by a read grant must open on a fresh
// origin, never on the granting one, so the granting page can never lift the
// new file's token.
func TestOpenRequestGrantOnlyFileGetsFreshOrigin(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	page := filepath.Join(home, "work", "fable", "index.htmlclay")
	cousin := filepath.Join(home, "work", "codex", "notes.htmlclay")
	writeTestFile(t, page, "<html><body>fable</body></html>")
	writeTestFile(t, cousin, "<html><body>codex</body></html>")

	a := newTestApp(t, home)
	a.rt.confirmOpen = func(string, string) (bool, error) { return true, nil }
	granting, _ := a.openForTest(t, page)
	if err := granting.sessions.GrantReadRoot(filepath.Join(home, "work")); err != nil {
		t.Fatal(err)
	}

	relCousin := filepath.Join("work", "codex", "notes.htmlclay")
	code, body := fetchNav(t, fileURL(granting.port, relCousin))
	if code != 200 || !strings.Contains(body, "htmlclay-banner") {
		t.Fatalf("grant-covered cousin should get the banner: %d", code)
	}
	m := bannerNonceRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no nonce")
	}

	code, out := postSameOrigin(t, fmt.Sprintf("http://127.0.0.1:%d/_/open-request", granting.port),
		"application/json", `{"nonce":"`+m[1]+`"}`)
	if code != 200 {
		t.Fatalf("open-request = %d: %s", code, out)
	}
	var resp struct {
		URL string `json:"url"`
	}
	json.Unmarshal([]byte(out), &resp)
	if strings.Contains(resp.URL, fmt.Sprintf(":%d/", granting.port)) {
		t.Fatalf("grant-only cousin opened on the granting origin: %s", resp.URL)
	}
	if len(a.sites) != 2 {
		t.Fatalf("expected a fresh site for the cousin, have %d", len(a.sites))
	}

	// The granting origin still reads it token-free; the fresh origin serves it
	// editable.
	if _, body := fetch(t, fileURL(granting.port, relCousin)); strings.Contains(body, "htmlclaytoken") {
		t.Fatal("granting origin received the cousin's token")
	}
	if code, body := fetch(t, resp.URL); code != 200 || !strings.Contains(body, "htmlclaytoken") {
		t.Fatalf("fresh origin should serve editable: %d", code)
	}
}

// The whole Feature B promise: inside a declared workspace, following links
// just works — every page editable in place, one site, zero prompts — while a
// silent fetch() still harvests no tokens and registers nothing.
func TestWorkspaceLinksOpenEditableInPlace(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	ws := filepath.Join(home, "thelaunch")
	index := filepath.Join(ws, "index.htmlclay")
	week := filepath.Join(ws, "weeks", "week-02.htmlclay")
	writeTestFile(t, index, "<html><body>launch</body></html>")
	writeTestFile(t, week, "<html><body>week two</body></html>")

	a := newTestApp(t, home)
	var prompts int32
	a.rt.confirm = countingDenyConfirm(&prompts)
	a.rt.confirmOpen = func(string, string) (bool, error) { prompts++; return false, nil }
	a.rt.confirmWorkspace = func(string, string) (bool, error) { prompts++; return false, nil }

	if err := a.addWorkspace(ws); err != nil {
		t.Fatal(err)
	}
	s, _ := a.openForTest(t, index)

	relWeek := filepath.Join("thelaunch", "weeks", "week-02.htmlclay")
	code, body := fetchNav(t, fileURL(s.port, relWeek))
	if code != 200 || !strings.Contains(body, "htmlclaytoken") {
		t.Fatalf("workspace link should serve editable with no prompt: %d", code)
	}
	if strings.Contains(body, "htmlclay-banner") {
		t.Fatal("a workspace page must not carry the read-only banner")
	}
	if len(a.sites) != 1 {
		t.Fatalf("workspace sibling created a site: %d", len(a.sites))
	}
	if via := s.sessions.Via(week); via != session.ViaWorkspace {
		t.Fatalf("provenance = %v, want ViaWorkspace", via)
	}

	tok := tokenAttrRe.FindStringSubmatch(body)
	if tok == nil {
		t.Fatal("no token")
	}
	code, out := postSameOrigin(t, fmt.Sprintf("http://127.0.0.1:%d/_/save/%s", s.port, tok[1]),
		"text/html", "<html><body>week two, edited</body></html>")
	if code != 200 {
		t.Fatalf("workspace save = %d: %s", code, out)
	}
	saved, _ := os.ReadFile(week)
	if !strings.Contains(string(saved), "edited") {
		t.Fatal("save did not reach disk")
	}

	// The worm bound: a silent fetch() of another workspace file gets bytes but
	// no token and no registration.
	other := filepath.Join(ws, "notes.htmlclay")
	writeTestFile(t, other, "<html><body>notes</body></html>")
	code, body = fetchAsSubresource(t, fileURL(s.port, filepath.Join("thelaunch", "notes.htmlclay")))
	if code != 200 {
		t.Fatalf("workspace fetch = %d", code)
	}
	if strings.Contains(body, "htmlclaytoken") {
		t.Fatal("a silent fetch() harvested a token")
	}
	if s.sessions.Via(other) != 0 {
		t.Fatal("a silent fetch() auto-registered a file")
	}

	if atomic.LoadInt32(&prompts) != 0 {
		t.Fatalf("%d prompt(s) fired inside a workspace", prompts)
	}
}

// T0.1's invariant under Feature B: one file, one registration, one origin —
// a second site covering the same workspace redirects to the hosting origin
// instead of minting a second token.
func TestWorkspaceFileNeverRegisteredInTwoSites(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	ws := filepath.Join(home, "ws")
	pageA := filepath.Join(ws, "alpha", "a.htmlclay")
	pageB := filepath.Join(ws, "bravo", "b.htmlclay")
	shared := filepath.Join(ws, "shared.htmlclay")
	writeTestFile(t, pageA, "<html><body>a</body></html>")
	writeTestFile(t, pageB, "<html><body>b</body></html>")
	writeTestFile(t, shared, "<html><body>shared</body></html>")

	a := newTestApp(t, home)
	siteA, _ := a.openForTest(t, pageA)
	siteB, _ := a.openForTest(t, pageB)
	if siteA == siteB {
		t.Fatal("disjoint folders should be separate sites before the workspace exists")
	}
	if err := a.addWorkspace(ws); err != nil {
		t.Fatal(err)
	}

	relShared := filepath.Join("ws", "shared.htmlclay")
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	nav := func(port int) *http.Response {
		req, _ := http.NewRequest("GET", fileURL(port, relShared), nil)
		req.Header.Set("Sec-Fetch-Dest", "document")
		req.Header.Set("Sec-Fetch-User", "?1")
		resp, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	respA := nav(siteA.port)
	respB := nav(siteB.port)
	defer respA.Body.Close()
	defer respB.Body.Close()

	hosts := 0
	for _, s := range a.sites {
		if _, ok := s.sessions.LookupByPath(shared); ok {
			hosts++
		}
	}
	if hosts != 1 {
		t.Fatalf("shared file registered in %d sites, want exactly 1", hosts)
	}

	// One of the two navigations served inline (200), the other redirected to
	// the hosting origin.
	codes := []int{respA.StatusCode, respB.StatusCode}
	sort.Ints(codes)
	if codes[0] != 200 || codes[1] != 302 {
		t.Fatalf("expected one 200 and one 302, got %v", codes)
	}
	var redirected *http.Response
	if respA.StatusCode == 302 {
		redirected = respA
	} else {
		redirected = respB
	}
	loc := redirected.Header.Get("Location")
	if loc == "" {
		t.Fatal("redirect carried no Location")
	}
	if code, body := fetchNav(t, loc); code != 200 || !strings.Contains(body, "htmlclaytoken") {
		t.Fatalf("hosting origin should serve editable: %d", code)
	}
}

// The page-request route end to end: an OS-opened page promotes its own folder
// after the dialog, a page-opened (Feature A) token is refused, and the
// refusal list stops protected folders by identity, not spelling.
func TestWorkspaceRequestFromPagePromotesFolder(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	proj := filepath.Join(home, "proj")
	index := filepath.Join(proj, "index.htmlclay")
	week := filepath.Join(proj, "week.htmlclay")
	writeTestFile(t, index, "<html><body>index</body></html>")
	writeTestFile(t, week, "<html><body>week</body></html>")

	a := newTestApp(t, home)
	wsDialogs := 0
	a.rt.confirmWorkspace = func(title, message string) (bool, error) {
		wsDialogs++
		if !strings.Contains(message, index) || !strings.Contains(message, proj) {
			t.Errorf("dialog must name the requesting file and the full folder: %q", message)
		}
		return true, nil
	}
	s, _ := a.openForTest(t, index)

	code, body := fetch(t, fileURL(s.port, filepath.Join("proj", "index.htmlclay")))
	if code != 200 {
		t.Fatal("index serve failed")
	}
	tok := tokenAttrRe.FindStringSubmatch(body)
	if tok == nil {
		t.Fatal("no token on the OS-opened page")
	}

	code, out := postSameOrigin(t, fmt.Sprintf("http://127.0.0.1:%d/_/workspace-request/%s", s.port, tok[1]), "", "")
	if code != 200 || !strings.Contains(out, `"ok":true`) {
		t.Fatalf("workspace-request = %d: %s", code, out)
	}
	if wsDialogs != 1 {
		t.Fatalf("workspace dialog fired %d times", wsDialogs)
	}
	found := false
	for _, wf := range a.rt.cfg.WorkspaceFolderList() {
		if wf.Path == proj {
			found = true
			if wf.Identity == "" {
				t.Error("workspace stored without an identity fingerprint")
			}
		}
	}
	if !found {
		t.Fatalf("workspace not recorded in config: %v", a.rt.cfg.WorkspaceFolderList())
	}

	// The promoted folder now auto-registers its files.
	code, body = fetchNav(t, fileURL(s.port, filepath.Join("proj", "week.htmlclay")))
	if code != 200 || !strings.Contains(body, "htmlclaytoken") {
		t.Fatal("promoted workspace should auto-register its files")
	}

	// A Feature A token (ViaOpenRequest) cannot drive the workspace route.
	weekTok := tokenAttrRe.FindStringSubmatch(body)
	if s.sessions.Via(week) != session.ViaWorkspace {
		t.Fatalf("week provenance = %v", s.sessions.Via(week))
	}
	code, _ = postSameOrigin(t, fmt.Sprintf("http://127.0.0.1:%d/_/workspace-request/%s", s.port, weekTok[1]), "", "")
	if code != 403 {
		t.Fatalf("non-OS-opened token drove workspace-request: %d", code)
	}
	if wsDialogs != 1 {
		t.Fatal("a refused token still raised a dialog")
	}
}

// The refusal list judges identity, not spelling: a protected folder is caught
// under any casing, an ancestor that would swallow one is caught, and an
// ordinary subfolder inside one stays requestable.
func TestWorkspaceRefusalListByIdentity(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	for _, d := range []string{
		filepath.Join(home, "Documents", "GitHub", "myproj"),
		filepath.Join(home, "Downloads"),
		filepath.Join(home, "projects", "site"),
	} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	a := newTestApp(t, home)

	refused := []string{
		filepath.Join(home, "Documents"),
		filepath.Join(home, "documents"), // casing alias of the same directory
		filepath.Join(home, "Downloads"),
		filepath.Join(home, "Documents", "GitHub"),
		filepath.Join(home, "documents", "github"),
	}
	for _, dir := range refused {
		if !a.isRefusedWorkspace(dir) {
			t.Errorf("%s must be refused", dir)
		}
	}
	allowed := []string{
		filepath.Join(home, "Documents", "GitHub", "myproj"),
		filepath.Join(home, "projects", "site"),
	}
	for _, dir := range allowed {
		if a.isRefusedWorkspace(dir) {
			t.Errorf("%s must stay requestable", dir)
		}
	}

	// The home root itself never reaches the list: canonicalization refuses it.
	if _, err := a.canonicalTrusted(home); err == nil {
		t.Error("home root must not canonicalize as a workspace candidate")
	}

	// The tray route ignores the list: a deliberate picker choice of a
	// protected folder still works.
	if err := a.addWorkspace(filepath.Join(home, "Downloads")); err != nil {
		t.Errorf("tray route must not consult the refusal list: %v", err)
	}
}

// Removing a workspace genuinely ends the capability: auto-registered tokens
// die, files serve read-only again, and an OS-opened file in the same folder
// keeps its session because its provenance survives.
func TestRemoveWorkspaceEndsCapability(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	ws := filepath.Join(home, "ws")
	index := filepath.Join(ws, "index.htmlclay")
	week := filepath.Join(ws, "week.htmlclay")
	writeTestFile(t, index, "<html><body>index</body></html>")
	writeTestFile(t, week, "<html><body>week</body></html>")

	a := newTestApp(t, home)
	if err := a.addWorkspace(ws); err != nil {
		t.Fatal(err)
	}
	s, _ := a.openForTest(t, index)

	relWeek := filepath.Join("ws", "week.htmlclay")
	_, body := fetchNav(t, fileURL(s.port, relWeek))
	tok := tokenAttrRe.FindStringSubmatch(body)
	if tok == nil {
		t.Fatal("workspace file did not auto-register")
	}

	if err := a.removeWorkspace(ws); err != nil {
		t.Fatal(err)
	}

	if _, ok := s.sessions.LookupByPath(week); ok {
		t.Fatal("auto-registered file survived workspace removal")
	}
	code, _ := postSameOrigin(t, fmt.Sprintf("http://127.0.0.1:%d/_/save/%s", s.port, tok[1]),
		"text/html", "<html><body>worm</body></html>")
	if code != 401 {
		t.Fatalf("save after workspace removal = %d, want 401", code)
	}
	if code, body := fetchNav(t, fileURL(s.port, relWeek)); code != 200 || strings.Contains(body, "htmlclaytoken") {
		t.Fatal("removed workspace's file should serve read-only")
	}

	// The OS-opened index keeps everything: its provenance is its own.
	if _, ok := s.sessions.LookupByPath(index); !ok {
		t.Fatal("OS-opened file lost its session on workspace removal")
	}
	if code, body := fetch(t, fileURL(s.port, filepath.Join("ws", "index.htmlclay"))); code != 200 || !strings.Contains(body, "htmlclaytoken") {
		t.Fatal("OS-opened file should still serve editable")
	}

	// Accumulated provenance survives the partial revoke: a file both
	// OS-opened and workspace-covered keeps its registration.
	if err := a.addWorkspace(ws); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := a.route(week, session.ViaOsOpen); !ok {
		t.Fatal("explicit open failed")
	}
	if err := a.removeWorkspace(ws); err != nil {
		t.Fatal(err)
	}
	if via := s.sessions.Via(week); !via.Has(session.ViaOsOpen) {
		t.Fatalf("OS-open provenance lost in workspace removal: %v", via)
	}
	if _, ok := s.sessions.LookupByPath(week); !ok {
		t.Fatal("dual-provenance file lost its registration")
	}
}
