package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
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
	"github.com/panphora/htmlclay/trust"
	"github.com/panphora/htmlclay/versions"
)

// No test may pop a real permission dialog. Every app built here defaults its
// confirm to deny, so a broker that parks an out-of-scope request resolves it
// without ever calling the native osascript prompt. A grant-flow test overrides
// a.rt.confirm before opening the site it wants to allow.
func denyConfirm(string, string, bool) (platform.ConfirmChoice, error) {
	return platform.ConfirmDeny, nil
}

func allowOnceConfirm(string, string, bool) (platform.ConfirmChoice, error) {
	return platform.ConfirmAllowOnce, nil
}

// trustFolderConfirm answers "Trust this folder". It reports Deny when the
// dialog was not allowed to offer that choice, because a dialog with two
// buttons cannot return a third one and a test that ignored allowTrust would
// assert a path the user can never take.
func trustFolderConfirm(_, _ string, allowTrust bool) (platform.ConfirmChoice, error) {
	if !allowTrust {
		return platform.ConfirmDeny, nil
	}
	return platform.ConfirmTrustFolder, nil
}

// countingDenyConfirm denies every prompt and counts how many were raised, so a
// test can assert that a refusal never reached the user at all.
func countingDenyConfirm(n *int32) func(string, string, bool) (platform.ConfirmChoice, error) {
	return func(string, string, bool) (platform.ConfirmChoice, error) {
		atomic.AddInt32(n, 1)
		return platform.ConfirmDeny, nil
	}
}

func newTestApp(t *testing.T, home string) *app {
	t.Helper()
	return newTestAppWithConfigDir(t, home, t.TempDir())
}

// newTestAppWithConfigDir builds an app whose config lives in a caller-chosen
// directory, so two app instances can share one config.json and a restart can
// be simulated.
func newTestAppWithConfigDir(t *testing.T, home, cfgBase string) *app {
	t.Helper()
	logger := logging.NewStdout()
	store := versions.New(t.TempDir())
	ls := server.NewLiveSync(server.SeqPath(store), logger)
	cfg, _, err := config.LoadFrom(cfgBase, platform.DirIdentity)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	configDir := filepath.Join(t.TempDir(), "htmlclay")
	guard := func(dir string) bool {
		return session.EqualOrUnder(dir, configDir) || session.EqualOrUnder(configDir, dir)
	}
	a := &app{
		rt: &appRuntime{
			cfg:       cfg,
			logger:    logger,
			versions:  store,
			ls:        ls,
			home:      home,
			configDir: configDir,
			confirm:   denyConfirm,
			// The write-granting dialog denies by default through its own
			// distinct seam, so a test that allows read grants does not
			// accidentally approve a folder trust.
			confirmTrust: func(string, string, string) (bool, error) { return false, nil },
			// Swallow notifications by default, for the same reason confirm is
			// stubbed: no test may put real UI on the user's screen. A test that
			// cares what the user was told overrides this with a recorder.
			notify: func(string, string) error { return nil },
			guard:  guard,
			policy: trust.Policy{Home: home, Guard: guard},
		},
	}
	t.Cleanup(func() {
		ls.Shutdown()
		a.mu.Lock()
		defer a.mu.Unlock()
		for _, s := range a.sites {
			s.close()
		}
		for _, p := range a.parked {
			p.close()
		}
	})
	return a
}

// hostOf returns the live site holding absPath. Untrusting a folder closes its
// origin and re-homes the files the user opened themselves, so a test that
// checks what survived has to ask where the file lives now rather than reuse
// the site pointer it started with.
func hostOf(t *testing.T, a *app, absPath string) *site {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	s, _, ok := a.lookupLocked(absPath)
	if !ok {
		t.Fatalf("%s is not registered in any live site", absPath)
	}
	return s
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
	a.rt.confirm = func(_ string, message string, _ bool) (platform.ConfirmChoice, error) {
		msgMu.Lock()
		shown = message
		msgMu.Unlock()
		return platform.ConfirmAllowOnce, nil
	}
	s, _ := a.openForTest(t, page)

	if code, _ := fetch(t, fileURL(s.port, filepath.Join("alias", "notes.js"))); code != 200 {
		t.Fatalf("the aliased asset should resume with 200 after Allow, got %d", code)
	}

	granted, _, ok := s.sessions.AssetRoot(filepath.Join(realDir, "notes.js"))
	if !ok {
		t.Fatal("the allowed asset installed no read root")
	}
	msgMu.Lock()
	defer msgMu.Unlock()
	if granted != realDir {
		t.Errorf("the grant must land on the resolved folder: got %q, want %q", granted, realDir)
	}
	// The alias name itself must never become a root of its own, or the folder
	// would be readable under two keys and revocable under only one.
	if _, _, aliased := s.sessions.AssetRoot(filepath.Join(alias, "notes.js")); aliased {
		t.Errorf("the grant must not also land on the alias %q", alias)
	}
	if !strings.Contains(shown, granted) {
		t.Errorf("the dialog must name the folder it grants:\ndialog  = %q\ngranted = %q", shown, granted)
	}
	if strings.Contains(shown, alias) {
		t.Errorf("the dialog must not name the alias the page reached through: %q", shown)
	}
}

// Allowing an out-of-scope asset installs a runtime read grant and the asset
// becomes readable; revoking that root makes it refused again. The confirm
// allows the first prompt and denies afterwards, so the post-revoke re-request
// 403s instead of quietly re-granting.
func TestActiveGrantsListsAndRevokesRuntimeGrants(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	page := filepath.Join(home, "work", "review", "fable", "index.html")
	asset := filepath.Join(home, "work", "review", "shared", "redpen.js")
	writeTestFile(t, page, "<html><body>fable</body></html>")
	writeTestFile(t, asset, "console.log('redpen')")

	var confirmMu sync.Mutex
	allowed := false
	a := newTestApp(t, home)
	a.rt.confirm = func(string, string, bool) (platform.ConfirmChoice, error) {
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
	root, _, ok := s.sessions.AssetRoot(asset)
	if !ok || root != grantDir {
		t.Fatalf("the allowed asset should be covered by a grant at %s: root=%q ok=%v", grantDir, root, ok)
	}

	s.sessions.RevokeReadRoot(grantDir)
	if _, _, ok := s.sessions.AssetRoot(asset); ok {
		t.Fatal("revoking the granted root should leave the asset uncovered")
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
		configDir,                            // exactly the config dir
		filepath.Join(configDir, "versions"), // inside it
		filepath.Join(home, "Library"),       // an ancestor that swallows it
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

// Trusting a folder BELOW an already-open site takes effect immediately: a file
// in it requested from the open site lands editable on the trusted folder's own
// origin, rather than serving read-only with the banner until the user reopens
// something. Anchoring reads the declared list, so there is nothing to seed.
func TestTrustFolderBelowOpenSiteGrantsImmediately(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	proj := filepath.Join(home, "proj")
	top := filepath.Join(proj, "top.htmlclay")
	review := filepath.Join(proj, "review")
	nested := filepath.Join(review, "d.htmlclay")
	writeTestFile(t, top, "<html><body>top</body></html>")
	writeTestFile(t, nested, "<html><body>nested</body></html>")

	a := newTestApp(t, home)
	s, _ := a.openForTest(t, top)

	relNested := filepath.Join("proj", "review", "d.htmlclay")
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	nav := func() *http.Response {
		req, _ := http.NewRequest("GET", fileURL(s.port, relNested), nil)
		req.Header.Set("Sec-Fetch-Dest", "document")
		req.Header.Set("Sec-Fetch-User", "?1")
		resp, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	before := nav()
	beforeBody, _ := io.ReadAll(before.Body)
	before.Body.Close()
	if before.StatusCode != 200 || strings.Contains(string(beforeBody), "htmlclaytoken") {
		t.Fatalf("before the trust the nested file must serve read-only: %d, token=%v",
			before.StatusCode, strings.Contains(string(beforeBody), "htmlclaytoken"))
	}

	if err := a.trustFolder(review); err != nil {
		t.Fatalf("trust: %v", err)
	}

	after := nav()
	defer after.Body.Close()
	if after.StatusCode != 302 {
		body, _ := io.ReadAll(after.Body)
		t.Fatalf("a folder trusted below an open site must take effect at once: got %d, %q",
			after.StatusCode, body)
	}
	loc := after.Header.Get("Location")
	if loc == "" {
		t.Fatal("the redirect carried no Location")
	}
	if strings.Contains(loc, fmt.Sprintf(":%d/", s.port)) {
		t.Fatalf("the trusted folder must own its own origin, not the opening site's: %s", loc)
	}
	code, body := fetchNav(t, loc)
	if code != 200 || !strings.Contains(body, "htmlclaytoken") {
		t.Fatalf("the trusted folder's origin should serve the file editable: %d, token=%v",
			code, strings.Contains(body, "htmlclaytoken"))
	}
}

// Untrusting a folder closes its origin: the config entry goes, the site goes
// with it, and a save through a token that was minted under that trust is
// refused. Anything less leaves a page holding write access to a folder the
// user just took back.
func TestUntrustFolderLiveRevokesFromOpenSite(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	sitesDir := filepath.Join(home, "sites")
	page := filepath.Join(sitesDir, "app", "index.htmlclay")
	writeTestFile(t, page, "<html><body>app</body></html>")

	a := newTestApp(t, home)
	if err := a.trustFolder(sitesDir); err != nil {
		t.Fatalf("trust: %v", err)
	}
	s, rel := a.openForTest(t, page)
	if s.anchor != sitesDir {
		t.Fatalf("a file in a trusted folder should anchor there: got %q", s.anchor)
	}

	code, body := fetch(t, fileURL(s.port, rel))
	if code != 200 {
		t.Fatalf("the trusted page should serve: %d", code)
	}
	tok := tokenAttrRe.FindStringSubmatch(body)
	if tok == nil {
		t.Fatal("no save token on the trusted page")
	}
	saveURL := fmt.Sprintf("http://127.0.0.1:%d/_/save/%s", s.port, tok[1])
	if code, out := postSameOrigin(t, saveURL, "text/html", "<html><body>edited</body></html>"); code != 200 {
		t.Fatalf("save while trusted = %d: %s", code, out)
	}

	if err := a.untrustFolder(sitesDir); err != nil {
		t.Fatalf("untrust: %v", err)
	}

	if hasFolder(a.rt.cfg.TrustedFolderList(), sitesDir) {
		t.Errorf("untrust must drop the folder from config: %v", a.rt.cfg.TrustedFolderList())
	}
	a.mu.Lock()
	stillLive := a.siteAtLocked(sitesDir) != nil
	a.mu.Unlock()
	if stillLive {
		t.Error("untrust must close the folder's origin, not leave its site listening")
	}
	if _, ok := s.sessions.Lookup(tok[1]); ok {
		t.Error("a token minted under the trust survived the untrust")
	}
	if code := postAllowingFailure(t, saveURL, "text/html", "<html><body>worm</body></html>"); code == 200 {
		t.Error("a save through a token minted under the trust must be refused after untrusting")
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

func hasFolder(list []config.TrustedFolder, want string) bool {
	for _, f := range list {
		if f.Path == want {
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
		if hasFolder(a.rt.cfg.TrustedFolderList(), dir) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the prompt's Trust this folder choice must record %s: trusted list = %v", dir, a.rt.cfg.TrustedFolderList())
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
// folder, by asking for an asset that sits directly in ~/Documents, so the
// durable choice is never offered at all: the read is allowed for this session
// and nothing durable is recorded. Wiring the tray's own permissive route here
// instead would trust the whole of Documents in one click.
//
// The refusal is decided BEFORE the dialog is drawn, which is why this asserts
// on the allowTrust flag rather than on a notification: the user is never asked
// to make a choice that could not be honored, so there is nothing to apologize
// for afterwards.
func TestPromptTrustChoiceRefusesAPersonalFolder(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	page := filepath.Join(home, "Documents", "evil", "index.html")
	asset := filepath.Join(home, "Documents", "lib.js")
	writeTestFile(t, page, "<html><body>evil</body></html>")
	writeTestFile(t, asset, "console.log('lib')")

	a := newTestApp(t, home)
	var offered atomic.Bool
	offered.Store(true)
	a.rt.confirm = func(_, _ string, allowTrust bool) (platform.ConfirmChoice, error) {
		offered.Store(allowTrust)
		return platform.ConfirmAllowOnce, nil
	}
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

	if offered.Load() {
		t.Error("a page-chosen personal folder must never be offered as a durable trust")
	}
	if got := a.rt.cfg.TrustedFolderList(); len(got) != 0 {
		t.Errorf("a page-chosen personal folder must never be remembered: %v", got)
	}
	select {
	case msg := <-told:
		t.Errorf("a choice that was never offered needs no apology, got %q", msg)
	case <-time.After(200 * time.Millisecond):
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
	a.rt.confirm = func(_, _ string, allowTrust bool) (platform.ConfirmChoice, error) {
		confirmMu.Lock()
		defer confirmMu.Unlock()
		if !asked && allowTrust {
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
	a.rt.confirm = func(string, string, bool) (platform.ConfirmChoice, error) {
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
	// One prompt installs one root, at the batch's common dir and no broader.
	root, _, ok := s.sessions.AssetRoot(filepath.Join(shared, names[0]))
	if !ok || root != shared {
		t.Errorf("exactly one read root should be installed, at %s: got %q (ok=%v)", shared, root, ok)
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
	a.rt.confirm = func(string, string, bool) (platform.ConfirmChoice, error) {
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
	if root, _, ok := s.sessions.AssetRoot(filepath.Join(shared, names[0])); ok {
		t.Errorf("a denied burst must install no read root, got %q", root)
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

// Untrusting a folder is complete: it drops the folder from the trusted list,
// on disk as well as in memory, AND closes the folder's origin so the tokens
// minted under it die. The two halves must move together, or a stale config
// entry could revive a revoked root on the next open.
func TestUntrustFolderClearsConfigAndRevokesServe(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	sitesDir := filepath.Join(home, "sites")
	page := filepath.Join(sitesDir, "app", "index.htmlclay")
	asset := filepath.Join(sitesDir, "shared", "lib.js")
	writeTestFile(t, page, "<html><body>app</body></html>")
	writeTestFile(t, asset, "console.log('lib')")

	cfgBase := t.TempDir()
	a := newTestAppWithConfigDir(t, home, cfgBase)
	if err := a.trustFolder(sitesDir); err != nil {
		t.Fatalf("trust: %v", err)
	}
	s, rel := a.openForTest(t, page)

	relAsset := filepath.Join("sites", "shared", "lib.js")
	if code, _ := fetch(t, fileURL(s.port, relAsset)); code != 200 {
		t.Fatal("the trusted asset should serve while the folder is trusted")
	}
	if !hasFolder(a.rt.cfg.TrustedFolderList(), sitesDir) {
		t.Fatalf("config should list the trusted folder before untrust: %v", a.rt.cfg.TrustedFolderList())
	}
	_, body := fetch(t, fileURL(s.port, rel))
	tok := tokenAttrRe.FindStringSubmatch(body)
	if tok == nil {
		t.Fatal("no save token on the trusted page")
	}
	saveURL := fmt.Sprintf("http://127.0.0.1:%d/_/save/%s", s.port, tok[1])

	if err := a.untrustFolder(sitesDir); err != nil {
		t.Fatalf("untrust: %v", err)
	}

	if hasFolder(a.rt.cfg.TrustedFolderList(), sitesDir) {
		t.Errorf("untrust must drop the folder from config: %v", a.rt.cfg.TrustedFolderList())
	}
	// The removal must reach disk, not just memory, or a restart revives the
	// revoked root from a stale config entry.
	reloaded, _, err := config.LoadFrom(cfgBase, platform.DirIdentity)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if hasFolder(reloaded.TrustedFolderList(), sitesDir) {
		t.Errorf("untrust must persist to disk: reloaded trusted list still has it: %v", reloaded.TrustedFolderList())
	}
	if code := postAllowingFailure(t, saveURL, "text/html", "<html><body>worm</body></html>"); code == 200 {
		t.Error("untrust must revoke the write capability its trust granted")
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

// postAllowingFailure is postSameOrigin for a request whose origin may be gone.
// Untrusting a folder closes its site, so the refusal a test is checking can
// arrive as a dead port rather than as a status code; 0 stands for that.
func postAllowingFailure(t *testing.T, target, contentType, body string) int {
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
		return 0
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// The whole banner flow on one origin: a linked sibling serves read-only with a
// banner, and the button trusts the file's folder rather than opening the one
// file. Approval records the folder durably and the returned URL serves the
// sibling editable, in place on the same origin, because the opened page's own
// folder is what just became trusted.
func TestOpenBannerFlowOpensSiblingInPlace(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	siteDir := filepath.Join(home, "site")
	page := filepath.Join(siteDir, "index.htmlclay")
	sibling := filepath.Join(siteDir, "week.htmlclay")
	writeTestFile(t, page, "<html><body>index</body></html>")
	writeTestFile(t, sibling, "<html><body>week</body></html>")

	a := newTestApp(t, home)
	trustCalls := 0
	a.rt.confirmTrust = func(title, message, affirmative string) (bool, error) {
		trustCalls++
		if !strings.Contains(message, sibling) {
			t.Errorf("dialog must name the requesting file %s, got %q", sibling, message)
		}
		if !strings.Contains(message, siteDir) {
			t.Errorf("dialog must name the full folder %s, got %q", siteDir, message)
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
	if trustCalls != 1 {
		t.Fatalf("dialog fired %d times, want 1", trustCalls)
	}
	if !hasFolder(a.rt.cfg.TrustedFolderList(), siteDir) {
		t.Fatalf("the banner's button must trust the folder: %v", a.rt.cfg.TrustedFolderList())
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
		t.Fatalf("trusting the opened page's own folder created a new site: %d sites", len(a.sites))
	}
	if via := s.sessions.Via(sibling); !via.Has(session.ViaTrusted) {
		t.Fatalf("sibling provenance = %v, want ViaTrusted", via)
	}

	code, body = fetch(t, resp.URL)
	if code != 200 || !strings.Contains(body, "htmlclaytoken") {
		t.Fatal("the returned URL should serve the sibling with a save token")
	}
	tok := tokenAttrRe.FindStringSubmatch(body)
	if tok == nil {
		t.Fatal("no token in editable serve")
	}
	if code, outSave := postSameOrigin(t, fmt.Sprintf("http://127.0.0.1:%d/_/save/%s", s.port, tok[1]),
		"text/html", "<html><body>week, edited</body></html>"); code != 200 {
		t.Fatalf("the trusted sibling should save: %d %s", code, outSave)
	}
	if _, ok := s.sessions.LookupByPath(page); !ok {
		t.Fatal("trusting the folder killed the OS-opened page's session")
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
	a.rt.confirmTrust = func(string, string, string) (bool, error) { return true, nil }
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

	// The granting origin's OWN answer never carries the cousin's token; the
	// fresh origin serves it editable. Redirects are deliberately not followed:
	// the granting site answering "it lives on that other origin" is the correct
	// behaviour, and following the hop would measure the fresh origin's response
	// while pretending it came from the granting one. In a browser that hop is
	// cross-origin, so the granting page cannot read it.
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	fromGranting, err := noRedirect.Get(fileURL(granting.port, relCousin))
	if err != nil {
		t.Fatal(err)
	}
	grantingBody, _ := io.ReadAll(fromGranting.Body)
	fromGranting.Body.Close()
	if strings.Contains(string(grantingBody), "htmlclaytoken") {
		t.Fatal("granting origin received the cousin's token")
	}
	if _, ok := granting.sessions.LookupByPath(cousin); ok {
		t.Fatal("granting origin received the cousin's token: it was minted there")
	}
	if code, body := fetch(t, resp.URL); code != 200 || !strings.Contains(body, "htmlclaytoken") {
		t.Fatalf("fresh origin should serve editable: %d", code)
	}
	if !hasFolder(a.rt.cfg.TrustedFolderList(), filepath.Join(home, "work", "codex")) {
		t.Fatalf("the banner's button must trust the cousin's own folder: %v", a.rt.cfg.TrustedFolderList())
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
	a.rt.confirmTrust = func(string, string, string) (bool, error) {
		atomic.AddInt32(&prompts, 1)
		return false, nil
	}

	if err := a.trustFolder(ws); err != nil {
		t.Fatal(err)
	}
	s, _ := a.openForTest(t, index)

	relWeek := filepath.Join("thelaunch", "weeks", "week-02.htmlclay")
	code, body := fetchNav(t, fileURL(s.port, relWeek))
	if code != 200 || !strings.Contains(body, "htmlclaytoken") {
		t.Fatalf("trusted-folder link should serve editable with no prompt: %d", code)
	}
	if strings.Contains(body, "htmlclay-banner") {
		t.Fatal("a trusted-folder page must not carry the read-only banner")
	}
	if len(a.sites) != 1 {
		t.Fatalf("trusted-folder sibling created a site: %d", len(a.sites))
	}
	if via := s.sessions.Via(week); via != session.ViaTrusted {
		t.Fatalf("provenance = %v, want ViaTrusted", via)
	}

	tok := tokenAttrRe.FindStringSubmatch(body)
	if tok == nil {
		t.Fatal("no token")
	}
	code, out := postSameOrigin(t, fmt.Sprintf("http://127.0.0.1:%d/_/save/%s", s.port, tok[1]),
		"text/html", "<html><body>week two, edited</body></html>")
	if code != 200 {
		t.Fatalf("trusted-folder save = %d: %s", code, out)
	}
	saved, _ := os.ReadFile(week)
	if !strings.Contains(string(saved), "edited") {
		t.Fatal("save did not reach disk")
	}

	// The worm bound: a silent fetch() of another file in the folder gets bytes
	// but no token and no registration.
	other := filepath.Join(ws, "notes.htmlclay")
	writeTestFile(t, other, "<html><body>notes</body></html>")
	code, body = fetchAsSubresource(t, fileURL(s.port, filepath.Join("thelaunch", "notes.htmlclay")))
	if code != 200 {
		t.Fatalf("trusted-folder fetch = %d", code)
	}
	if strings.Contains(body, "htmlclaytoken") {
		t.Fatal("a silent fetch() harvested a token")
	}
	if s.sessions.Via(other) != 0 {
		t.Fatal("a silent fetch() auto-registered a file")
	}

	if got := atomic.LoadInt32(&prompts); got != 0 {
		t.Fatalf("%d prompt(s) fired inside a trusted folder", got)
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
		t.Fatal("disjoint folders should be separate sites before the trusted folder exists")
	}
	if err := a.trustFolder(ws); err != nil {
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

	// Neither navigation serves the shared file inline: the trusted folder owns
	// its own origin, so both sites redirect there rather than one of them
	// keeping the registration.
	codes := []int{respA.StatusCode, respB.StatusCode}
	if codes[0] != 302 || codes[1] != 302 {
		t.Fatalf("both navigations should redirect to the trusted folder's origin, got %v", codes)
	}
	locA := respA.Header.Get("Location")
	locB := respB.Header.Get("Location")
	if locA == "" {
		t.Fatal("redirect carried no Location")
	}
	if locA != locB {
		t.Fatalf("both redirects must name the one hosting origin: %q vs %q", locA, locB)
	}
	if code, body := fetchNav(t, locA); code != 200 || !strings.Contains(body, "htmlclaytoken") {
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
	a.rt.confirmTrust = func(title, message, affirmative string) (bool, error) {
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
		t.Fatalf("trust dialog fired %d times", wsDialogs)
	}
	found := false
	for _, tf := range a.rt.cfg.TrustedFolderList() {
		if tf.Path == proj {
			found = true
			// The pin must be the folder's own fingerprint, which is what makes a
			// folder deleted and replaced stop granting. Windows produces no cheap
			// lasting fingerprint, so there the pin is empty and the stored path is
			// the whole of the entry's identity; asserting equality states both
			// facts without pretending the Windows one is a bug.
			if want := platform.DirIdentity(proj); tf.Identity != want {
				t.Errorf("trusted folder pin = %q, want the folder's fingerprint %q", tf.Identity, want)
			} else if want == "" && runtime.GOOS != "windows" {
				t.Error("this platform should produce a directory fingerprint")
			}
		}
	}
	if !found {
		t.Fatalf("trusted folder not recorded in config: %v", a.rt.cfg.TrustedFolderList())
	}

	// The trusted folder now auto-registers its files.
	code, body = fetchNav(t, fileURL(s.port, filepath.Join("proj", "week.htmlclay")))
	if code != 200 || !strings.Contains(body, "htmlclaytoken") {
		t.Fatal("a trusted folder should auto-register its files")
	}

	// An auto-registered sibling is already covered, so its ask is answered
	// from the declared list: 200, and no second dialog.
	weekTok := tokenAttrRe.FindStringSubmatch(body)
	if s.sessions.Via(week) != session.ViaTrusted {
		t.Fatalf("week provenance = %v", s.sessions.Via(week))
	}
	code, out = postSameOrigin(t, fmt.Sprintf("http://127.0.0.1:%d/_/workspace-request/%s", s.port, weekTok[1]), "", "")
	if code != 200 || !strings.Contains(out, `"ok":true`) {
		t.Fatalf("covered sibling ask = %d: %s", code, out)
	}
	if wsDialogs != 1 {
		t.Fatal("a covered sibling raised a dialog")
	}
}

// A trusted entry whose folder was deleted and recreated stops granting and
// shows as dead in the tray. Approving the dialog again re-pins it to the folder
// now on disk: without that, the entry stays dead forever and approving the
// dialog never helps, so the page asks on every single load.
//
// FAILING, on purpose, at the re-pin half. handleTrustRequest answers a
// covered file from Hooks.TrustedCovers, which is the app's deliberately
// LEXICAL auto-registration gate and carries no identity check, so a dead entry
// reports itself covered and the request returns {"ok":true} without ever
// reaching Hooks.TrustRequest. The app's own trustFromPage would have asked,
// because it goes through anchorFor and that does check the pin. The two halves
// of the test are what they are today: the entry does stop granting and does
// read as dead in the tray, and a page can no longer re-approve it.
func TestWorkspaceRequestRepinsReplacedFolder(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	proj := filepath.Join(home, "proj")
	index := filepath.Join(proj, "index.htmlclay")
	writeTestFile(t, index, "<html><body>index</body></html>")

	a := newTestApp(t, home)
	wsDialogs := 0
	a.rt.confirmTrust = func(title, message, affirmative string) (bool, error) {
		wsDialogs++
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
	url := fmt.Sprintf("http://127.0.0.1:%d/_/workspace-request/%s", s.port, tok[1])

	if code, out := postSameOrigin(t, url, "", ""); code != 200 {
		t.Fatalf("first trust request = %d: %s", code, out)
	}
	if wsDialogs != 1 {
		t.Fatalf("trust dialog fired %d times", wsDialogs)
	}

	// The folder is replaced on disk: the stored fingerprint no longer matches
	// what is there, so the entry stops granting and reads as dead in the tray.
	// The runtime root goes with it, the way a restart would simply never
	// install a root for an entry that fails its identity check.
	if _, ok := a.rt.cfg.SetTrustedIdentity(proj, "stale-identity"); !ok {
		t.Fatal("no trusted entry to break")
	}
	s.sessions.RevokeTrustedRoot(proj)
	if rows := a.trustedFolderRows(); len(rows) != 1 || !strings.HasSuffix(rows[0].Label, deadFolderSuffix) {
		t.Fatalf("tray rows = %v, want the entry marked dead", rows)
	}

	if code, out := postSameOrigin(t, url, "", ""); code != 200 || !strings.Contains(out, `"ok":true`) {
		t.Fatalf("re-approval trust request = %d: %s", code, out)
	}
	if wsDialogs != 2 {
		t.Fatalf("trust dialog fired %d times; a dead entry must ask again", wsDialogs)
	}
	want := platform.DirIdentity(proj)
	for _, tf := range a.rt.cfg.TrustedFolderList() {
		if tf.Path == proj && tf.Identity != want {
			t.Fatalf("identity = %q, want the folder now on disk (%q)", tf.Identity, want)
		}
	}
	if rows := a.trustedFolderRows(); len(rows) != 1 || strings.HasSuffix(rows[0].Label, deadFolderSuffix) {
		t.Fatalf("tray rows = %v, want the entry live again", rows)
	}

	// Live again, so the next ask is silent.
	if code, out := postSameOrigin(t, url, "", ""); code != 200 || !strings.Contains(out, `"ok":true`) {
		t.Fatalf("ask after the re-pin = %d: %s", code, out)
	}
	if wsDialogs != 2 {
		t.Fatalf("trust dialog fired %d times; a re-pinned entry must not re-prompt", wsDialogs)
	}
}

// deadFolderSuffix is the marker trustedFolderRows appends to an entry whose
// directory is gone or whose identity pin no longer matches.
const deadFolderSuffix = " (missing or replaced)"

// Asking again for a folder that is already trusted grants nothing, so it is
// answered without a dialog. A page that requests its folder on every load must
// raise the prompt once, not once per reload.
func TestWorkspaceRequestAlreadyCoveredSkipsDialog(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	proj := filepath.Join(home, "proj")
	index := filepath.Join(proj, "index.htmlclay")
	writeTestFile(t, index, "<html><body>index</body></html>")

	a := newTestApp(t, home)
	wsDialogs := 0
	a.rt.confirmTrust = func(title, message, affirmative string) (bool, error) {
		wsDialogs++
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
	url := fmt.Sprintf("http://127.0.0.1:%d/_/workspace-request/%s", s.port, tok[1])

	if code, out := postSameOrigin(t, url, "", ""); code != 200 {
		t.Fatalf("first trust request = %d: %s", code, out)
	}
	for i := 0; i < 3; i++ {
		if code, out := postSameOrigin(t, url, "", ""); code != 200 || !strings.Contains(out, `"ok":true`) {
			t.Fatalf("repeat trust request %d = %d: %s", i, code, out)
		}
	}
	if wsDialogs != 1 {
		t.Fatalf("trust dialog fired %d times; a repeat ask must not re-prompt", wsDialogs)
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
		if !a.rt.policy.RefuseOwnFolder(dir) {
			t.Errorf("%s must be refused", dir)
		}
	}
	allowed := []string{
		filepath.Join(home, "Documents", "GitHub", "myproj"),
		filepath.Join(home, "projects", "site"),
	}
	for _, dir := range allowed {
		if a.rt.policy.RefuseOwnFolder(dir) {
			t.Errorf("%s must stay requestable", dir)
		}
	}

	// The home root itself never reaches the list: canonicalization refuses it.
	if _, err := a.rt.policy.Canonical(home); err == nil {
		t.Error("home root must not canonicalize as a trust candidate")
	}

	// The tray route ignores the list: a deliberate picker choice of a
	// protected folder still works.
	if err := a.trustFolder(filepath.Join(home, "Downloads")); err != nil {
		t.Errorf("tray route must not consult the refusal list: %v", err)
	}
}

// Untrusting a folder genuinely ends the capability: auto-registered tokens
// die and those files serve read-only again, while an OS-opened file in the
// same folder keeps its session because a double-click is its own capability.
// The origin closes, so the surviving file is re-homed and asked for by where
// it lives now rather than where it lived before.
func TestRemoveWorkspaceEndsCapability(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	ws := filepath.Join(home, "ws")
	index := filepath.Join(ws, "index.htmlclay")
	week := filepath.Join(ws, "week.htmlclay")
	writeTestFile(t, index, "<html><body>index</body></html>")
	writeTestFile(t, week, "<html><body>week</body></html>")

	a := newTestApp(t, home)
	if err := a.trustFolder(ws); err != nil {
		t.Fatal(err)
	}
	s, _ := a.openForTest(t, index)

	relWeek := filepath.Join("ws", "week.htmlclay")
	_, body := fetchNav(t, fileURL(s.port, relWeek))
	tok := tokenAttrRe.FindStringSubmatch(body)
	if tok == nil {
		t.Fatal("trusted-folder file did not auto-register")
	}

	if err := a.untrustFolder(ws); err != nil {
		t.Fatal(err)
	}

	// The OS-opened index keeps everything: its provenance is its own. It moves
	// to the origin its own folder now anchors.
	host := hostOf(t, a, index)
	if _, ok := host.sessions.LookupByPath(week); ok {
		t.Fatal("the auto-registered file survived the untrust")
	}
	if code := postAllowingFailure(t, fmt.Sprintf("http://127.0.0.1:%d/_/save/%s", host.port, tok[1]),
		"text/html", "<html><body>worm</body></html>"); code == 200 {
		t.Fatal("a token minted under the trust must not save after the untrust")
	}
	if code, body := fetchNav(t, fileURL(host.port, relWeek)); code != 200 || strings.Contains(body, "htmlclaytoken") {
		t.Fatalf("the untrusted folder's file should serve read-only: %d", code)
	}
	if code, body := fetch(t, fileURL(host.port, filepath.Join("ws", "index.htmlclay"))); code != 200 || !strings.Contains(body, "htmlclaytoken") {
		t.Fatal("OS-opened file should still serve editable")
	}

	// Accumulated provenance survives the untrust: a file both OS-opened and
	// trusted-folder-covered keeps its registration.
	if err := a.trustFolder(ws); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := a.route(week, session.ViaOsOpen); !ok {
		t.Fatal("explicit open failed")
	}
	if err := a.untrustFolder(ws); err != nil {
		t.Fatal(err)
	}
	host = hostOf(t, a, week)
	if via := host.sessions.Via(week); !via.Has(session.ViaOsOpen) {
		t.Fatalf("OS-open provenance lost when the folder was untrusted: %v", via)
	}
}

// Opening a file the site already holds must record that open on the existing
// registration. A trusted-folder file the user then double-clicks is held two
// ways, and untrusting the folder must not take away the open the user did.
func TestOpeningAWorkspaceFileRecordsTheOpen(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	proj := filepath.Join(home, "proj")
	index := filepath.Join(proj, "index.htmlclay")
	week := filepath.Join(proj, "week.htmlclay")
	writeTestFile(t, index, "<html><body>index</body></html>")
	writeTestFile(t, week, "<html><body>week</body></html>")

	a := newTestApp(t, home)
	a.rt.confirmTrust = func(string, string, string) (bool, error) { return true, nil }
	s, _ := a.openForTest(t, index)

	code, body := fetch(t, fileURL(s.port, filepath.Join("proj", "index.htmlclay")))
	if code != 200 {
		t.Fatal("index serve failed")
	}
	tok := tokenAttrRe.FindStringSubmatch(body)
	if tok == nil {
		t.Fatal("no token on the OS-opened page")
	}
	if code, out := postSameOrigin(t, fmt.Sprintf("http://127.0.0.1:%d/_/workspace-request/%s", s.port, tok[1]), "", ""); code != 200 || !strings.Contains(out, `"ok":true`) {
		t.Fatalf("trust request = %d: %s", code, out)
	}

	code, body = fetchNav(t, fileURL(s.port, filepath.Join("proj", "week.htmlclay")))
	if code != 200 || !strings.Contains(body, "htmlclaytoken") {
		t.Fatal("a trusted folder should auto-register its files")
	}
	weekTok := tokenAttrRe.FindStringSubmatch(body)
	if weekTok == nil {
		t.Fatal("no token on the auto-registered sibling")
	}
	if via := s.sessions.Via(week); via != session.ViaTrusted {
		t.Fatalf("provenance = %v, want ViaTrusted alone", via)
	}

	// The user now double-clicks a file the site already holds.
	a.openForTest(t, week)
	via := s.sessions.Via(week)
	if !via.Has(session.ViaTrusted) || !via.Has(session.ViaOsOpen) {
		t.Fatalf("provenance = %v, want both ViaTrusted and ViaOsOpen", via)
	}
	f, ok := s.sessions.LookupByPath(week)
	if !ok {
		t.Fatal("the file lost its registration when it was opened again")
	}
	if f.Token != weekTok[1] {
		t.Fatal("opening an already-open file minted a second token")
	}

	if err := a.untrustFolder(proj); err != nil {
		t.Fatal(err)
	}
	// The untrust closes the folder's origin, so the file the user opened
	// themselves is re-homed rather than left where it was. What must survive is
	// the registration and the save, not the address.
	host := hostOf(t, a, week)
	f, ok = host.sessions.LookupByPath(week)
	if !ok {
		t.Fatal("untrusting the folder revoked a file the user had opened themselves: the open never accumulated onto the existing registration")
	}
	code, out := postSameOrigin(t, fmt.Sprintf("http://127.0.0.1:%d/_/save/%s", host.port, f.Token),
		"text/html", "<html><body>week, edited</body></html>")
	if code != 200 {
		t.Fatalf("save after the untrust = %d: %s; the user's own open must keep the file saveable", code, out)
	}
	saved, _ := os.ReadFile(week)
	if !strings.Contains(string(saved), "edited") {
		t.Fatal("save did not reach disk")
	}
}

// The affirmative button is what the user actually clicks, so its label has to
// agree with what clicking it does. v1.3.0 hardcoded "Trust Folder" for every
// dialog confirmTrustRequest raises, including the tray's removal confirm, so
// "Stop trusting this folder?" was answered by clicking a button that said
// Trust Folder while the button that KEPT the trust said Deny. Both labels were
// the opposite of their action, on the app's only destructive action. No test
// saw it because every test injects the seam and none of them looked at the
// label, which is why the seam now carries it.
func TestConfirmLabelsMatchTheirAction(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	proj := filepath.Join(home, "proj")
	file := filepath.Join(proj, "index.htmlclay")
	writeTestFile(t, file, "<html><body>doc</body></html>")

	a := newTestApp(t, home)
	var message, label string
	a.rt.confirmTrust = func(_, msg, affirmative string) (bool, error) {
		message, label = msg, affirmative
		return true, nil
	}

	if _, ok := a.trustFromPage(file, true); !ok {
		t.Fatalf("a page asking to trust its own ordinary folder should be allowed: %q", message)
	}
	if label != "Trust Folder" {
		t.Errorf("the dialog that trusts a folder must say so, got %q with message %q", label, message)
	}

	message, label = "", ""
	a.removeTrustedFolder(proj)
	if label != "Stop Trusting" {
		t.Errorf("the dialog that UNtrusts a folder must not offer to trust it, got %q with message %q", label, message)
	}
	if !strings.Contains(message, "Stop trusting") {
		t.Errorf("the removal confirm should say what it does, got %q", message)
	}
	if hasFolder(a.rt.cfg.TrustedFolderList(), proj) {
		t.Error("approving the removal confirm must actually untrust the folder")
	}
}

// SECURITY.md:34 promises that a file you opened yourself survives an untrust
// "on a new address of their own". The new address is the whole of the
// protection. v1.3.0 kept the folder's remembered port, and the survivor's own
// folder IS the untrusted folder, so it anchored there and bound the very same
// port: every page the trust had auto-registered, still open in a tab, stayed
// same-origin with a file that now holds a fresh save token, and could open it
// and lift that token. Untrusting has to take the address with it.
func TestUntrustMovesTheSurvivorToANewAddress(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	proj := filepath.Join(home, "proj")
	mine := filepath.Join(proj, "mine.htmlclay")
	writeTestFile(t, mine, "<html><body>mine</body></html>")

	a := newTestApp(t, home)
	if err := a.trustFolder(proj); err != nil {
		t.Fatalf("trust: %v", err)
	}
	s, _ := a.openForTest(t, mine)
	before := s.port

	if err := a.untrustFolder(proj); err != nil {
		t.Fatalf("untrust: %v", err)
	}

	if after := hostOf(t, a, mine); after.port == before {
		t.Fatalf("the survivor must leave the untrusted folder's origin, still on port %d", before)
	}
	if remembered := a.rt.cfg.SitePort(proj); remembered == before {
		t.Errorf("the untrusted folder's port must not stay remembered, still %d", remembered)
	}
	// The freed address degrades to the recovery page rather than serving a file.
	code, body := fetchNav(t, fmt.Sprintf("http://127.0.0.1:%d/proj/mine.htmlclay", before))
	if code != 404 || !strings.Contains(body, "Nothing is open at this address") {
		t.Fatalf("the freed port should hold nothing but the recovery page, got %d: %s", code, body)
	}
	if strings.Contains(body, "htmlclaytoken") {
		t.Error("the freed port served a save token")
	}
}

// The untrust rollback has to put back exactly what it took out. Rebuilding the
// entry from the folder on disk re-pins it, so an entry that was DEAD — folder
// deleted and replaced, granting nothing, showing "(missing or replaced)" in the
// tray — comes back as a live grant over whatever is at that path now. That is
// the one thing the identity pin exists to prevent, reached through an error
// path. trustFolder's own rollback twenty lines earlier already gets this right.
func TestUntrustSaveFailureRestoresTheEntryVerbatim(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	proj := filepath.Join(home, "proj")
	writeTestFile(t, filepath.Join(proj, "index.htmlclay"), "<html><body>doc</body></html>")

	cfgBase := t.TempDir()
	a := newTestAppWithConfigDir(t, home, cfgBase)
	if err := a.trustFolder(proj); err != nil {
		t.Fatalf("trust: %v", err)
	}
	// A pin no recomputation could ever produce, standing in for an entry whose
	// folder has since been replaced.
	const deadPin = "pin-of-the-folder-that-used-to-be-here"
	if _, ok := a.rt.cfg.SetTrustedIdentity(proj, deadPin); !ok {
		t.Fatal("could not set the stored pin")
	}
	a.rt.cfg.RememberSitePort(proj, 51515)

	// Fail the next Save on every platform, Windows included: config.json becomes
	// a directory, so the atomic rename onto it cannot succeed. A chmod would be a
	// no-op there.
	path := filepath.Join(config.DirFrom(cfgBase), "config.json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0755); err != nil {
		t.Fatal(err)
	}

	if err := a.untrustFolder(proj); err == nil {
		t.Fatal("untrust must report a config write that did not land")
	}

	var restored config.TrustedFolder
	for _, tf := range a.rt.cfg.TrustedFolderList() {
		if tf.Path == proj {
			restored = tf
		}
	}
	if restored.Path != proj {
		t.Fatalf("a failed untrust must leave the folder trusted: %v", a.rt.cfg.TrustedFolderList())
	}
	if restored.Identity != deadPin {
		t.Errorf("the rollback re-pinned the entry: identity = %q, want the stored %q", restored.Identity, deadPin)
	}
	if got := a.rt.cfg.SitePort(proj); got != 51515 {
		t.Errorf("the rollback must restore the remembered port too, got %d", got)
	}
}

// The recovery page's own instruction has to work when followed.
//
// It says to add the file's folder under Trusted Folders. Until 1.4.0 doing that
// changed nothing until the app was restarted: trustFolder adopted an existing
// site anchored at the folder, and a parked port is not one, so the bookmark kept
// answering with the same page that had just explained how to fix it.
func TestTrustingAParkedFolderMakesItsAddressWorkWithoutARestart(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(home, "proj")
	page := filepath.Join(proj, "index.htmlclay")
	writeTestFile(t, page, "<html><body>index</body></html>")

	cfgBase := t.TempDir()
	first := newTestAppWithConfigDir(t, home, cfgBase)
	s, rel := first.openForTest(t, page)
	bookmark := fileURL(s.port, rel)
	port := s.port
	first.shutdown()

	// The folder was never trusted, so the second launch holds its remembered port
	// with the recovery page rather than a site.
	second := newTestAppWithConfigDir(t, home, cfgBase)
	defer second.shutdown()
	second.startSites()
	if code, _ := fetch(t, bookmark); code != http.StatusNotFound {
		t.Fatalf("precondition: the remembered port should hold the recovery page, got %d", code)
	}

	if err := second.trustFolder(proj); err != nil {
		t.Fatalf("trust: %v", err)
	}

	code, body := fetch(t, bookmark)
	if code != http.StatusOK {
		t.Fatalf("the bookmark must answer after following the page's instruction, got %d", code)
	}
	if !strings.Contains(body, "htmlclaytoken") {
		t.Error("the file should serve editable once its folder is trusted")
	}

	second.mu.Lock()
	defer second.mu.Unlock()
	live := 0
	for _, site := range second.sites {
		if site.anchor == proj {
			live++
			if site.port != port {
				t.Errorf("the origin moved to %d; the whole point of the remembered port is that it does not", site.port)
			}
		}
	}
	if live != 1 {
		t.Errorf("the trusted folder should own exactly one site, got %d", live)
	}
}

// Trusting a folder nested inside one already trusted must not bind a second
// origin for a tree that already has one (invariant 4).
//
// The outer folder keeps anchoring everything under it, so a second site would
// hold a redundant listener and remember a port for a shadowed anchor. startSites
// skips shadowed folders, so after a restart that port would be parked instead:
// the same URL would work now and be dead tomorrow.
func TestTrustingANestedFolderBindsNoSecondSite(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outer := filepath.Join(home, "proj")
	inner := filepath.Join(outer, "sub")
	if err := os.MkdirAll(inner, 0755); err != nil {
		t.Fatal(err)
	}

	a := newTestAppWithConfigDir(t, home, t.TempDir())
	defer a.shutdown()
	if err := a.trustFolder(outer); err != nil {
		t.Fatalf("trust outer: %v", err)
	}
	if err := a.trustFolder(inner); err != nil {
		t.Fatalf("trust inner: %v", err)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	var anchors []string
	for _, s := range a.sites {
		anchors = append(anchors, s.anchor)
	}
	if len(anchors) != 1 || anchors[0] != outer {
		t.Errorf("the outer folder must own the only site, got %v", anchors)
	}
	if got := a.rt.cfg.SitePort(inner); got != 0 {
		t.Errorf("a shadowed folder must not have a remembered port, got %d", got)
	}
}

// A trusted folder covered by a broader one says so in the tray, and its removal
// dialog stops claiming a revocation it will not perform.
//
// Removing the inner entry is a no-op while the outer folder stands: the tree
// still anchors there, and a brand new file created in the inner folder still
// serves with a save token. The dialog nonetheless said files would stop opening
// editable and open pages would need reopening.
func TestAShadowedTrustedFolderIsLabelledAndItsDialogTellsTheTruth(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outer := filepath.Join(home, "proj")
	inner := filepath.Join(outer, "sub")
	if err := os.MkdirAll(inner, 0755); err != nil {
		t.Fatal(err)
	}

	a := newTestAppWithConfigDir(t, home, t.TempDir())
	defer a.shutdown()
	if err := a.trustFolder(outer); err != nil {
		t.Fatalf("trust outer: %v", err)
	}
	if err := a.trustFolder(inner); err != nil {
		t.Fatalf("trust inner: %v", err)
	}

	labels := map[string]string{}
	for _, row := range a.trustedFolderRows() {
		labels[row.Path] = row.Label
	}
	if !strings.Contains(labels[inner], "covered by") {
		t.Errorf("the shadowed row should say what covers it, got %q", labels[inner])
	}
	if strings.Contains(labels[outer], "covered by") {
		t.Errorf("the folder in charge is not covered by anything, got %q", labels[outer])
	}

	var innerMsg, outerMsg string
	a.rt.confirmTrust = func(_, message, _ string) (bool, error) {
		innerMsg = message
		return false, nil
	}
	a.removeTrustedFolder(inner)
	a.rt.confirmTrust = func(_, message, _ string) (bool, error) {
		outerMsg = message
		return false, nil
	}
	a.removeTrustedFolder(outer)

	if strings.Contains(innerMsg, "stop opening editable") {
		t.Errorf("removing a shadowed entry stops nothing; the dialog must not say it does:\n%s", innerMsg)
	}
	if !strings.Contains(innerMsg, outer) {
		t.Errorf("the dialog should name the folder that keeps the files editable:\n%s", innerMsg)
	}
	if !strings.Contains(outerMsg, "stop opening editable") {
		t.Errorf("the control: removing the folder in charge really does revoke:\n%s", outerMsg)
	}
}
