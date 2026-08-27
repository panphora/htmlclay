package main

import (
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/panphora/htmlclay/internal/platform"
)

// requirePortFree proves nothing is listening by successfully taking the address.
// A failed dial cannot prove that: it is equally consistent with a slow local
// stack. Binding is the only positive evidence, so it is what this asserts.
func requirePortFree(t *testing.T, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("port %d is still bound: %v", port, err)
	}
	ln.Close()
}

// failingConfirm fails the test if any native dialog is raised, and counts the
// attempts so the failure message can say how many.
func failingConfirm(t *testing.T, n *int32) func(string, string, bool) (platform.ConfirmChoice, error) {
	return func(string, string, bool) (platform.ConfirmChoice, error) {
		atomic.AddInt32(n, 1)
		t.Error("startup raised a permission dialog; a trusted folder must serve with no prompt")
		return platform.ConfirmDeny, nil
	}
}

// The acceptance test for binding remembered ports at startup. A URL bookmarked
// before the last quit must answer on the first launch after it: the same port,
// the file editable, and not one dialog on the way. Before this, the origin
// moved on every launch and the bookmark broke.
func TestBookmarkedURLSurvivesARestart(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	proj := filepath.Join(home, "proj")
	page := filepath.Join(proj, "index.htmlclay")
	writeTestFile(t, page, "<html><body>index</body></html>")

	cfgBase := t.TempDir()
	first := newTestAppWithConfigDir(t, home, cfgBase)
	if err := first.trustFolder(proj); err != nil {
		t.Fatalf("trust: %v", err)
	}
	s, rel := first.openForTest(t, page)
	bookmark := fileURL(s.port, rel)
	port := s.port
	first.shutdown()

	requirePortFree(t, port)

	second := newTestAppWithConfigDir(t, home, cfgBase)
	var prompts int32
	second.rt.confirm = failingConfirm(t, &prompts)
	second.rt.confirmTrust = func(string, string, string) (bool, error) {
		t.Error("startup raised the trust dialog")
		return false, nil
	}
	second.startSites()

	var relaunched *site
	second.mu.Lock()
	for _, cand := range second.sites {
		if cand.anchor == proj {
			relaunched = cand
		}
	}
	second.mu.Unlock()
	if relaunched == nil {
		t.Fatal("a trusted folder must get its site back at startup")
	}
	if relaunched.port != port {
		t.Fatalf("the origin moved across the restart: port %d, want %d", relaunched.port, port)
	}

	code, body := fetch(t, bookmark)
	if code != 200 {
		t.Fatalf("the bookmarked URL should answer after a restart: %d", code)
	}
	if !strings.Contains(body, "htmlclaytoken") {
		t.Fatal("the bookmarked URL should serve the file with a save token")
	}
	if got := atomic.LoadInt32(&prompts); got != 0 {
		t.Fatalf("%d dialog(s) were raised for a trusted folder", got)
	}
}

// A remembered port for an ordinary (untrusted) folder is bound too, but with
// no capability at all behind it: the recovery page, no registration, and no
// read root anywhere. A bookmark then degrades to a page that explains itself
// rather than a connection refusal, without the port quietly serving files
// nobody asked it to.
func TestRememberedAdHocPortServesOnlyTheRecoveryPage(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	loose := filepath.Join(home, "notes", "loose.htmlclay")
	writeTestFile(t, loose, "<html><body>loose</body></html>")

	cfgBase := t.TempDir()
	first := newTestAppWithConfigDir(t, home, cfgBase)
	s, rel := first.openForTest(t, loose)
	bookmark := fileURL(s.port, rel)
	first.shutdown()

	second := newTestAppWithConfigDir(t, home, cfgBase)
	second.startSites()

	code, body := fetch(t, bookmark)
	if code != 404 {
		t.Fatalf("a parked port must answer 404, got %d", code)
	}
	if !strings.Contains(body, "Nothing is open at this address") {
		t.Fatalf("a parked port must serve the recovery page, got %q", body)
	}
	if strings.Contains(body, "loose") || strings.Contains(body, "htmlclaytoken") {
		t.Fatal("a parked port served the file it once held")
	}

	second.mu.Lock()
	defer second.mu.Unlock()
	for _, cand := range second.sites {
		if _, ok := cand.sessions.LookupByPath(loose); ok {
			t.Error("startup registered a file for an ad-hoc root")
		}
		if _, _, ok := cand.sessions.AssetRoot(loose); ok {
			t.Error("startup installed a read root for an ad-hoc root")
		}
	}
	if len(second.sites) != 0 {
		t.Fatalf("an ad-hoc root must bind no site at startup, got %d", len(second.sites))
	}
}

// A trusted folder whose identity pin no longer matches is left unbound: the
// path may be anything now, and serving it would hand a page whatever tree has
// taken that name.
func TestTrustedFolderWithABrokenPinNeverBinds(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	proj := filepath.Join(home, "proj")
	page := filepath.Join(proj, "index.htmlclay")
	writeTestFile(t, page, "<html><body>index</body></html>")

	cfgBase := t.TempDir()
	first := newTestAppWithConfigDir(t, home, cfgBase)
	if err := first.trustFolder(proj); err != nil {
		t.Fatalf("trust: %v", err)
	}
	s, _ := first.openForTest(t, page)
	port := s.port
	first.shutdown()

	second := newTestAppWithConfigDir(t, home, cfgBase)
	if _, ok := second.rt.cfg.SetTrustedIdentity(proj, "not-the-folder-on-disk"); !ok {
		t.Fatal("no trusted entry to break")
	}
	second.startSites()

	requirePortFree(t, port)
	second.mu.Lock()
	defer second.mu.Unlock()
	if len(second.sites) != 0 {
		t.Fatalf("a dead pin must bind no site, got %d", len(second.sites))
	}
}

// route() takes back the port its own recovery listener is holding. Without the
// unpark, opening a file under a remembered ad-hoc root found the port taken by
// HTML Clay itself, moved the origin, and broke the one bookmark that binding
// at startup exists to preserve.
func TestRouteReclaimsAParkedPort(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	loose := filepath.Join(home, "notes", "loose.htmlclay")
	writeTestFile(t, loose, "<html><body>loose</body></html>")

	cfgBase := t.TempDir()
	first := newTestAppWithConfigDir(t, home, cfgBase)
	s, _ := first.openForTest(t, loose)
	port := s.port
	first.shutdown()

	second := newTestAppWithConfigDir(t, home, cfgBase)
	second.startSites()
	second.mu.Lock()
	parked := len(second.parked)
	second.mu.Unlock()
	if parked != 1 {
		t.Fatalf("expected the remembered port to be parked, got %d parked", parked)
	}

	reopened, rel := second.openForTest(t, loose)
	if reopened.port != port {
		t.Fatalf("route must reclaim the parked port: got %d, want %d", reopened.port, port)
	}
	code, body := fetch(t, fileURL(reopened.port, rel))
	if code != 200 || !strings.Contains(body, "loose") {
		t.Fatalf("the reclaimed port should serve the file: %d, %q", code, body)
	}
}

// Broadest wins: with a folder and a folder inside it both declared, a file
// anchors at the outer one and exactly one site hosts it. One project is one
// origin however many of its subfolders were trusted separately.
func TestNestedTrustedFoldersAnchorAtTheBroadest(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	proj := filepath.Join(home, "proj")
	sub := filepath.Join(proj, "sub")
	page := filepath.Join(sub, "deep.htmlclay")
	writeTestFile(t, page, "<html><body>deep</body></html>")

	a := newTestApp(t, home)
	if err := a.trustFolder(proj); err != nil {
		t.Fatalf("trust proj: %v", err)
	}
	if err := a.trustFolder(sub); err != nil {
		t.Fatalf("trust sub: %v", err)
	}

	anchor, trusted := a.anchorFor(page)
	if !trusted || anchor != proj {
		t.Fatalf("anchorFor = (%q, %v), want (%q, true)", anchor, trusted, proj)
	}

	s, rel := a.openForTest(t, page)
	if s.anchor != proj {
		t.Fatalf("the site should anchor at the broadest trusted folder: got %q", s.anchor)
	}

	hosts := 0
	a.mu.Lock()
	for _, cand := range a.sites {
		if _, ok := cand.sessions.LookupByPath(page); ok {
			hosts++
		}
	}
	a.mu.Unlock()
	if hosts != 1 {
		t.Fatalf("the file is registered in %d sites, want exactly 1", hosts)
	}

	code, body := fetch(t, fileURL(s.port, rel))
	if code != 200 || !strings.Contains(body, "htmlclaytoken") {
		t.Fatalf("the anchoring origin should serve the file editable: %d", code)
	}
}

// startSites binds the broadest of a nested set only. A folder shadowed by a
// broader one still gets its remembered port held, so a bookmark made before
// the broader folder was declared answers with a page rather than nothing.
func TestNestedTrustedFolderIsShadowedButItsPortIsHeld(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	proj := filepath.Join(home, "proj")
	sub := filepath.Join(proj, "sub")
	page := filepath.Join(sub, "deep.htmlclay")
	writeTestFile(t, page, "<html><body>deep</body></html>")

	cfgBase := t.TempDir()
	first := newTestAppWithConfigDir(t, home, cfgBase)
	if err := first.trustFolder(sub); err != nil {
		t.Fatalf("trust sub: %v", err)
	}
	s, rel := first.openForTest(t, page)
	subPort := s.port
	subBookmark := fileURL(subPort, rel)
	if err := first.trustFolder(proj); err != nil {
		t.Fatalf("trust proj: %v", err)
	}
	first.shutdown()

	second := newTestAppWithConfigDir(t, home, cfgBase)
	second.startSites()

	second.mu.Lock()
	anchors := make([]string, 0, len(second.sites))
	for _, cand := range second.sites {
		anchors = append(anchors, cand.anchor)
	}
	second.mu.Unlock()
	if len(anchors) != 1 || anchors[0] != proj {
		t.Fatalf("only the broadest folder should get a site, got %v", anchors)
	}

	code, body := fetch(t, subBookmark)
	if code != 404 || !strings.Contains(body, "Nothing is open at this address") {
		t.Fatalf("the shadowed folder's port should hold the recovery page: %d, %q", code, body)
	}
}

// A file named on the command line after startup routes onto the site that is
// already listening, rather than building a second origin for the same folder.
func TestOpenAfterStartupJoinsTheBoundSite(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	proj := filepath.Join(home, "proj")
	page := filepath.Join(proj, "index.htmlclay")
	other := filepath.Join(proj, "other.htmlclay")
	writeTestFile(t, page, "<html><body>index</body></html>")
	writeTestFile(t, other, "<html><body>other</body></html>")

	cfgBase := t.TempDir()
	first := newTestAppWithConfigDir(t, home, cfgBase)
	if err := first.trustFolder(proj); err != nil {
		t.Fatalf("trust: %v", err)
	}
	s, _ := first.openForTest(t, page)
	port := s.port
	first.shutdown()

	second := newTestAppWithConfigDir(t, home, cfgBase)
	second.startSites()
	before := len(second.sites)

	opened, rel := second.openForTest(t, other)
	if opened.port != port {
		t.Fatalf("an open after startup should join the bound site on %d, got %d", port, opened.port)
	}
	if len(second.sites) != before {
		t.Fatalf("an open after startup built another site: %d, want %d", len(second.sites), before)
	}
	code, body := fetch(t, fileURL(opened.port, rel))
	if code != 200 || !strings.Contains(body, "htmlclaytoken") {
		t.Fatalf("the bound site should serve the newly opened file editable: %d", code)
	}
}
