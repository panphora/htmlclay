package main

import (
	"os"
	"path/filepath"
	"slices"

	"github.com/panphora/htmlclay/internal/browser"
	"github.com/panphora/htmlclay/internal/server"
	"github.com/panphora/htmlclay/internal/session"
	"github.com/panphora/htmlclay/internal/tray"
)

// exportDocumentRows lists every document open in HTML Clay, labelled with a
// ~/ path, for the Export as Zip submenu.
func (a *app) exportDocumentRows() []tray.Row {
	a.mu.Lock()
	documents := make(map[string]bool)
	if !a.stopping {
		for _, s := range a.sites {
			for _, registration := range s.sessions.Registrations() {
				documents[registration.Path] = true
			}
		}
	}
	a.mu.Unlock()
	paths := make([]string, 0, len(documents))
	for document := range documents {
		paths = append(paths, document)
	}
	slices.Sort(paths)
	var rows []tray.Row
	for _, document := range paths {
		label := document
		if rel, err := filepath.Rel(a.rt.home, document); err == nil && document != a.rt.home && session.EqualOrUnder(document, a.rt.home) {
			label = "~/" + filepath.ToSlash(rel)
		}
		rows = append(rows, tray.Row{Path: document, Label: label})
	}
	return rows
}

// exportDocument zips one open document into ~/Downloads, opens that folder and
// says so in a notification. A document closed since the menu was drawn is
// refused with a notification instead.
func (a *app) exportDocument(path string) []tray.Row {
	a.mu.Lock()
	open := a.helperDocumentOpenLocked(path)
	a.mu.Unlock()
	if !open {
		a.notifyExport("That file is no longer open.")
		return a.exportDocumentRows()
	}
	downloads := filepath.Join(a.rt.home, "Downloads")
	if err := os.MkdirAll(downloads, 0o755); err != nil {
		return a.exportDocumentFailed(path, err)
	}
	out, err := server.ExportDocumentZip(path, downloads)
	if err != nil {
		return a.exportDocumentFailed(path, err)
	}
	if err := browser.OpenURL(downloads); err != nil {
		a.rt.logger.Printf("Error opening the Downloads folder: %v", err)
	}
	a.notifyExport("Saved " + filepath.Base(out) + " in Downloads.")
	return a.exportDocumentRows()
}

// exportDocumentFailed reports an export that could not be written, by name, and
// hands back the list the submenu should show now.
func (a *app) exportDocumentFailed(path string, err error) []tray.Row {
	a.rt.logger.Printf("Could not export %s: %v", path, err)
	a.notifyExport("Couldn't export " + filepath.Base(path) + ".")
	return a.exportDocumentRows()
}

// notifyExport sends a best-effort native message through the runtime seam, the
// same way every other notification does, so a test asserting what the user was
// told never puts a real banner on their screen.
func (a *app) notifyExport(message string) {
	if err := a.notifyUser("HTML Clay", message); err != nil {
		a.rt.logger.Printf("Could not notify about the export: %v", err)
	}
}
