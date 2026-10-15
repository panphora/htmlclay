package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// handoffFixture is one app's worth of open documents: two files in a trusted
// folder and one file opened ad hoc with a read grant beside it, every token and
// host captured, and the app shut down. The handoff, when there is one, is
// written before the shutdown exactly as the updater will write it.
type handoffFixture struct {
	home, cfgBase          string
	page, other, loose     string
	assets                 string
	pageTok, otherTok      string
	looseTok               string
	trustedHost, looseHost string
	handoff                string
	port                   int
}

func newHandoffFixture(t *testing.T, targetVersion string) handoffFixture {
	t.Helper()
	home, _ := filepath.EvalSymlinks(t.TempDir())
	fx := handoffFixture{
		home:    home,
		cfgBase: t.TempDir(),
		page:    filepath.Join(home, "proj", "index.htmlclay"),
		other:   filepath.Join(home, "proj", "other.htmlclay"),
		loose:   filepath.Join(home, "notes", "loose.htmlclay"),
		assets:  filepath.Join(home, "assets"),
		handoff: filepath.Join(t.TempDir(), updateHandoffFile),
	}
	writeTestFile(t, fx.page, "<html><body>index</body></html>")
	writeTestFile(t, fx.other, "<html><body>other</body></html>")
	writeTestFile(t, fx.loose, "<html><body>loose</body></html>")
	writeTestFile(t, filepath.Join(fx.assets, "logo.txt"), "logo")

	first := newTestAppWithConfigDir(t, home, fx.cfgBase)
	if err := first.trustFolder(filepath.Join(home, "proj")); err != nil {
		t.Fatalf("trust: %v", err)
	}
	trusted, _ := first.openForTest(t, fx.page)
	if again, _ := first.openForTest(t, fx.other); again != trusted {
		t.Fatal("both trusted files should share one site")
	}
	adHoc, _ := first.openForTest(t, fx.loose)
	if err := adHoc.sessions.GrantCanonicalRoot(fx.assets); err != nil {
		t.Fatalf("grant: %v", err)
	}
	fx.trustedHost, fx.looseHost = trusted.host, adHoc.host
	fx.pageTok = tokenFor(t, trusted, fx.page)
	fx.otherTok = tokenFor(t, trusted, fx.other)
	fx.looseTok = tokenFor(t, adHoc, fx.loose)
	fx.port = first.rt.port

	if targetVersion != "" {
		if err := first.writeUpdateHandoff(fx.handoff, targetVersion); err != nil {
			t.Fatalf("write handoff: %v", err)
		}
	}
	first.shutdown()
	requirePortFree(t, fx.port)
	return fx
}

func tokenFor(t *testing.T, s *site, path string) string {
	t.Helper()
	f, ok := s.sessions.LookupByPath(path)
	if !ok {
		t.Fatalf("%s is not registered on %s", path, s.host)
	}
	return f.Token
}

// restart starts a fresh app on the same config, the way startup_test does, and
// runs the startup steps main runs before argv: the trusted sites, then the
// handoff.
func (fx handoffFixture) restart(t *testing.T) (*app, int, error) {
	t.Helper()
	second := newTestAppWithConfigDir(t, fx.home, fx.cfgBase)
	if second.rt.port != fx.port {
		t.Fatalf("the port moved across the restart: %d, want %d", second.rt.port, fx.port)
	}
	second.startSites()
	n, err := second.restoreUpdateHandoff(fx.handoff)
	return second, n, err
}

func saveWith(t *testing.T, host, token, body string) (int, string) {
	t.Helper()
	return postSameOrigin(t, "http://"+host+"/_/save/"+token, "text/html", body)
}

func requireGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the handoff must be deleted on read, stat err = %v", err)
	}
}

// The acceptance test for the update handoff: tabs open before an update
// restart keep saving afterwards with the tokens they already hold, at the
// addresses they are already on, for a trusted folder and for an ad-hoc file,
// and the ad-hoc file's read grant comes back with it.
func TestUpdateHandoffKeepsOpenTabsSaving(t *testing.T) {
	fx := newHandoffFixture(t, version)
	if runtime.GOOS != "windows" {
		info, err := os.Stat(fx.handoff)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0600 {
			t.Fatalf("the handoff holds save tokens and must be 0600, got %o", perm)
		}
	}

	second, n, err := fx.restart(t)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if n != 3 {
		t.Fatalf("restored %d registrations, want 3", n)
	}
	requireGone(t, fx.handoff)

	for _, c := range []struct{ host, token, path, body string }{
		{fx.trustedHost, fx.pageTok, fx.page, "<html><body>index, after the update</body></html>"},
		{fx.trustedHost, fx.otherTok, fx.other, "<html><body>other, after the update</body></html>"},
		{fx.looseHost, fx.looseTok, fx.loose, "<html><body>loose, after the update</body></html>"},
	} {
		if code, out := saveWith(t, c.host, c.token, c.body); code != 200 {
			t.Fatalf("save to %s with the old token = %d: %s", c.host, code, out)
		}
		saved, _ := os.ReadFile(c.path)
		if !strings.Contains(string(saved), "after the update") {
			t.Fatalf("the save to %s did not reach disk", c.path)
		}
	}

	s := liveSiteWithHost(t, second, fx.looseHost)
	if _, _, ok := s.sessions.AssetRoot(filepath.Join(fx.assets, "logo.txt")); !ok {
		t.Fatal("the ad-hoc site's read grant did not come back")
	}
}

func liveSiteWithHost(t *testing.T, a *app, host string) *site {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range a.sites {
		if s.host == host {
			return s
		}
	}
	t.Fatalf("no live site at %s", host)
	return nil
}

// An ordinary restart, with no handoff, is unchanged: the old tokens are dead,
// a trusted folder answers with its new tokens only, and the ad-hoc address
// answers with the recovery page and holds nothing.
func TestOrdinaryRestartStillRevokesEverySession(t *testing.T) {
	fx := newHandoffFixture(t, "")

	second, n, err := fx.restart(t)
	if err != nil || n != 0 {
		t.Fatalf("restore with no handoff = (%d, %v), want (0, nil)", n, err)
	}
	if code, _ := saveWith(t, fx.trustedHost, fx.pageTok, "<html><body>x</body></html>"); code != 401 {
		t.Fatalf("an old trusted token must be refused after an ordinary restart, got %d", code)
	}
	code, body := saveWith(t, fx.looseHost, fx.looseTok, "<html><body>x</body></html>")
	if code != 404 || !strings.Contains(body, "Nothing is open at this address") {
		t.Fatalf("an old ad-hoc address must answer the recovery page, got %d", code)
	}
	second.mu.Lock()
	defer second.mu.Unlock()
	for _, s := range second.sites {
		if _, ok := s.sessions.LookupByPath(fx.loose); ok {
			t.Fatal("an ordinary restart registered the ad-hoc file")
		}
	}
}

// A handoff that is too old, or written for another version, is deleted and
// ignored whole: nothing it names comes back.
func TestStaleOrMismatchedHandoffIsIgnoredAndDeleted(t *testing.T) {
	cases := []struct {
		name   string
		target string
		age    time.Duration
	}{
		{"stale", version, 2 * updateHandoffMaxAge},
		{"other version", version + "-other", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := newHandoffFixture(t, c.target)
			if c.age > 0 {
				backdateHandoff(t, fx.handoff, c.age)
			}
			_, n, err := fx.restart(t)
			if err == nil || n != 0 {
				t.Fatalf("restore = (%d, %v), want the handoff refused", n, err)
			}
			requireGone(t, fx.handoff)
			if code, _ := saveWith(t, fx.trustedHost, fx.pageTok, "<html><body>x</body></html>"); code != 401 {
				t.Fatalf("a refused handoff restored a trusted token, save = %d", code)
			}
			if code, _ := saveWith(t, fx.looseHost, fx.looseTok, "<html><body>x</body></html>"); code != 404 {
				t.Fatalf("a refused handoff restored the ad-hoc site, save = %d", code)
			}
		})
	}
}

func backdateHandoff(t *testing.T, path string, by time.Duration) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var h updateHandoff
	if err := json.Unmarshal(data, &h); err != nil {
		t.Fatal(err)
	}
	h.Created = h.Created.Add(-by)
	out, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0600); err != nil {
		t.Fatal(err)
	}
}

// A file deleted while the app was down is skipped, and everything else still
// comes back. A site whose every file is gone is not published at all, so its
// address keeps answering the recovery page.
func TestHandoffSkipsADeletedFileAndRestoresTheRest(t *testing.T) {
	fx := newHandoffFixture(t, version)
	for _, gone := range []string{fx.other, fx.loose} {
		if err := os.Remove(gone); err != nil {
			t.Fatal(err)
		}
	}

	second, n, err := fx.restart(t)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if n != 1 {
		t.Fatalf("restored %d registrations, want 1", n)
	}
	if code, out := saveWith(t, fx.trustedHost, fx.pageTok, "<html><body>index, kept</body></html>"); code != 200 {
		t.Fatalf("the surviving file should still save, got %d: %s", code, out)
	}
	if code, _ := saveWith(t, fx.trustedHost, fx.otherTok, "<html><body>x</body></html>"); code != 401 {
		t.Fatalf("the deleted file's token must not come back, got %d", code)
	}
	if code, _ := saveWith(t, fx.looseHost, fx.looseTok, "<html><body>x</body></html>"); code != 404 {
		t.Fatalf("a site with no surviving file must not be published, got %d", code)
	}
	if _, err := os.Stat(fx.loose); !os.IsNotExist(err) {
		t.Fatal("a refused save recreated the deleted file")
	}
	second.mu.Lock()
	defer second.mu.Unlock()
	if len(second.sites) != 1 {
		t.Fatalf("want only the trusted folder's site live, got %d", len(second.sites))
	}
}
