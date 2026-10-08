package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// The export menu lists documents, not helper bindings: a document that declares
// no program is still a document the user can export, and a document opened by a
// bookmark counts because it is registered exactly like an OS-open one.
func TestExportDocumentRowsListEveryOpenDocumentWithoutHelperFiltering(t *testing.T) {
	home, err := resolveSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	folder := filepath.Join(home, "proj")
	declaring := filepath.Join(folder, "page.htmlclay")
	writeTestFile(t, declaring, `<!doctype html><head><meta name="htmlclay-helper" content="search"></head>`)
	quiet := filepath.Join(folder, "quiet.htmlclay")
	writeTestFile(t, quiet, `<!doctype html><head></head>`)
	never := filepath.Join(folder, "never.htmlclay")
	writeTestFile(t, never, `<!doctype html><head></head>`)

	a := newTestApp(t, home)
	if err := a.trustFolder(folder); err != nil {
		t.Fatal(err)
	}
	s := liveSite(t, a, folder)
	if status, body := fetchNav(t, fileURL(s.port, filepath.Join("proj", "page.htmlclay"))); status != 200 {
		t.Fatalf("bookmark navigation = %d: %s", status, body)
	}
	a.openForTest(t, quiet)

	rows := a.exportDocumentRows()
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want one per open document", rows)
	}
	if rows[0].Path != declaring || rows[0].Label != "~/proj/page.htmlclay" {
		t.Errorf("first row = %+v, want the bookmarked document labelled with its ~/ path", rows[0])
	}
	if rows[1].Path != quiet || rows[1].Label != "~/proj/quiet.htmlclay" {
		t.Errorf("second row = %+v, want the opened document labelled with its ~/ path", rows[1])
	}
	if _, ok := helperRowByPath(rows, never); ok {
		t.Errorf("rows = %+v, want no row for a document that was never opened", rows)
	}
}

// A row can be clicked after the document it names was closed. That export must
// be refused with a notification and must not touch the disk: half an export, or
// a Downloads folder the user never asked for, is worse than a message.
func TestExportDocumentRefusesAFileThatIsNoLongerOpen(t *testing.T) {
	home, err := resolveSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	folder := filepath.Join(home, "proj")
	document := filepath.Join(folder, "board.htmlclay")
	writeTestFile(t, document, "<!doctype html><p>board</p>")

	a := newTestApp(t, home)
	var told []string
	a.rt.notify = func(title, message string) error {
		told = append(told, title+": "+message)
		return nil
	}

	rows := a.exportDocument(document)
	if len(rows) != 0 {
		t.Errorf("rows = %+v, want no open documents", rows)
	}
	if len(told) != 1 || told[0] != "HTML Clay: That file is no longer open." {
		t.Errorf("told = %q, want the refusal alone", told)
	}
	if _, err := os.Stat(filepath.Join(home, "Downloads")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a refused export must not create the Downloads folder: %v", err)
	}
}
