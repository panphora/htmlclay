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

	if err := siteA.sessions.GrantReadRoot(filepath.Join(home, "work", "review"), false); err != nil {
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
	if err := granting.sessions.GrantReadRoot(filepath.Join(home, "work", "review"), false); err != nil {
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

	if err := s.sessions.GrantReadRoot(filepath.Join(home, "work", "review"), false); err != nil {
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

	if err := siteF.sessions.GrantReadRoot(filepath.Join(home, "review"), false); err != nil {
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
		if err := s.sessions.GrantReadRoot(dir, false); err == nil {
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

func hasFolder(list []string, want string) bool {
	for _, f := range list {
		if f == want {
			return true
		}
	}
	return false
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
