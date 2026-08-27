package platform_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/panphora/htmlclay/internal/platform"
)

// The contract every platform now honors: a real directory has a fingerprint,
// the fingerprint follows the directory through a rename, and a new directory
// created at the old path gets a different one. No build tag, because the
// scenario is the same everywhere and the Windows implementation is the one
// most in need of the coverage: it returned "" until 1.8.0, so a folder renamed
// away and replaced inherited the grant through its path alone.
//
// The replacement is created while the renamed original still exists, so the
// filesystem cannot hand the new directory the old inode or file id. Deleting
// first would make that reuse possible on ext4 and the test flaky.
func TestDirIdentityChangesWhenAFolderIsReplaced(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "project")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}

	original := platform.DirIdentity(dir)
	if original == "" {
		t.Fatal("a real directory must have a fingerprint on this platform")
	}
	if again := platform.DirIdentity(dir); again != original {
		t.Fatalf("two reads of one directory disagree: %q then %q", original, again)
	}

	moved := filepath.Join(base, "renamed")
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}

	if got := platform.DirIdentity(moved); got != original {
		t.Errorf("the fingerprint must follow the directory through a rename: got %q, want %q", got, original)
	}
	if got := platform.DirIdentity(dir); got == original {
		t.Error("a new directory at the old path must not inherit the fingerprint; the grant would follow the path, not the folder")
	}
}

// A path with nothing behind it yields "", which trust.IdentityOK treats as
// path-only identity. Returning an error string here instead would make a dead
// entry compare unequal forever and read as replaced rather than missing.
func TestDirIdentityOfAMissingPathIsEmpty(t *testing.T) {
	if got := platform.DirIdentity(filepath.Join(t.TempDir(), "never-made")); got != "" {
		t.Errorf("missing path must have no fingerprint, got %q", got)
	}
}
