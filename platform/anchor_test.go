package platform_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/panphora/htmlclay/platform"
)

func anchorFile(t *testing.T, path, body string) *platform.Anchor {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	a, err := platform.NewAnchor(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a
}

// Editing a file in place leaves it the same file, so an editor that rewrites
// rather than replaces must not look like a new document.
func TestAnchorRecognisesTheSameFileAfterAnInPlaceWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "page.html")
	before := anchorFile(t, path, "one")

	if err := os.WriteFile(path, []byte("two, longer"), 0644); err != nil {
		t.Fatal(err)
	}
	after, err := platform.NewAnchor(path)
	if err != nil {
		t.Fatal(err)
	}
	defer after.Close()

	if !before.Same(after) {
		t.Fatal("an in-place rewrite must leave the file the same file")
	}
}

func TestAnchorDistinguishesTwoFiles(t *testing.T) {
	dir := t.TempDir()
	a := anchorFile(t, filepath.Join(dir, "a.html"), "same bytes")
	b := anchorFile(t, filepath.Join(dir, "b.html"), "same bytes")

	if a.Same(b) {
		t.Fatal("two files with identical contents are still two files")
	}
}

// The case the whole seam exists for: something outside htmlclay replaced the
// file at a path, so anything the hub retained about the old one is about a
// document that is gone.
func TestAnchorSeesAnAtomicReplaceAsADifferentFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "page.html")
	tmp := filepath.Join(dir, "page.html.tmp")
	before := anchorFile(t, target, "one")

	if err := os.WriteFile(tmp, []byte("two"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, target); err != nil {
		t.Fatal(err)
	}
	after, err := platform.NewAnchor(target)
	if err != nil {
		t.Fatal(err)
	}
	defer after.Close()

	if before.Same(after) {
		t.Fatal("a file replaced from outside must not pass as the file we anchored")
	}
}

// An existing anchor must never make its file unsaveable. This is trivially true
// on Unix and is the load-bearing assertion on Windows, where a handle held for
// bookkeeping vetoes the rename-over that every htmlclay save performs.
func TestAnchorDoesNotBlockRenameOver(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "page.html")
	tmp := filepath.Join(dir, "page.html.tmp")
	anchorFile(t, target, "one")

	if err := os.WriteFile(tmp, []byte("two"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, target); err != nil {
		t.Fatalf("an anchor is holding the file open, which makes every save fail on Windows: %v", err)
	}
}

// A directory is not a document. The Unix half checks the mode; the Windows half
// gets this from leaving FILE_FLAG_BACKUP_SEMANTICS out of CreateFile, which is
// subtle enough to be worth pinning.
func TestAnchorRejectsADirectory(t *testing.T) {
	if _, err := platform.NewAnchor(t.TempDir()); err == nil {
		t.Fatal("a directory must not be anchorable")
	}
}

// The hub closes a displaced anchor and closes it again on shutdown rather than
// tracking which ones it has already released, so the promise in the doc comment
// is load-bearing.
func TestClosingAnAnchorTwiceIsHarmless(t *testing.T) {
	a := anchorFile(t, filepath.Join(t.TempDir(), "page.html"), "one")
	a.Close()
	a.Close()
}

// Fail closed. An identity nobody could establish is not evidence that nothing
// changed, so it compares unequal even against another unknown.
func TestNilAnchorIsNeverTheSameAsAnything(t *testing.T) {
	real := anchorFile(t, filepath.Join(t.TempDir(), "page.html"), "one")
	var missing *platform.Anchor

	if missing.Same(real) || real.Same(missing) || missing.Same(missing) {
		t.Fatal("an unestablished identity must never compare equal")
	}
	missing.Close()
}
