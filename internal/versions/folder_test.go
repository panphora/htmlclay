package versions

import (
	"os"
	"path/filepath"
	"testing"
)

// Folder must name the directory versions are really written to, so it is checked
// against a backup rather than against a path the test computed itself.
func TestFolderHoldsTheBackup(t *testing.T) {
	s := newStore(t)
	path := filepath.Join(t.TempDir(), "board.htmlclay")
	content := doc("board")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	key := Key(path, content)

	if _, err := s.Backup(key, path, content); err != nil {
		t.Fatalf("backup: %v", err)
	}

	folder, err := s.Folder(key, path)
	if err != nil {
		t.Fatalf("folder: %v", err)
	}
	if !filepath.IsAbs(folder) {
		t.Errorf("folder %q is not absolute", folder)
	}
	if !s.Contains(folder) {
		t.Errorf("folder %q is outside the versions directory %q", folder, s.BaseDir())
	}
	entries, err := os.ReadDir(folder)
	if err != nil {
		t.Fatalf("read %q: %v", folder, err)
	}
	found := false
	for _, entry := range entries {
		if _, _, err := ParseEntryName(entry.Name()); err == nil {
			found = true
		}
	}
	if !found {
		t.Errorf("folder %q holds no version file: %v", folder, entries)
	}
}

// A document with no history yet still gets a folder, because the action opens
// one and a person who just started editing expects somewhere to look.
func TestFolderCreatesForANewDocument(t *testing.T) {
	s := newStore(t)
	path := filepath.Join(t.TempDir(), "new.htmlclay")
	if err := os.WriteFile(path, doc("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	folder, err := s.Folder(Key(path, nil), path)
	if err != nil {
		t.Fatalf("folder: %v", err)
	}
	info, err := os.Stat(folder)
	if err != nil || !info.IsDir() {
		t.Fatalf("folder %q: %v", folder, err)
	}
}
