//go:build darwin

package platform

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A reboot renumbering st_dev, simulated on a real directory: the pin carries
// the directory's real inode and a device number it no longer has.
func TestLegacyPinSurvivesADeviceRenumbering(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	dir := filepath.Join(home, "proj")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(dir)
	st := info.Sys().(*syscall.Stat_t)
	stale := fmt.Sprintf("%d:%d", uint64(st.Dev)+3, uint64(st.Ino))

	current, ok := MatchDirIdentity(dir, stale, home)
	if !ok {
		t.Fatalf("a dev:inode pin whose dev was renumbered must still match on home's volume (pin %q, now %q)", stale, current)
	}
	if current != DirIdentity(dir) {
		t.Fatalf("current = %q, want the directory's fingerprint %q", current, DirIdentity(dir))
	}

	moved := filepath.Join(home, "moved")
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, ok := MatchDirIdentity(dir, stale, home); ok {
		t.Fatal("a folder recreated at the path must not satisfy the old pin")
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, dir); err != nil {
		t.Fatal(err)
	}
	if _, ok := MatchDirIdentity(dir, current, home); !ok {
		t.Fatal("a symlink to the very same directory is that directory, as it was under dev:inode")
	}
}

func TestDirIdentityCarriesAStableVolumeID(t *testing.T) {
	id := DirIdentity(t.TempDir())
	if len(id) < 3 || id[:3] != "v2:" {
		t.Fatalf("expected a v2 volume-based fingerprint on this platform, got %q", id)
	}
}

// "/" is the sealed System volume, which has a different UUID from the data
// volume holding the temp dir, so it cannot vouch for a legacy pin there.
func TestLegacyPinNeedsHomesVolume(t *testing.T) {
	dir := t.TempDir()
	info, _ := os.Stat(dir)
	st := info.Sys().(*syscall.Stat_t)
	stale := fmt.Sprintf("%d:%d", uint64(st.Dev)+3, uint64(st.Ino))
	if _, ok := MatchDirIdentity(dir, stale, "/"); ok {
		t.Fatal("a renumbered legacy pin must not match when home is on another volume")
	}
}

func TestTraverseOnlyFolderKeepsAVolumeID(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "x")
	if err := os.Mkdir(dir, 0311); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0755) })
	if id := DirIdentity(dir); !strings.HasPrefix(id, "v2:") {
		t.Fatalf("a folder that cannot be listed should still get a v2 fingerprint, got %q", id)
	}
}

func TestFIFOAtTrustedPathDoesNotBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p")
	if err := syscall.Mkfifo(path, 0644); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		MatchDirIdentity(path, "16777229:1", filepath.Dir(path))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("reading the identity of a FIFO blocked")
	}
}
