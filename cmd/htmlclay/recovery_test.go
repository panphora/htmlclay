package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/panphora/htmlclay/internal/versions"
)

// navRequest builds the request a browser makes for a bookmarked or typed
// address: a top-level document navigation that belongs to no site yet.
func navRequest(t *testing.T, target string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("request %s: %v", target, err)
	}
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Site", "none")
	return req
}

// browserClient hands back the 302 itself instead of the page at the end of it,
// so a test can assert the relocation the way the browser sees it.
func browserClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func do(t *testing.T, client *http.Client, req *http.Request) (int, http.Header, string) {
	t.Helper()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(body)
}

// recoveryRedirect asserts that a real navigation on a parked port answers 302
// with an empty body, and returns where it points.
func recoveryRedirect(t *testing.T, target string) string {
	t.Helper()
	code, header, body := do(t, browserClient(), navRequest(t, target))
	if code != http.StatusFound {
		t.Fatalf("navigation to %s = %d, want 302", target, code)
	}
	if body != "" {
		t.Fatalf("302 from %s carried a body: %q", target, body)
	}
	return header.Get("Location")
}

// followNav walks navigation redirects the way a browser does, including the
// destination's own auto-registration hop onto the URL it serves.
func followNav(t *testing.T, target string) (int, string) {
	t.Helper()
	for hop := 0; hop < 4; hop++ {
		code, header, body := do(t, browserClient(), navRequest(t, target))
		if code != http.StatusFound {
			return code, body
		}
		next := header.Get("Location")
		if next == "" {
			t.Fatalf("302 from %s carried no Location", target)
		}
		target = next
	}
	t.Fatalf("more than four redirects starting at %s", target)
	return 0, ""
}

func registrations(t *testing.T, a *app, absPath string) int {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, s := range a.sites {
		if _, ok := s.sessions.LookupByPath(absPath); ok {
			n++
		}
	}
	return n
}

func liveSite(t *testing.T, a *app, anchor string) *site {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.siteAtLocked(anchor)
	if s == nil {
		t.Fatalf("no live site anchored at %s", anchor)
	}
	return s
}

// A bookmark made while a child folder was trusted keeps working after an
// enclosing folder is trusted instead: a real navigation on the old address
// redirects to the origin that owns the tree now, which serves the file. The
// retired address serves no content, registers nothing, and leaves everything to
// the destination.
func TestRecoveryNavigationMovesAnOldBookmarkToTheParentsOrigin(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	proj := filepath.Join(home, "proj")
	sub := filepath.Join(proj, "sub")
	page := filepath.Join(sub, "my file.htmlclay")
	writeTestFile(t, page, "<html><body>index</body></html>")

	cfgBase := t.TempDir()
	first := newTestAppWithConfigDir(t, home, cfgBase)
	if err := first.trustFolder(sub); err != nil {
		t.Fatal(err)
	}
	s, rel := first.openForTest(t, page)
	bookmark := fileURL(s.port, rel)
	subPort := s.port
	if err := first.trustFolder(proj); err != nil {
		t.Fatal(err)
	}
	first.shutdown()

	second := newTestAppWithConfigDir(t, home, cfgBase)
	second.startSites()
	t.Cleanup(second.shutdown)

	parent := liveSite(t, second, proj)
	if parent.port == subPort {
		t.Fatal("the parent took the child's port, so the fixture proves nothing")
	}

	// A plain GET is not a navigation and keeps the old answer, exactly as
	// before this change.
	if code, _ := fetch(t, bookmark); code != 404 {
		t.Fatalf("a non-navigation request on a parked port = %d, want 404", code)
	}

	query := "?editmode=true&label=a%20b"
	code, header, body := do(t, browserClient(), navRequest(t, bookmark+query))
	if code != http.StatusFound {
		t.Fatalf("navigation on the retired port = %d, want 302", code)
	}
	want := fileURL(parent.port, rel) + query
	if got := header.Get("Location"); got != want {
		t.Fatalf("Location = %q, want %q", got, want)
	}
	if body != "" {
		t.Fatalf("a relocation must carry no body: %q", body)
	}
	if n := registrations(t, second, page); n != 0 {
		t.Fatalf("the retired port registered the file in %d sites", n)
	}

	code, body = followNav(t, header.Get("Location"))
	if code != 200 || !strings.Contains(body, "savetoken") {
		t.Fatalf("the destination should serve the file editable: %d, %q", code, body)
	}
	if n := registrations(t, second, page); n != 1 {
		t.Fatalf("the destination visit registered the file in %d sites, want 1", n)
	}

	// A file that is not there gets the same answer: the lookup is lexical and
	// the destination is the only place existence is decided.
	missingRel := filepath.Join(filepath.Dir(rel), "missing.htmlclay")
	if got := recoveryRedirect(t, fileURL(subPort, missingRel)); got != fileURL(parent.port, missingRel) {
		t.Fatalf("a missing eligible file relocated to %q, want %q", got, fileURL(parent.port, missingRel))
	}
}

// The shape the Journal bookmark is in: config remembers a port for a folder
// that is no longer in the trusted list, while a folder above it is trusted. The
// old address relocates to the covering origin on the strength of that grant
// alone, without reconstructing any identity for the retired folder.
func TestRecoveryNavigationMovesAnUndeclaredRememberedAnchorToTheTrustedParent(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	proj := filepath.Join(home, "hyper")
	child := filepath.Join(proj, "LOCAL_APPS")
	page := filepath.Join(child, "journal.htmlclay")
	writeTestFile(t, page, "<html><body>journal</body></html>")

	cfgBase := t.TempDir()
	first := newTestAppWithConfigDir(t, home, cfgBase)
	s, rel := first.openForTest(t, page)
	bookmark := fileURL(s.port, rel)
	if err := first.trustFolder(proj); err != nil {
		t.Fatal(err)
	}
	first.shutdown()

	second := newTestAppWithConfigDir(t, home, cfgBase)
	second.startSites()
	t.Cleanup(second.shutdown)

	if second.rt.cfg.SitePort(child) == 0 {
		t.Fatal("the fixture did not remember a port for the child folder")
	}
	for _, tf := range second.rt.cfg.TrustedFolderList() {
		if tf.Path == child {
			t.Fatal("the fixture left the child folder in the trusted list")
		}
	}
	parent := liveSite(t, second, proj)

	if got := recoveryRedirect(t, bookmark); got != fileURL(parent.port, rel) {
		t.Fatalf("the remembered child port relocated to %q, want %q", got, fileURL(parent.port, rel))
	}
	if n := registrations(t, second, page); n != 0 {
		t.Fatalf("the retired port registered the file in %d sites", n)
	}
}

// Trusting the parent while the app is running is enough: the parked address
// begins relocating immediately, with no restart and no trip to Finder.
func TestRecoveryNavigationFollowsTrustAddedWhileRunning(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	proj := filepath.Join(home, "proj")
	child := filepath.Join(proj, "child")
	page := filepath.Join(child, "index.htmlclay")
	writeTestFile(t, page, "<html><body>index</body></html>")

	cfgBase := t.TempDir()
	first := newTestAppWithConfigDir(t, home, cfgBase)
	s, rel := first.openForTest(t, page)
	bookmark := fileURL(s.port, rel)
	first.shutdown()

	second := newTestAppWithConfigDir(t, home, cfgBase)
	second.startSites()
	t.Cleanup(second.shutdown)

	if code, _ := fetch(t, bookmark); code != 404 {
		t.Fatalf("before the parent is trusted the port should hold the recovery page: %d", code)
	}

	if err := second.trustFolder(proj); err != nil {
		t.Fatal(err)
	}
	parent := liveSite(t, second, proj)
	if got := recoveryRedirect(t, bookmark); got != fileURL(parent.port, rel) {
		t.Fatalf("the parked port relocated to %q, want %q", got, fileURL(parent.port, rel))
	}
}

// recoveryTarget is the whole gate, so it is exercised directly: every request
// kind and path that is not an eligible top-level document navigation inside the
// anchor gets no location. No ServeMux sits in front of it here, because a mux
// would clean a traversal path before the gate could refuse it.
func TestRecoveryTargetRefusesAnythingButAnEligibleNavigation(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	proj := filepath.Join(home, "proj")
	sub := filepath.Join(proj, "sub")
	writeTestFile(t, filepath.Join(sub, "index.htmlclay"), "<html><body>index</body></html>")

	a := newTestApp(t, home)
	if err := a.trustFolder(proj); err != nil {
		t.Fatal(err)
	}
	parent := liveSite(t, a, proj)
	parkedPort := parent.port + 1

	// The config dir and the versions store sit inside the anchor here, which is
	// the only way a home-relative request path can name them.
	a.rt.configDir = filepath.Join(sub, "internal")
	a.rt.versions = versions.New(filepath.Join(sub, "versions"))

	host := fmt.Sprintf("127.0.0.1:%d", parkedPort)
	valid := "/proj/sub/index.htmlclay"
	nav := map[string]string{
		"Sec-Fetch-Mode": "navigate",
		"Sec-Fetch-Dest": "document",
		"Sec-Fetch-Site": "none",
	}
	// An empty value removes the header, which is how a browser without fetch
	// metadata and a non-document request are both spelled.
	with := func(over map[string]string) map[string]string {
		out := make(map[string]string, len(nav)+len(over))
		for k, v := range nav {
			out[k] = v
		}
		for k, v := range over {
			if v == "" {
				delete(out, k)
				continue
			}
			out[k] = v
		}
		return out
	}

	cases := []struct {
		name   string
		method string
		target string
		host   string
		header map[string]string
		want   bool
	}{
		{"bookmark navigation", http.MethodGet, valid, host, nav, true},
		{"same-origin user-activated navigation", http.MethodGet, valid, host,
			with(map[string]string{"Sec-Fetch-Site": "same-origin", "Sec-Fetch-User": "?1"}), true},
		{"no fetch metadata", http.MethodGet, valid, host, nil, false},
		{"wrong host", http.MethodGet, valid, fmt.Sprintf("127.0.0.1:%d", parkedPort+1), nav, false},
		{"iframe", http.MethodGet, valid, host, with(map[string]string{"Sec-Fetch-Dest": "iframe"}), false},
		{"fetch", http.MethodGet, valid, host,
			with(map[string]string{"Sec-Fetch-Mode": "no-cors", "Sec-Fetch-Dest": ""}), false},
		{"same-site", http.MethodGet, valid, host, with(map[string]string{"Sec-Fetch-Site": "same-site"}), false},
		{"cross-site", http.MethodGet, valid, host, with(map[string]string{"Sec-Fetch-Site": "cross-site"}), false},
		{"same-origin without user activation", http.MethodGet, valid, host,
			with(map[string]string{"Sec-Fetch-Site": "same-origin"}), false},
		{"POST", http.MethodPost, valid, host, nav, false},
		{"outside the anchor", http.MethodGet, "/proj/other.htmlclay", host, nav, false},
		{"traversal", http.MethodGet, "/proj/sub/../sub/index.htmlclay", host, nav, false},
		{"encoded traversal", http.MethodGet, "/proj/sub/%2e%2e/sub/index.htmlclay", host, nav, false},
		{"reserved route", http.MethodGet, "/_/index.htmlclay", host, nav, false},
		{"hidden path", http.MethodGet, "/proj/sub/.hidden/index.htmlclay", host, nav, false},
		{"config dir", http.MethodGet, "/proj/sub/internal/notes.htmlclay", host, nav, false},
		{"versions dir", http.MethodGet, "/proj/sub/versions/notes-abcd/x.htmlclay", host, nav, false},
		{"wrong extension", http.MethodGet, "/proj/sub/index.txt", host, nav, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, "http://"+host+tc.target, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Host = tc.host
			for k, v := range tc.header {
				req.Header.Set(k, v)
			}
			target, ok := a.recoveryTarget(sub, parkedPort, req)
			if ok != tc.want {
				t.Fatalf("recoveryTarget = (%q, %v), want ok=%v", target, ok, tc.want)
			}
			if ok && target != fileURL(parent.port, valid[1:]) {
				t.Fatalf("target = %q, want %q", target, fileURL(parent.port, valid[1:]))
			}
		})
	}
}

// A parked port only relocates when the destination can actually serve: a source
// whose own pin is dead, a destination pin that failed after startup, a
// destination that exists only because a file was opened there, a quitting app,
// and a target on the same port all keep an appropriate recovery page.
func TestRecoveryNavigationRefusesWhenTheDestinationCannotServe(t *testing.T) {
	t.Run("dead source pin", func(t *testing.T) {
		home, _ := filepath.EvalSymlinks(t.TempDir())
		proj := filepath.Join(home, "proj")
		sub := filepath.Join(proj, "sub")
		page := filepath.Join(sub, "index.htmlclay")
		writeTestFile(t, page, "<html><body>index</body></html>")

		cfgBase := t.TempDir()
		first := newTestAppWithConfigDir(t, home, cfgBase)
		if err := first.trustFolder(sub); err != nil {
			t.Fatal(err)
		}
		s, rel := first.openForTest(t, page)
		bookmark := fileURL(s.port, rel)
		if err := first.trustFolder(proj); err != nil {
			t.Fatal(err)
		}
		first.shutdown()

		second := newTestAppWithConfigDir(t, home, cfgBase)
		if _, ok := second.rt.cfg.SetTrustedIdentity(sub, "not-the-folder-on-disk"); !ok {
			t.Fatal("no trusted entry to break")
		}
		second.startSites()
		t.Cleanup(second.shutdown)
		liveSite(t, second, proj)

		code, header, body := do(t, browserClient(), navRequest(t, bookmark))
		if code != 404 || header.Get("Location") != "" {
			t.Fatalf("a dead source pin relocated: %d, %q", code, header.Get("Location"))
		}
		if body != string(deadFolderPage) {
			t.Fatalf("expected the dead-folder approval page, got %q", body)
		}
		if n := registrations(t, second, page); n != 0 {
			t.Fatalf("a dead source pin registered the file in %d sites", n)
		}
	})

	t.Run("dead destination pin", func(t *testing.T) {
		home, _ := filepath.EvalSymlinks(t.TempDir())
		proj := filepath.Join(home, "proj")
		child := filepath.Join(proj, "child")
		page := filepath.Join(child, "index.htmlclay")
		writeTestFile(t, page, "<html><body>index</body></html>")

		cfgBase := t.TempDir()
		first := newTestAppWithConfigDir(t, home, cfgBase)
		s, rel := first.openForTest(t, page)
		bookmark := fileURL(s.port, rel)
		if err := first.trustFolder(proj); err != nil {
			t.Fatal(err)
		}
		first.shutdown()

		second := newTestAppWithConfigDir(t, home, cfgBase)
		second.startSites()
		t.Cleanup(second.shutdown)
		parent := liveSite(t, second, proj)
		if got := recoveryRedirect(t, bookmark); got != fileURL(parent.port, rel) {
			t.Fatalf("setup: the parked port relocated to %q", got)
		}
		if _, ok := second.rt.cfg.SetTrustedIdentity(proj, "not-the-folder-on-disk"); !ok {
			t.Fatal("no trusted entry to break")
		}

		code, header, _ := do(t, browserClient(), navRequest(t, bookmark))
		if code != 404 || header.Get("Location") != "" {
			t.Fatalf("a destination whose pin failed must not be used: %d, %q", code, header.Get("Location"))
		}
	})

	t.Run("destination only from an OS open", func(t *testing.T) {
		home, _ := filepath.EvalSymlinks(t.TempDir())
		proj := filepath.Join(home, "proj")
		sub := filepath.Join(proj, "sub")
		page := filepath.Join(sub, "index.htmlclay")
		other := filepath.Join(proj, "other.htmlclay")
		writeTestFile(t, page, "<html><body>index</body></html>")
		writeTestFile(t, other, "<html><body>other</body></html>")

		cfgBase := t.TempDir()
		first := newTestAppWithConfigDir(t, home, cfgBase)
		s, rel := first.openForTest(t, page)
		bookmark := fileURL(s.port, rel)
		first.openForTest(t, other)
		first.shutdown()

		second := newTestAppWithConfigDir(t, home, cfgBase)
		second.startSites()
		t.Cleanup(second.shutdown)
		opened, _ := second.openForTest(t, other)
		if opened.anchor != proj || opened.trusted {
			t.Fatalf("the destination should exist only because a file was opened there: %+v", opened)
		}

		code, header, _ := do(t, browserClient(), navRequest(t, bookmark))
		if code != 404 || header.Get("Location") != "" {
			t.Fatalf("an untrusted destination must not be a target: %d, %q", code, header.Get("Location"))
		}
	})

	t.Run("stopping", func(t *testing.T) {
		home, _ := filepath.EvalSymlinks(t.TempDir())
		proj := filepath.Join(home, "proj")
		child := filepath.Join(proj, "child")
		page := filepath.Join(child, "index.htmlclay")
		writeTestFile(t, page, "<html><body>index</body></html>")

		cfgBase := t.TempDir()
		first := newTestAppWithConfigDir(t, home, cfgBase)
		s, rel := first.openForTest(t, page)
		bookmark := fileURL(s.port, rel)
		if err := first.trustFolder(proj); err != nil {
			t.Fatal(err)
		}
		first.shutdown()

		second := newTestAppWithConfigDir(t, home, cfgBase)
		second.startSites()
		t.Cleanup(second.shutdown)
		second.mu.Lock()
		second.stopping = true
		second.mu.Unlock()

		code, header, _ := do(t, browserClient(), navRequest(t, bookmark))
		if code != 404 || header.Get("Location") != "" {
			t.Fatalf("a quitting app must not relocate: %d, %q", code, header.Get("Location"))
		}
	})

	t.Run("target on the same port", func(t *testing.T) {
		home, _ := filepath.EvalSymlinks(t.TempDir())
		proj := filepath.Join(home, "proj")
		sub := filepath.Join(proj, "sub")
		writeTestFile(t, filepath.Join(sub, "index.htmlclay"), "<html><body>index</body></html>")

		a := newTestApp(t, home)
		if err := a.trustFolder(proj); err != nil {
			t.Fatal(err)
		}
		parent := liveSite(t, a, proj)

		req := navRequest(t, fileURL(parent.port, filepath.Join("proj", "sub", "index.htmlclay")))
		if target, ok := a.recoveryTarget(sub, parent.port, req); ok {
			t.Fatalf("a target on the same port relocated to %q", target)
		}
	})
}

// Untrusting a folder is a revocation, not a relocation. Its freed port keeps
// the recovery page even though a trusted parent still covers the file, so an
// old address cannot quietly revive what the user took back.
func TestRecoveryNavigationDoesNotReviveAnUntrustedPort(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	proj := filepath.Join(home, "proj")
	sub := filepath.Join(proj, "sub")
	page := filepath.Join(sub, "index.htmlclay")
	writeTestFile(t, page, "<html><body>index</body></html>")

	cfgBase := t.TempDir()
	first := newTestAppWithConfigDir(t, home, cfgBase)
	if err := first.trustFolder(sub); err != nil {
		t.Fatal(err)
	}
	s, rel := first.openForTest(t, page)
	bookmark := fileURL(s.port, rel)
	if err := first.trustFolder(proj); err != nil {
		t.Fatal(err)
	}
	first.shutdown()

	second := newTestAppWithConfigDir(t, home, cfgBase)
	second.startSites()
	t.Cleanup(second.shutdown)

	if got := recoveryRedirect(t, bookmark); got == "" {
		t.Fatal("setup: a merely shadowed folder's port should relocate")
	}
	if err := second.untrustFolder(sub); err != nil {
		t.Fatal(err)
	}

	code, header, body := do(t, browserClient(), navRequest(t, bookmark))
	if code != 404 || header.Get("Location") != "" {
		t.Fatalf("a revoked port relocated: %d, %q", code, header.Get("Location"))
	}
	if strings.Contains(body, "savetoken") {
		t.Fatalf("a revoked port served the file: %q", body)
	}
	if !strings.Contains(body, "Nothing is open at this address") {
		t.Fatalf("expected the recovery page, got %q", body)
	}
	if n := registrations(t, second, page); n != 0 {
		t.Fatalf("a revoked port registered the file in %d sites", n)
	}
}

// Trusting a parent while a child's page is open does not move that page. Its
// token and its origin stay, and the file stays registered in exactly one site:
// a live tab's unsaved work is never migrated behind the user's back.
func TestRecoveryLeavesALiveChildRegistrationAloneWhenAParentIsTrusted(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	proj := filepath.Join(home, "proj")
	sub := filepath.Join(proj, "sub")
	page := filepath.Join(sub, "index.htmlclay")
	writeTestFile(t, page, "<html><body>index</body></html>")

	a := newTestApp(t, home)
	if err := a.trustFolder(sub); err != nil {
		t.Fatal(err)
	}
	s, rel := a.openForTest(t, page)
	f, ok := s.sessions.LookupByPath(page)
	if !ok {
		t.Fatal("the child site did not register the file")
	}
	token := f.Token

	if err := a.trustFolder(proj); err != nil {
		t.Fatal(err)
	}

	if host := hostOf(t, a, page); host != s {
		t.Fatal("trusting the parent moved the live registration to another site")
	}
	again, ok := s.sessions.LookupByPath(page)
	if !ok {
		t.Fatal("the child site lost the registration")
	}
	if again.Token != token {
		t.Fatalf("the child's token changed: %q -> %q", token, again.Token)
	}
	if n := registrations(t, a, page); n != 1 {
		t.Fatalf("the file is registered in %d sites, want exactly 1", n)
	}
	code, body := fetch(t, fileURL(s.port, rel))
	if code != 200 || !strings.Contains(body, `savetoken="`+token+`"`) {
		t.Fatalf("the child's own origin should keep serving its page: %d, %q", code, body)
	}
}

// A folder that failed its identity check may relocate only after native approval
// has pinned the folder on disk again. Re-approving it while a trusted parent
// covers it does not build a second origin over the parent's tree.
func TestRecoveryDeadChildRelocatesOnlyAfterItsPinIsRestored(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	proj := filepath.Join(home, "proj")
	child := filepath.Join(proj, "child")
	page := filepath.Join(child, "index.htmlclay")
	writeTestFile(t, page, "<html><body>index</body></html>")

	cfgBase := t.TempDir()
	first := newTestAppWithConfigDir(t, home, cfgBase)
	if err := first.trustFolder(child); err != nil {
		t.Fatal(err)
	}
	s, rel := first.openForTest(t, page)
	childPort := s.port
	bookmark := fileURL(childPort, rel)
	if err := first.trustFolder(proj); err != nil {
		t.Fatal(err)
	}
	first.shutdown()

	second := newTestAppWithConfigDir(t, home, cfgBase)
	if _, ok := second.rt.cfg.SetTrustedIdentity(child, "not-the-folder-on-disk"); !ok {
		t.Fatal("no trusted entry to break")
	}
	second.startSites()
	t.Cleanup(second.shutdown)

	code, header, _ := do(t, browserClient(), navRequest(t, bookmark))
	if code != http.StatusNotFound || header.Get("Location") != "" {
		t.Fatalf("a dead source pin relocated: %d, %q", code, header.Get("Location"))
	}

	if err := second.trustFolder(child); err != nil {
		t.Fatal(err)
	}
	second.mu.Lock()
	childLive := second.siteAtLocked(child) != nil
	second.mu.Unlock()
	if childLive {
		t.Fatal("a child covered by a trusted parent must not get its own site")
	}
	if n := registrations(t, second, page); n != 0 {
		t.Fatalf("re-approving the child registered the file in %d sites", n)
	}

	parent := liveSite(t, second, proj)
	if parent.port == childPort {
		t.Fatal("the parent took the child's port, so the fixture proves nothing")
	}
	if got := recoveryRedirect(t, bookmark); got != fileURL(parent.port, rel) {
		t.Fatalf("the repaired port relocated to %q, want %q", got, fileURL(parent.port, rel))
	}
	if n := registrations(t, second, page); n != 0 {
		t.Fatalf("relocating registered the file in %d sites", n)
	}
	code, body := followNav(t, fileURL(parent.port, rel))
	if code != 200 || !strings.Contains(body, "savetoken") {
		t.Fatalf("the destination should serve the file editable: %d, %q", code, body)
	}
}

// Re-trusting a folder the user explicitly revoked does not undo the revocation
// for the address that was revoked: the port stays fixed on the recovery page
// while the parent's own origin keeps serving the file.
func TestRecoveryNavigationDoesNotReviveARetrustedChild(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	proj := filepath.Join(home, "proj")
	child := filepath.Join(proj, "child")
	page := filepath.Join(child, "index.htmlclay")
	writeTestFile(t, page, "<html><body>index</body></html>")

	cfgBase := t.TempDir()
	first := newTestAppWithConfigDir(t, home, cfgBase)
	if err := first.trustFolder(child); err != nil {
		t.Fatal(err)
	}
	s, rel := first.openForTest(t, page)
	childPort := s.port
	bookmark := fileURL(childPort, rel)
	if err := first.trustFolder(proj); err != nil {
		t.Fatal(err)
	}
	first.shutdown()

	second := newTestAppWithConfigDir(t, home, cfgBase)
	second.startSites()
	t.Cleanup(second.shutdown)

	parent := liveSite(t, second, proj)
	if parent.port == childPort {
		t.Fatal("the parent took the child's port, so the fixture proves nothing")
	}
	if got := recoveryRedirect(t, bookmark); got == "" {
		t.Fatal("setup: a merely shadowed folder's port should relocate")
	}

	if err := second.untrustFolder(child); err != nil {
		t.Fatal(err)
	}
	if err := second.trustFolder(child); err != nil {
		t.Fatal(err)
	}

	code, header, body := do(t, browserClient(), navRequest(t, bookmark))
	if code != http.StatusNotFound || header.Get("Location") != "" {
		t.Fatalf("a revoked port relocated: %d, %q", code, header.Get("Location"))
	}
	if strings.Contains(body, "savetoken") {
		t.Fatalf("a revoked port served the file: %q", body)
	}
	code, body = followNav(t, fileURL(parent.port, rel))
	if code != 200 || !strings.Contains(body, "savetoken") {
		t.Fatalf("the trusted parent should still serve the file editable: %d, %q", code, body)
	}
}

// Removing a folder must hand its surviving trusted children their own origins at
// once: their bookmarks have to work again without an OS open, and the port the
// removed folder held must not become a way back into the tree.
func TestRecoveryUntrustingAParentBringsItsShadowedChildLive(t *testing.T) {
	fixture := func(t *testing.T) (home, proj, child, page, rel, cfgBase string, childPort int, bookmark string) {
		t.Helper()
		home, _ = filepath.EvalSymlinks(t.TempDir())
		proj = filepath.Join(home, "proj")
		child = filepath.Join(proj, "child")
		page = filepath.Join(child, "index.htmlclay")
		writeTestFile(t, page, "<html><body>index</body></html>")

		cfgBase = t.TempDir()
		first := newTestAppWithConfigDir(t, home, cfgBase)
		if err := first.trustFolder(child); err != nil {
			t.Fatal(err)
		}
		s, rel := first.openForTest(t, page)
		childPort = s.port
		bookmark = fileURL(childPort, rel)
		if err := first.trustFolder(proj); err != nil {
			t.Fatal(err)
		}
		first.shutdown()
		return home, proj, child, page, rel, cfgBase, childPort, bookmark
	}

	t.Run("valid child", func(t *testing.T) {
		home, proj, child, page, rel, cfgBase, childPort, bookmark := fixture(t)

		second := newTestAppWithConfigDir(t, home, cfgBase)
		second.startSites()
		t.Cleanup(second.shutdown)

		parent := liveSite(t, second, proj)
		if parent.port == childPort {
			t.Fatal("the parent took the child's port, so the fixture proves nothing")
		}
		parentPort := parent.port
		if code, _ := fetch(t, bookmark); code != 404 {
			t.Fatalf("setup: the shadowed child's port should hold the recovery page, got %d", code)
		}

		if err := second.untrustFolder(proj); err != nil {
			t.Fatal(err)
		}

		childSite := liveSite(t, second, child)
		if childSite.port != childPort {
			t.Fatalf("the surviving child bound port %d, want its remembered %d", childSite.port, childPort)
		}
		if n := registrations(t, second, page); n != 0 {
			t.Fatalf("bringing the child live registered the file in %d sites", n)
		}
		code, body := followNav(t, bookmark)
		if code != 200 || !strings.Contains(body, "savetoken") {
			t.Fatalf("the bookmark should serve the file editable: %d, %q", code, body)
		}

		code, header, _ := do(t, browserClient(), navRequest(t, fileURL(parentPort, rel)))
		if code != 404 || header.Get("Location") != "" {
			t.Fatalf("the untrusted parent's port relocated: %d, %q", code, header.Get("Location"))
		}
	})

	t.Run("broken child pin", func(t *testing.T) {
		home, proj, child, _, _, cfgBase, _, bookmark := fixture(t)

		second := newTestAppWithConfigDir(t, home, cfgBase)
		if _, ok := second.rt.cfg.SetTrustedIdentity(child, "not-the-folder-on-disk"); !ok {
			t.Fatal("no trusted entry to break")
		}
		second.startSites()
		t.Cleanup(second.shutdown)
		liveSite(t, second, proj)

		if err := second.untrustFolder(proj); err != nil {
			t.Fatal(err)
		}

		second.mu.Lock()
		childLive := second.siteAtLocked(child) != nil
		second.mu.Unlock()
		if childLive {
			t.Fatal("a child whose identity pin is broken must not be brought live")
		}
		code, header, body := do(t, browserClient(), navRequest(t, bookmark))
		if code != 404 || header.Get("Location") != "" {
			t.Fatalf("a broken child's port relocated: %d, %q", code, header.Get("Location"))
		}
		if strings.Contains(body, "savetoken") {
			t.Fatalf("a broken child's port served the file: %q", body)
		}
	})
}

// A dead or revoked source has to declare itself: a covering parent's grant is
// not evidence that the address the bookmark names still stands for a folder the
// user approved. An ordinary remembered anchor keeps relocating on that grant
// alone, which is what the undeclared Journal anchor relies on.
func TestRecoveryTargetForRequiresTheAnchorToDeclareItself(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	proj := filepath.Join(home, "proj")
	child := filepath.Join(proj, "child")
	writeTestFile(t, filepath.Join(child, "index.htmlclay"), "<html><body>index</body></html>")

	a := newTestApp(t, home)
	if err := a.trustFolder(proj); err != nil {
		t.Fatal(err)
	}
	parent := liveSite(t, a, proj)
	parkedPort := parent.port + 1
	rel := filepath.Join("proj", "child", "index.htmlclay")
	req := navRequest(t, fileURL(parkedPort, rel))

	if target, ok := a.recoveryTargetFor(child, parkedPort, req, true); ok {
		t.Fatalf("an undeclared anchor relocated to %q", target)
	}
	want := fileURL(parent.port, rel)
	if target, ok := a.recoveryTargetFor(child, parkedPort, req, false); !ok || target != want {
		t.Fatalf("an ordinary relocation of the same anchor = (%q, %v), want (%q, true)", target, ok, want)
	}
	if target, ok := a.recoveryTarget(child, parkedPort, req); !ok || target != want {
		t.Fatalf("recoveryTarget = (%q, %v), want (%q, true)", target, ok, want)
	}
}

// A bookmark can name a file whose literal name contains a percent escape. The
// recovery redirect receives the already decoded request path, so the destination
// has to escape it exactly once: treating it as an escaped path decodes "%20" a
// second time into a space and points the browser at a different document.
func TestRecoveryPreservesLiteralPercentInFilename(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	proj := filepath.Join(home, "proj")
	child := filepath.Join(proj, "child")
	rel := "/proj/child/report%20one.htmlclay"
	writeTestFile(t, filepath.Join(home, rel[1:]), "<html><body>literal percent filename</body></html>")

	a := newTestApp(t, home)
	if err := a.trustFolder(proj); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	a.parkPort(child, port)

	// Built independently of fileURL, the way a browser remembers an address it
	// was handed: the escape belongs to the literal filename.
	bookmark := (&url.URL{
		Scheme:   "http",
		Host:     fmt.Sprintf("127.0.0.1:%d", port),
		Path:     rel,
		RawQuery: "editmode=false",
	}).String()
	code, headers, _ := do(t, browserClient(), navRequest(t, bookmark))
	if code != http.StatusFound {
		t.Fatalf("expected redirect, got %d", code)
	}
	destination, err := url.Parse(headers.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if destination.Path != rel {
		t.Errorf("redirect changed filename: got %q, want %q", destination.Path, rel)
	}
	if destination.RawQuery != "editmode=false" {
		t.Errorf("redirect changed the query: got %q", destination.RawQuery)
	}
}

// Recovery always targets the broadest trusted site's origin, but a document can
// still be registered on a live child's origin. The second redirect, back to that
// child, has to carry the request's raw query too, or the bookmark loses editmode
// and any app parameter on the way. The child keeps its one registration and token.
func TestRecoveryPreservesQueryThroughExistingChild(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	proj := filepath.Join(home, "proj")
	child := filepath.Join(proj, "child")
	grand := filepath.Join(child, "grand")
	page := filepath.Join(grand, "page.htmlclay")
	writeTestFile(t, page, "<html><body>fixture</body></html>")

	cfgBase := t.TempDir()
	first := newTestAppWithConfigDir(t, home, cfgBase)
	old, rel := first.openForTest(t, page)
	bookmark := fileURL(old.port, rel) + "?editmode=false&label=a%20b"
	first.shutdown()

	second := newTestAppWithConfigDir(t, home, cfgBase)
	second.startSites()
	t.Cleanup(second.shutdown)
	if err := second.trustFolder(child); err != nil {
		t.Fatal(err)
	}
	liveChild, _ := second.openForTest(t, page)
	registered, ok := liveChild.sessions.LookupByPath(page)
	if !ok {
		t.Fatal("the child site did not register the document")
	}
	token := registered.Token
	if err := second.trustFolder(proj); err != nil {
		t.Fatal(err)
	}

	firstHop := recoveryRedirect(t, bookmark)
	code, headers, _ := do(t, browserClient(), navRequest(t, firstHop))
	if code != http.StatusFound {
		t.Fatalf("expected redirect to existing child, got %d", code)
	}
	secondHop := headers.Get("Location")
	parsed, err := url.Parse(secondHop)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.RawQuery != "editmode=false&label=a%20b" {
		t.Errorf("query lost on second hop: %q", parsed.RawQuery)
	}
	if n := registrations(t, second, page); n != 1 {
		t.Errorf("registration count=%d", n)
	}
	again, ok := liveChild.sessions.LookupByPath(page)
	if !ok || again.Token != token {
		t.Errorf("the live child's registration changed: %+v, %v", again, ok)
	}
}

func TestRecoveryNavigationUsesTrustForTheRequestedFile(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	apps := filepath.Join(home, "apps")
	project := filepath.Join(apps, "search")
	index := filepath.Join(apps, "index.htmlclay")
	page := filepath.Join(project, "search.htmlclay")
	sibling := filepath.Join(project, "other.htmlclay")
	outside := filepath.Join(apps, "outside.htmlclay")
	for _, path := range []string{index, page, sibling, outside} {
		writeTestFile(t, path, "<html><body>fixture</body></html>")
	}
	configDir := t.TempDir()
	first := newTestAppWithConfigDir(t, home, configDir)
	initial, _ := first.openForTest(t, index)
	opened, _ := first.openForTest(t, page)
	if opened != initial {
		t.Fatal("fixture did not use the broader ad hoc origin")
	}
	oldPort := opened.port
	if err := first.trustFolder(project); err != nil {
		t.Fatal(err)
	}
	first.shutdown()
	current := newTestAppWithConfigDir(t, home, configDir)
	current.startSites()
	target := liveSite(t, current, project)
	query := "editmode=false&label=a%20b&literal=%2523"
	for _, path := range []string{page, sibling, filepath.Join(project, "missing.htmlclay")} {
		rel, err := filepath.Rel(current.rt.home, path)
		if err != nil {
			t.Fatal(err)
		}
		code, headers, body := do(t, browserClient(), navRequest(t, fileURL(oldPort, rel)+"?"+query))
		if code != http.StatusFound || body != "" {
			t.Fatalf("recovery for %s: %d %q", path, code, body)
		}
		want := fileURL(target.port, rel) + "?" + query
		if headers.Get("Location") != want {
			t.Fatalf("redirect %q, want %q", headers.Get("Location"), want)
		}
		if registrations(t, current, path) != 0 {
			t.Fatal("recovery registered a document")
		}
	}
	outsideRel, err := filepath.Rel(current.rt.home, outside)
	if err != nil {
		t.Fatal(err)
	}
	code, headers, _ := do(t, browserClient(), navRequest(t, fileURL(oldPort, outsideRel)))
	if code != http.StatusNotFound || headers.Get("Location") != "" {
		t.Fatal("untrusted sibling relocated")
	}
	pageRel, err := filepath.Rel(current.rt.home, page)
	if err != nil {
		t.Fatal(err)
	}
	code, _, body := do(t, browserClient(), navRequest(t, fileURL(target.port, pageRel)+"?"+query))
	if code != http.StatusOK || !strings.Contains(body, "savetoken") || registrations(t, current, page) != 1 {
		t.Fatalf("destination: %d %q", code, body)
	}
	if _, ok := current.rt.cfg.SetTrustedIdentity(project, "invalid-fixture-pin"); !ok {
		t.Fatal("missing trust entry")
	}
	code, headers, _ = do(t, browserClient(), navRequest(t, fileURL(oldPort, pageRel)))
	if code != http.StatusNotFound || headers.Get("Location") != "" {
		t.Fatal("invalid destination identity relocated")
	}
}

func TestRecoveryDeadNestedSourceExplainsItsOwnReapproval(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(home, "project")
	child := filepath.Join(parent, "child")
	page := filepath.Join(child, "index.htmlclay")
	writeTestFile(t, page, "<html><body>fixture</body></html>")
	configDir := t.TempDir()
	first := newTestAppWithConfigDir(t, home, configDir)
	if err := first.trustFolder(child); err != nil {
		t.Fatal(err)
	}
	site, rel := first.openForTest(t, page)
	bookmark := fileURL(site.port, rel)
	if err := first.trustFolder(parent); err != nil {
		t.Fatal(err)
	}
	first.shutdown()
	current := newTestAppWithConfigDir(t, home, configDir)
	if _, ok := current.rt.cfg.SetTrustedIdentity(child, "invalid-fixture-pin"); !ok {
		t.Fatal("missing child trust")
	}
	current.startSites()
	assertDead := func() {
		t.Helper()
		code, headers, body := do(t, browserClient(), navRequest(t, bookmark))
		if code != http.StatusNotFound || headers.Get("Location") != "" || body != string(deadFolderPage) {
			t.Fatalf("dead source: %d %q %q", code, headers.Get("Location"), body)
		}
	}
	assertDead()
	if err := current.trustFolder(parent); err != nil {
		t.Fatal(err)
	}
	assertDead()
	if err := current.trustFolder(child); err != nil {
		t.Fatal(err)
	}
	code, headers, body := do(t, browserClient(), navRequest(t, bookmark))
	target := liveSite(t, current, parent)
	if code != http.StatusFound || headers.Get("Location") != fileURL(target.port, rel) || body != "" {
		t.Fatalf("reapproved source: %d %q %q", code, headers.Get("Location"), body)
	}
}

func revokedTestSite(a *app, anchor string) *site {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.siteAtLocked(anchor)
}

func revokedTestNavigation(t *testing.T, client *http.Client, target string) (int, error) {
	t.Helper()
	r, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Sec-Fetch-Mode", "navigate")
	r.Header.Set("Sec-Fetch-Dest", "document")
	r.Header.Set("Sec-Fetch-Site", "none")
	resp, err := client.Do(r)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

func revokedTestClient() *http.Client {
	return &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func TestRecoveryRevokedChildPortSurvivesParentRemoval(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(home, "project")
	child := filepath.Join(parent, "child")
	writeTestFile(t, filepath.Join(child, "page.htmlclay"), "<!doctype html><body>fixture</body>")
	cfgBase := t.TempDir()
	first := newTestAppWithConfigDir(t, home, cfgBase)
	if err := first.trustFolder(child); err != nil {
		t.Fatal(err)
	}
	oldPort := revokedTestSite(first, child).port
	if err := first.trustFolder(parent); err != nil {
		t.Fatal(err)
	}
	first.shutdown()

	a := newTestAppWithConfigDir(t, home, cfgBase)
	a.startSites()
	if err := a.untrustFolder(child); err != nil {
		t.Fatal(err)
	}
	if err := a.trustFolder(child); err != nil {
		t.Fatal(err)
	}
	bookmark := fileURL(oldPort, "project/child/page.htmlclay")
	client := revokedTestClient()
	if status, err := revokedTestNavigation(t, client, bookmark); err != nil || status != http.StatusNotFound {
		t.Fatalf("revoked child before parent removal = %d, %v; want 404", status, err)
	}
	if err := a.untrustFolder(parent); err != nil {
		t.Fatal(err)
	}
	status, err := revokedTestNavigation(t, client, bookmark)
	if err != nil {
		ln, bindErr := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(oldPort))
		if bindErr == nil {
			ln.Close()
		}
		t.Fatalf("revoked bookmark stopped answering after parent removal: %v; old port free=%v", err, bindErr == nil)
	}
	if status != http.StatusNotFound {
		t.Fatalf("revoked port = %d, want 404", status)
	}
}

func TestRecoveryRevokedPortSurvivesRetrustingTheSameFolder(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	folder := filepath.Join(home, "proj")
	writeTestFile(t, filepath.Join(folder, "page.htmlclay"), "<!doctype html><body>fixture</body>")
	a := newTestAppWithConfigDir(t, home, t.TempDir())
	if err := a.trustFolder(folder); err != nil {
		t.Fatal(err)
	}
	oldPort := revokedTestSite(a, folder).port
	if err := a.untrustFolder(folder); err != nil {
		t.Fatal(err)
	}
	if err := a.trustFolder(folder); err != nil {
		t.Fatal(err)
	}
	s := revokedTestSite(a, folder)
	if s == nil || s.port == oldPort {
		t.Fatalf("re-trusted site = %+v, want a live site on a fresh port", s)
	}
	status, err := revokedTestNavigation(t, revokedTestClient(), fileURL(oldPort, "proj/page.htmlclay"))
	if err != nil || status != http.StatusNotFound {
		t.Fatalf("revoked address after re-trust = %d, %v; want the 404 recovery page", status, err)
	}
}
