//go:build windows

package platform_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/panphora/htmlclay/platform"
)

func mkdir(t *testing.T, path string) string {
	t.Helper()
	if err := os.Mkdir(path, 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

// The case the pin exists for. Before this, DirIdentity returned "" on Windows,
// the stored path was the whole identity, and a folder deleted and recreated at
// the same path inherited a standing write grant over its contents.
func TestDirIdentitySeesAReplacedDirectory(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "project")
	mkdir(t, project)

	before := platform.DirIdentity(project)
	if before == "" {
		t.Fatal("a directory on the test volume produced no fingerprint at all")
	}

	if err := os.Rename(project, filepath.Join(base, "moved-away")); err != nil {
		t.Fatal(err)
	}
	mkdir(t, project)

	after := platform.DirIdentity(project)
	if after == "" {
		t.Fatal("the replacement directory produced no fingerprint")
	}
	if after == before {
		t.Fatal("a new directory at the same path must not carry the old one's fingerprint")
	}
}

// The other half of the same property: the fingerprint follows the directory,
// not the name. A user who renames their project folder has not replaced it, and
// a rename must not read as a swap.
func TestDirIdentityFollowsARenamedDirectory(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "project")
	mkdir(t, project)
	before := platform.DirIdentity(project)

	moved := filepath.Join(base, "renamed")
	if err := os.Rename(project, moved); err != nil {
		t.Fatal(err)
	}

	if got := platform.DirIdentity(moved); got != before {
		t.Errorf("fingerprint changed on rename: %q then %q", before, got)
	}
}

// Reading twice, and writing inside the folder in between, must answer the same.
// A fingerprint that moved when a file was added would make every trusted folder
// go dead the moment it was used.
func TestDirIdentityIsStableAcrossWritesInside(t *testing.T) {
	dir := t.TempDir()
	first := platform.DirIdentity(dir)
	if first == "" {
		t.Fatal("no fingerprint for a temp directory")
	}
	if err := os.WriteFile(filepath.Join(dir, "page.htmlclay"), []byte("<html></html>"), 0644); err != nil {
		t.Fatal(err)
	}
	if second := platform.DirIdentity(dir); second != first {
		t.Errorf("fingerprint changed after a write inside: %q then %q", first, second)
	}
}

// "" is the documented no-answer, and trust.IdentityOK reads it as "the path is
// the whole identity". A missing folder must produce it rather than something
// that looks like a real pin.
func TestDirIdentityIsEmptyForAMissingDirectory(t *testing.T) {
	if got := platform.DirIdentity(filepath.Join(t.TempDir(), "not-there")); got != "" {
		t.Errorf("a missing directory produced %q, want the empty fingerprint", got)
	}
}
