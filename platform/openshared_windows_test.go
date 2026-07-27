//go:build windows

package platform_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/panphora/htmlclay/platform"
)

// These two tests pin the operating-system behavior OpenShared exists for. They
// only mean anything on Windows, where a held handle can veto a rename, so they
// carry the windows build tag rather than being skipped elsewhere.

// The premise: a handle from os.Open blocks an atomic replace. If this ever stops
// failing, Go changed its default sharing mode and OpenShared can go away.
func TestPlainOpenBlocksRename(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.html")
	tmp := filepath.Join(dir, "tmp.html")
	if err := os.WriteFile(target, []byte("one"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("two"), 0644); err != nil {
		t.Fatal(err)
	}

	held, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	if err := os.Rename(tmp, target); err == nil {
		t.Fatal("expected os.Open to block the rename; if it no longer does, OpenShared is unnecessary")
	}
}

// What OpenShared actually buys: the file can still be deleted while we hold it.
func TestOpenSharedAllowsDelete(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.html")
	if err := os.WriteFile(target, []byte("one"), 0644); err != nil {
		t.Fatal(err)
	}

	held, err := platform.OpenShared(target)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	if err := os.Remove(target); err != nil {
		t.Fatalf("a shared handle must not block deletion: %v", err)
	}
}

// And what it does NOT buy, which is the part that cost a release run to learn:
// sharing mode does not make a rename-over-open succeed. MoveFileEx with
// MOVEFILE_REPLACE_EXISTING is refused while any handle is open, however
// permissive. Anything that must survive an atomic replace has to not hold a
// handle at all. Kept as a failing-if-it-ever-changes pin: if Windows starts
// allowing this, the live-sync anchor can go back to holding its descriptor.
func TestOpenSharedStillBlocksRenameOver(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.html")
	tmp := filepath.Join(dir, "tmp.html")
	if err := os.WriteFile(target, []byte("one"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("two"), 0644); err != nil {
		t.Fatal(err)
	}

	held, err := platform.OpenShared(target)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	if err := os.Rename(tmp, target); err == nil {
		t.Fatal("Windows now allows rename over an open shared handle; the anchor may hold a descriptor again")
	}
}
