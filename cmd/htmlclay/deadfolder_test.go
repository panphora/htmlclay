package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestDeadFolderNeverTakesALiveFoldersPort(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	dead := filepath.Join(home, "aaa-dead")
	live := filepath.Join(home, "zzz-live")
	page := filepath.Join(live, "index.htmlclay")
	writeTestFile(t, filepath.Join(dead, "x.htmlclay"), "<html><body>x</body></html>")
	writeTestFile(t, page, "<html><body>live</body></html>")

	cfgBase := t.TempDir()
	first := newTestAppWithConfigDir(t, home, cfgBase)
	if err := first.trustFolder(dead); err != nil {
		t.Fatal(err)
	}
	if err := first.trustFolder(live); err != nil {
		t.Fatal(err)
	}
	s, rel := first.openForTest(t, page)
	livePort := s.port
	bookmark := fileURL(livePort, rel)
	first.shutdown()

	second := newTestAppWithConfigDir(t, home, cfgBase)
	second.rt.cfg.SetTrustedIdentity(dead, "not-the-folder-on-disk")
	second.rt.cfg.RememberSitePort(dead, livePort)
	second.startSites()
	t.Cleanup(second.shutdown)

	code, body := fetch(t, bookmark)
	if code != 200 || !strings.Contains(body, "savetoken") {
		t.Fatalf("the live folder should keep its remembered port, got %d", code)
	}
}

func TestDeadFolderUnderALiveOneGetsTheOrdinaryRecoveryPage(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	proj := filepath.Join(home, "proj")
	sub := filepath.Join(proj, "sub")
	page := filepath.Join(sub, "deep.htmlclay")
	writeTestFile(t, page, "<html><body>deep</body></html>")

	cfgBase := t.TempDir()
	first := newTestAppWithConfigDir(t, home, cfgBase)
	if err := first.trustFolder(sub); err != nil {
		t.Fatal(err)
	}
	s, rel := first.openForTest(t, page)
	subBookmark := fileURL(s.port, rel)
	if err := first.trustFolder(proj); err != nil {
		t.Fatal(err)
	}
	first.shutdown()

	second := newTestAppWithConfigDir(t, home, cfgBase)
	second.rt.cfg.SetTrustedIdentity(sub, "not-the-folder-on-disk")
	second.startSites()
	t.Cleanup(second.shutdown)

	_, body := fetch(t, subBookmark)
	if strings.Contains(body, "needs approving again") {
		t.Fatal("re-approving a folder under a live one cannot bring its origin back, so the page must not ask for it")
	}
	if !strings.Contains(body, "Nothing is open at this address") {
		t.Fatal("expected the ordinary recovery page")
	}
}

func TestUntrustingADeadFolderSwapsItsPage(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	proj := filepath.Join(home, "proj")
	page := filepath.Join(proj, "index.htmlclay")
	writeTestFile(t, page, "<html><body>index</body></html>")

	cfgBase := t.TempDir()
	first := newTestAppWithConfigDir(t, home, cfgBase)
	if err := first.trustFolder(proj); err != nil {
		t.Fatal(err)
	}
	s, rel := first.openForTest(t, page)
	bookmark := fileURL(s.port, rel)
	first.shutdown()

	second := newTestAppWithConfigDir(t, home, cfgBase)
	second.rt.cfg.SetTrustedIdentity(proj, "not-the-folder-on-disk")
	second.startSites()
	t.Cleanup(second.shutdown)

	if _, body := fetch(t, bookmark); !strings.Contains(body, "needs approving again") {
		t.Fatal("setup: expected the dead-folder page before untrusting")
	}
	if err := second.untrustFolder(proj); err != nil {
		t.Fatal(err)
	}
	_, body := fetch(t, bookmark)
	if strings.Contains(body, "needs approving again") {
		t.Fatal("an untrusted folder's port must not ask for the folder to be approved again")
	}
	if !strings.Contains(body, "Nothing is open at this address") {
		t.Fatal("expected the ordinary recovery page")
	}
}
