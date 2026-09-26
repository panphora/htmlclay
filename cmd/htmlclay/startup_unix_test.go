//go:build darwin

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/panphora/htmlclay/internal/config"
	"github.com/panphora/htmlclay/internal/platform"
)

// The reported bug: macOS renumbers st_dev at boot, so every pin taken in the old
// dev:inode form went stale and the bookmark refused to connect. Simulated by
// pinning the folder's real inode under a device number it no longer has.
func TestTrustedFolderSurvivesADeviceRenumbering(t *testing.T) {
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
	port := s.port
	bookmark := fileURL(port, rel)
	first.shutdown()

	info, err := os.Stat(proj)
	if err != nil {
		t.Fatal(err)
	}
	st := info.Sys().(*syscall.Stat_t)
	stale := fmt.Sprintf("%d:%d", uint64(st.Dev)+3, uint64(st.Ino))

	aged, _, err := config.LoadFrom(cfgBase, platform.DirIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := aged.SetTrustedIdentity(proj, stale); !ok {
		t.Fatal("no trusted entry to age")
	}
	if err := aged.Save(); err != nil {
		t.Fatal(err)
	}
	second := newTestAppWithConfigDir(t, home, cfgBase)
	second.finishUpgrade()
	second.startSites()
	t.Cleanup(second.shutdown)

	for _, tf := range second.rt.cfg.TrustedFolderList() {
		if tf.Path == proj && tf.Identity != platform.DirIdentity(proj) {
			t.Fatalf("the stale pin was not moved to the current fingerprint: %q", tf.Identity)
		}
	}
	reloaded, _, err := config.LoadFrom(cfgBase, platform.DirIdentity)
	if err != nil {
		t.Fatal(err)
	}
	for _, tf := range reloaded.TrustedFolderList() {
		if tf.Path == proj && tf.Identity != platform.DirIdentity(proj) {
			t.Fatalf("the re-pin did not reach disk: %q", tf.Identity)
		}
	}
	code, body := fetch(t, bookmark)
	if code != 200 || !strings.Contains(body, "savetoken") {
		t.Fatalf("the bookmark should serve the file editable after the renumbering, got %d", code)
	}
}
