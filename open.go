package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/panphora/htmlclay/browser"
	"github.com/panphora/htmlclay/session"
)

func (a *app) openFile(filePath string) {
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		a.rt.logger.Printf("Error resolving path: %v", err)
		return
	}

	if _, err := os.Stat(absPath); os.IsNotExist(err) {
		a.rt.logger.Printf("File not found: %s", absPath)
		return
	}

	absPath, err = resolveSymlinks(absPath)
	if err != nil {
		a.rt.logger.Printf("Error resolving symlinks: %v", err)
		return
	}

	// Refuse an out-of-home file before any site exists. Registration would
	// reject it anyway, but by then a port was bound and a server started.
	if _, ok := session.ContainWithinHome(a.rt.home, absPath); !ok {
		a.rt.logger.Printf("Refusing file outside home: %s", absPath)
		msg := fmt.Sprintf("%s is outside your home folder. HTML Clay only opens files inside %s.",
			filepath.Base(absPath), a.rt.home)
		go func() {
			if nErr := a.notifyUser("HTML Clay can't open this file", msg); nErr != nil {
				a.rt.logger.Printf("Could not show notification: %v", nErr)
			}
		}()
		return
	}

	s, rel, ok := a.route(absPath, session.ViaOsOpen)
	if !ok {
		return
	}

	target := fileURL(s.port, rel)
	a.rt.logger.Printf("Serving %s at %s", filepath.Base(absPath), target)
	a.launchBrowser(target)
}

func ensureExampleFile(path string) error {
	_, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		if mkErr := os.MkdirAll(filepath.Dir(path), 0755); mkErr != nil {
			return mkErr
		}
		return os.WriteFile(path, exampleHTML, 0644)
	}
	return err
}

func (a *app) openExample() {
	home, err := os.UserHomeDir()
	if err != nil {
		a.rt.logger.Printf("Error resolving home dir: %v", err)
		return
	}
	path := filepath.Join(home, "htmlclay", "examples", "welcome.htmlclay")
	if err := ensureExampleFile(path); err != nil {
		a.rt.logger.Printf("Error creating example file: %v", err)
		return
	}
	a.openFile(path)
}

// openBackups opens the versions folder in the platform file manager. This is the
// discovery mechanism that makes the plain-folder backup design usable: a version
// can be double-clicked straight from Finder.
func (a *app) openBackups() {
	dir, err := a.rt.versions.Dir()
	if err != nil {
		a.rt.logger.Printf("Error resolving backups dir: %v", err)
		return
	}
	if err := browser.OpenURL(dir); err != nil {
		a.rt.logger.Printf("Error opening backups folder: %v", err)
	}
}

func (a *app) launchBrowser(targetURL string) {
	a.rt.logger.Printf("Opening in default browser: %s", targetURL)
	if err := browser.OpenURL(targetURL); err != nil {
		a.rt.logger.Printf("Error opening browser: %v", err)
	}
}
