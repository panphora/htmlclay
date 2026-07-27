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

// The fix: the same handle from OpenShared must let the replace through, because
// htmlclay holds one of these on every file it serves while saving to it.
func TestOpenSharedAllowsRename(t *testing.T) {
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

	if err := os.Rename(tmp, target); err != nil {
		t.Fatalf("a shared handle must not block the replace: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "two" {
		t.Fatalf("target holds %q, want the replacement", got)
	}
}
