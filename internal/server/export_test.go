package server

import (
	"archive/zip"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

	out, err := ExportDocumentZip(document, outDir, "")
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

	out, err := ExportDocumentZip(document, outDir, "")
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

	first, err := ExportDocumentZip(document, outDir, "")
	if err != nil {
		t.Fatalf("first ExportDocumentZip: %v", err)
	}
	before, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}

	second, err := ExportDocumentZip(document, outDir, "")
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

	out, err := ExportDocumentZip(document, outDir, "")
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

// A document travels with the files it links: every /_/uploads/ path it names is
// read out of the per-computer library and put under <top>/uploads/, and the copy
// in the zip says uploads/... instead, so unzipping the archive anywhere leaves a
// page whose pictures still load. The same file linked twice goes in once, and
// the working document is never written.
func TestExportDocumentZipPackagesLinkedLibraryUploads(t *testing.T) {
	dir := t.TempDir()
	outDir := t.TempDir()
	library := t.TempDir()
	document := filepath.Join(dir, "board.htmlclay")
	writeExportFile(t, document, `<img src="/_/uploads/assets-board/a.png">`+
		`<a href="/_/uploads/assets-board/b%20c.pdf">b</a>`+
		`<img src="/_/uploads/assets-board/a.png">`)
	writeExportFile(t, filepath.Join(library, "assets-board", "a.png"), "PNGDATA")
	writeExportFile(t, filepath.Join(library, "assets-board", "b c.pdf"), "PDFDATA")

	out, err := ExportDocumentZip(document, outDir, library)
	if err != nil {
		t.Fatalf("ExportDocumentZip: %v", err)
	}
	entries := zipEntries(t, out)
	want := map[string]string{
		"board/board.htmlclay": `<img src="uploads/assets-board/a.png">` +
			`<a href="uploads/assets-board/b%20c.pdf">b</a>` +
			`<img src="uploads/assets-board/a.png">`,
		"board/uploads/assets-board/a.png":   "PNGDATA",
		"board/uploads/assets-board/b c.pdf": "PDFDATA",
	}
	if len(entries) != len(want) {
		t.Fatalf("zip holds %v, want exactly %v", exportNames(entries), exportNames(want))
	}
	for name, content := range want {
		if entries[name] != content {
			t.Errorf("zip entry %q = %q, want %q", name, entries[name], content)
		}
	}
	onDisk, err := os.ReadFile(document)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != `<img src="/_/uploads/assets-board/a.png">`+
		`<a href="/_/uploads/assets-board/b%20c.pdf">b</a>`+
		`<img src="/_/uploads/assets-board/a.png">` {
		t.Errorf("the working document changed: %q", onDisk)
	}
}

// A full URL to another host is not a host path: /_/uploads/ at the end of a host
// name is part of that host's address, not a link into this computer's library.
func TestExportDocumentZipIgnoresAUploadPathOnAnotherHost(t *testing.T) {
	dir := t.TempDir()
	outDir := t.TempDir()
	library := t.TempDir()
	document := filepath.Join(dir, "board.htmlclay")
	doc := `<img src="https://x.hyperclay.com/_/uploads/assets-board/a.png">`
	writeExportFile(t, document, doc)
	writeExportFile(t, filepath.Join(library, "assets-board", "a.png"), "PNGDATA")

	out, err := ExportDocumentZip(document, outDir, library)
	if err != nil {
		t.Fatalf("ExportDocumentZip: %v", err)
	}
	entries := zipEntries(t, out)
	if len(entries) != 1 || entries["board/board.htmlclay"] != doc {
		t.Fatalf("zip holds %v, want the document alone, unchanged", exportNames(entries))
	}
}

// Nothing outside the library can be reached through a link, and nothing that is
// not a regular file goes in: a "..", an escaped "..", a symlink out of the
// library and a name that is simply not there all add nothing. The export still
// succeeds, because a document with a stale link is still worth exporting.
func TestExportDocumentZipLeavesRefusedAndMissingLibraryFilesOut(t *testing.T) {
	dir := t.TempDir()
	outDir := t.TempDir()
	library := t.TempDir()
	outside := t.TempDir()
	document := filepath.Join(dir, "board.htmlclay")
	writeExportFile(t, document, `<img src="/_/uploads/assets-board/../secret.txt">`+
		`<img src="/_/uploads/assets-board/%2e%2e/secret.txt">`+
		`<img src="/_/uploads/assets-linked/out.png">`+
		`<img src="/_/uploads/assets-board/missing.png">`)
	writeExportFile(t, filepath.Join(library, "secret.txt"), "SECRET")
	writeExportFile(t, filepath.Join(outside, "out.png"), "OUTSIDE")
	if err := os.Symlink(outside, filepath.Join(library, "assets-linked")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	out, err := ExportDocumentZip(document, outDir, library)
	if err != nil {
		t.Fatalf("ExportDocumentZip: %v", err)
	}
	entries := zipEntries(t, out)
	want := `<img src="/_/uploads/assets-board/../secret.txt">` +
		`<img src="/_/uploads/assets-board/%2e%2e/secret.txt">` +
		`<img src="uploads/assets-linked/out.png">` +
		`<img src="uploads/assets-board/missing.png">`
	if len(entries) != 1 || entries["board/board.htmlclay"] != want {
		t.Fatalf("zip holds %v, want the document alone with no library file in it", exportNames(entries))
	}
}

// A host with no library to read -- the home directory could not be resolved, or
// nothing was ever uploaded on this computer -- still exports: the links are
// rewritten, so the document never travels carrying a host path that resolves
// nowhere on the computer it was sent to.
func TestExportDocumentZipRewritesLinksWithNoLibraryToRead(t *testing.T) {
	for _, uploadsDir := range []string{"", filepath.Join(t.TempDir(), "never-created")} {
		dir := t.TempDir()
		outDir := t.TempDir()
		document := filepath.Join(dir, "board.htmlclay")
		writeExportFile(t, document, `<img src="/_/uploads/assets-board/a.png">`)

		out, err := ExportDocumentZip(document, outDir, uploadsDir)
		if err != nil {
			t.Fatalf("ExportDocumentZip(%q): %v", uploadsDir, err)
		}
		entries := zipEntries(t, out)
		if len(entries) != 1 || entries["board/board.htmlclay"] != `<img src="uploads/assets-board/a.png">` {
			t.Fatalf("zip holds %v, want the document with a relative link", exportNames(entries))
		}
	}
}

// A document that links no host path exports exactly as it did: its own bytes,
// and the uploads folder beside it, which is how a document that never used the
// library travels.
func TestExportDocumentZipExportsADocumentWithoutHostPathsUnchanged(t *testing.T) {
	dir := t.TempDir()
	outDir := t.TempDir()
	library := t.TempDir()
	document := filepath.Join(dir, "board.htmlclay")
	doc := `<!doctype html><img src="assets-board/a.png"><img src="https://x.hyperclay.com/_/uploads/assets-board/a.png">`
	writeExportFile(t, document, doc)
	writeExportFile(t, filepath.Join(dir, "assets-board", "a.png"), "PNGDATA")
	writeExportFile(t, filepath.Join(library, "assets-board", "a.png"), "LIBRARYDATA")

	out, err := ExportDocumentZip(document, outDir, library)
	if err != nil {
		t.Fatalf("ExportDocumentZip: %v", err)
	}
	entries := zipEntries(t, out)
	want := map[string]string{
		"board/board.htmlclay":     doc,
		"board/assets-board/a.png": "PNGDATA",
	}
	if len(entries) != len(want) {
		t.Fatalf("zip holds %v, want exactly %v", exportNames(entries), exportNames(want))
	}
	for name, content := range want {
		if entries[name] != content {
			t.Errorf("zip entry %q = %q, want %q", name, entries[name], content)
		}
	}
}

// The finder's left boundary, directly: /_/uploads/ counts only where it stands
// free. The tail of a host name and the inside of a longer token are not links.
func TestHostUploadRefsLeftBoundary(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want bool
	}{
		{"document start", "/_/uploads/assets-board/a.png", true},
		{"after a double quote", `src="/_/uploads/assets-board/a.png"`, true},
		{"after a single quote", `url('/_/uploads/assets-board/a.png')`, true},
		{"after a parenthesis", `(/_/uploads/assets-board/a.png)`, true},
		{"after a space", `<img src=/_/uploads/assets-board/a.png alt=x>`, true},
		{"after an equals sign", "src=/_/uploads/assets-board/a.png", true},
		{"after a newline", "\n/_/uploads/assets-board/a.png", true},
		{"after a host name", "https://x.hyperclay.com/_/uploads/assets-board/a.png", false},
		{"after a path segment", "https://x.hyperclay.com/board/_/uploads/assets-board/a.png", false},
		{"after a letter", "a/_/uploads/assets-board/a.png", false},
		{"after a digit", "1/_/uploads/assets-board/a.png", false},
		{"after a hyphen", "x-/_/uploads/assets-board/a.png", false},
		{"after an underscore", "x_/_/uploads/assets-board/a.png", false},
		{"after a dot", "./_/uploads/assets-board/a.png", false},
		{"after a tilde", "~/_/uploads/assets-board/a.png", false},
		{"after a percent", "%/_/uploads/assets-board/a.png", false},
		{"after a slash", "//_/uploads/assets-board/a.png", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hostUploadRefs([]byte(tc.doc))
			if !tc.want {
				if len(got) != 0 {
					t.Fatalf("hostUploadRefs(%q) = %+v, want no ref", tc.doc, got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("hostUploadRefs(%q) = %+v, want one ref", tc.doc, got)
			}
			if text := tc.doc[got[0].start:got[0].end]; text != "/_/uploads/assets-board/a.png" {
				t.Errorf("hostUploadRefs(%q) read %q, want the reference alone", tc.doc, text)
			}
			if rel := strings.Join(got[0].segments, "/"); rel != "assets-board/a.png" {
				t.Errorf("hostUploadRefs(%q) segments = %q, want assets-board/a.png", tc.doc, rel)
			}
		})
	}
}

// The finder's right extent: the reference runs over the bytes a URL path holds
// and stops at the first one it does not, so a query, a fragment, a quote or a
// tag is not dragged into the name.
func TestHostUploadRefsRightExtent(t *testing.T) {
	const ref = "/_/uploads/assets-board/b%20c.pdf"
	for _, stop := range []string{`"`, "'", ")", " ", "?", "#", "<", "&", "\\", "|", ",", "="} {
		doc := "x " + ref + stop + "tail"
		got := hostUploadRefs([]byte(doc))
		if len(got) != 1 {
			t.Fatalf("hostUploadRefs(%q) = %+v, want one ref", doc, got)
		}
		if text := doc[got[0].start:got[0].end]; text != ref {
			t.Errorf("hostUploadRefs(%q) read %q, want %q", doc, text, ref)
		}
		if rel := strings.Join(got[0].segments, "/"); rel != "assets-board/b c.pdf" {
			t.Errorf("hostUploadRefs(%q) segments = %q, want the unescaped name", doc, rel)
		}
	}
}

// A folder and a name is a library file, and a deeper path is one too: the whole
// reference is read, escapes and all, with the name left as the document wrote it.
func TestHostUploadRefsAcceptsALibraryPath(t *testing.T) {
	for _, doc := range []string{
		"/_/uploads/assets-board/a.png",
		"/_/uploads/assets-board/sub/a.png",
		"/_/uploads/assets-board/b%20c.pdf",
	} {
		got := hostUploadRefs([]byte(doc))
		if len(got) != 1 || got[0].start != 0 || got[0].end != len(doc) {
			t.Fatalf("hostUploadRefs(%q) = %+v, want one reference over the whole path", doc, got)
		}
	}
}

// Everything that is not a library file is refused before any disk is touched: a
// flat name with no folder, an empty segment, a hidden name, an escape out of the
// library in either spelling, an escaped separator, and an escape that is not one.
func TestHostUploadRefsRefusesWhatIsNotALibraryFile(t *testing.T) {
	for _, doc := range []string{
		"/_/uploads/a.png",
		"/_/uploads/assets-board/",
		"/_/uploads/assets-board//a.png",
		"/_/uploads/assets-board/../a.png",
		"/_/uploads/assets-board/%2e%2e/a.png",
		"/_/uploads/assets-board/%2e/a.png",
		"/_/uploads/.hidden/a.png",
		"/_/uploads/assets-board/.hidden.png",
		"/_/uploads/assets-board%2Fa.png",
		"/_/uploads/assets-board%5Ca.png",
		"/_/uploads/assets-board/%zz.png",
	} {
		if got := hostUploadRefs([]byte(doc)); len(got) != 0 {
			t.Errorf("hostUploadRefs(%q) = %+v, want no ref", doc, got)
		}
	}
}
