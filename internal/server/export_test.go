package server

import (
	"archive/zip"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// zipEntries reads a zip back as entry name -> contents, so a test can compare
// the whole archive rather than one member of it.
func zipEntries(t *testing.T, path string) map[string]string {
	t.Helper()
	r, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer r.Close()
	entries := make(map[string]string, len(r.File))
	for _, f := range r.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %s inside the zip: %v", f.Name, err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read %s inside the zip: %v", f.Name, err)
		}
		entries[f.Name] = string(b)
	}
	return entries
}

// exportNames is the zip's entry list in a stable order, for a failure message
// that names what was actually written.
func exportNames(entries map[string]string) []string {
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func writeExportFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Only the document and its uploads go in, under one top folder named after the
// document: the relative URL an upload wrote into the page has to keep resolving
// once the zip is unpacked anywhere. The hidden temp file of an interrupted
// upload and a symlink out of the folder are both left out, and nothing of the
// half-written zip survives.
func TestExportDocumentZipCarriesTheDocumentAndItsAssets(t *testing.T) {
	dir := t.TempDir()
	outDir := t.TempDir()
	document := filepath.Join(dir, "board.htmlclay")
	writeExportFile(t, document, "<!doctype html><h1>board</h1>")
	writeExportFile(t, filepath.Join(dir, "assets-board", "photo-abc.png"), "PNGDATA")
	writeExportFile(t, filepath.Join(dir, "assets-board", "sub", "x.pdf"), "PDFDATA")
	writeExportFile(t, filepath.Join(dir, "assets-board", ".upload-1.tmp"), "half")
	outside := filepath.Join(t.TempDir(), "secret.txt")
	writeExportFile(t, outside, "outside-secret")
	if err := os.Symlink(outside, filepath.Join(dir, "assets-board", "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	out, err := ExportDocumentZip(document, outDir)
	if err != nil {
		t.Fatalf("ExportDocumentZip: %v", err)
	}
	if want := filepath.Join(outDir, "board.zip"); out != want {
		t.Fatalf("zip path = %q, want %q", out, want)
	}

	entries := zipEntries(t, out)
	want := map[string]string{
		"board/board.htmlclay":             "<!doctype html><h1>board</h1>",
		"board/assets-board/photo-abc.png": "PNGDATA",
		"board/assets-board/sub/x.pdf":     "PDFDATA",
	}
	if len(entries) != len(want) {
		t.Fatalf("zip holds %v, want exactly %v", exportNames(entries), exportNames(want))
	}
	for name, content := range want {
		if entries[name] != content {
			t.Errorf("zip entry %q = %q, want %q", name, entries[name], content)
		}
	}

	left, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].Name() != "board.zip" {
		names := make([]string, 0, len(left))
		for _, e := range left {
			names = append(names, e.Name())
		}
		t.Errorf("output folder holds %v, want only board.zip", names)
	}
}

// A document with no uploads still exports, as itself alone: the export is not
// allowed to depend on an assets folder that may never have been created.
func TestExportDocumentZipWithoutAssetsFolder(t *testing.T) {
	dir := t.TempDir()
	outDir := t.TempDir()
	document := filepath.Join(dir, "solo.htmlclay")
	writeExportFile(t, document, "<!doctype html><p>solo</p>")

	out, err := ExportDocumentZip(document, outDir)
	if err != nil {
		t.Fatalf("ExportDocumentZip: %v", err)
	}
	entries := zipEntries(t, out)
	if len(entries) != 1 || entries["solo/solo.htmlclay"] != "<!doctype html><p>solo</p>" {
		t.Fatalf("zip holds %v, want only the document", exportNames(entries))
	}
}

// Exporting the same document twice must never overwrite the first zip: the user
// who exports again is asking for a second copy, and the first may already be the
// one they sent.
func TestExportDocumentZipNeverReplacesAnExistingZip(t *testing.T) {
	dir := t.TempDir()
	outDir := t.TempDir()
	document := filepath.Join(dir, "board.htmlclay")
	writeExportFile(t, document, "first")
	writeExportFile(t, filepath.Join(dir, "assets-board", "photo-abc.png"), "PNGDATA")

	first, err := ExportDocumentZip(document, outDir)
	if err != nil {
		t.Fatalf("first ExportDocumentZip: %v", err)
	}
	before, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}

	second, err := ExportDocumentZip(document, outDir)
	if err != nil {
		t.Fatalf("second ExportDocumentZip: %v", err)
	}
	if want := filepath.Join(outDir, "board 2.zip"); second != want {
		t.Fatalf("second zip path = %q, want %q", second, want)
	}
	after, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("the first zip changed when the second was written")
	}
	if len(zipEntries(t, second)) != 2 {
		t.Errorf("the second zip holds %v, want the document and its asset", exportNames(zipEntries(t, second)))
	}
}

// A plain .html document names its uploads folder the same way an upload does:
// the export and the asset lane have to agree on the folder or the zip would ship
// a page whose links point at a folder that is not in it.
func TestExportDocumentZipUsesTheUploadFolderForHTML(t *testing.T) {
	dir := t.TempDir()
	outDir := t.TempDir()
	document := filepath.Join(dir, "page.html")
	writeExportFile(t, document, "<!doctype html><p>page</p>")
	writeExportFile(t, filepath.Join(dir, "assets-page", "photo-abc.png"), "PNGDATA")

	out, err := ExportDocumentZip(document, outDir)
	if err != nil {
		t.Fatalf("ExportDocumentZip: %v", err)
	}
	if want := filepath.Join(outDir, "page.zip"); out != want {
		t.Fatalf("zip path = %q, want %q", out, want)
	}
	entries := zipEntries(t, out)
	if entries["page/assets-page/photo-abc.png"] != "PNGDATA" {
		t.Fatalf("zip holds %v, want the asset under assets-page", exportNames(entries))
	}
}
