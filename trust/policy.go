// Package trust holds the rules about folders the user vouches for.
//
// Everything here is a pure function of a path, the home directory, and the
// config-tree guard: no live site, no session manager, no app mutex, and
// nothing that reads the declared list. That is deliberate. These rules are
// what stands between a page and a durable write grant over a tree, so they
// should be readable and testable without standing up an HTTP server, a port,
// and a temp home first.
//
// The declared list itself stays in package main, next to the sites it
// anchors: adding and removing a folder has to move listeners and
// registrations, so it is app work, not policy.
package trust

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/panphora/htmlclay/platform"
	"github.com/panphora/htmlclay/session"
)

// Policy answers "may this folder be trusted, and on whose say-so".
type Policy struct {
	Home string
	// Guard reports whether dir touches HTML Clay's own config tree, in either
	// direction. The app owns it because only the app knows where that tree is.
	Guard func(dir string) bool
}

// personalNames are the top-level folders in home that hold files the user
// mostly did not write, plus the sync roots, whose contents arrive from other
// machines and other people. Both refusal rules below read this table; they
// differ in the containment direction they test, not in the names.
var personalNames = []string{
	"Desktop", "Documents", "Downloads", "Library", "Movies", "Music",
	"Pictures", "Public",
	"Dropbox", "Google Drive", "Nextcloud", "Sync", "Box",
}

// ownFolderExtra are refused on the RefuseOwnFolder route only. A file sitting
// directly in ~/Documents/GitHub asking to trust its own folder would take the
// whole checkout tree; one level further down is an ordinary project.
var ownFolderExtra = [][]string{{"Documents", "GitHub"}}

func resolve(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

// Canonical resolves and validates a folder the user asked to trust, returning
// the canonical path to store. The folder must resolve, sit strictly inside
// home (so home itself is refused), carry no hidden component, and not be
// HTML Clay's own config/versions tree. Storing the same canonical form the
// session manager keys its roots on is what keeps live-revoke able to find the
// root later.
func (p Policy) Canonical(dir string) (string, error) {
	resolved, err := resolve(dir)
	if err != nil {
		return "", fmt.Errorf("cannot resolve folder: %w", err)
	}
	canonical, ok := session.ContainWithinHome(p.Home, resolved)
	if !ok {
		return "", fmt.Errorf("%s is outside your home folder", resolved)
	}
	if session.HasHiddenComponent(p.Home, canonical) {
		return "", fmt.Errorf("%s is a hidden folder", canonical)
	}
	if p.Guard != nil && p.Guard(canonical) {
		return "", fmt.Errorf("%s is used by HTML Clay and can't be trusted", canonical)
	}
	return canonical, nil
}

// RefuseSteered reports whether dir is a personal folder, sits inside one, or
// is an ancestor that would swallow one.
//
// This is the rule for the route where the PAGE steers which folder is named.
// The read prompt offers to trust the common ancestor of the requesting file
// and the asset it asked for, and a page picks its own assets: two requests
// under different subfolders make their common ancestor the whole of
// Documents. One click would then durably trust everything the user owns. A
// real project folder is essentially never one of these exactly, and a
// subfolder such as ~/Documents/projects/site is still trustable, so this
// costs the honest case nothing. Picking one of these from the tray still
// works, because that takes a deliberate act with a folder picker.
//
// dir arrives already symlink-resolved, so each name is compared in both its
// lexical and its resolved form. A Downloads or Documents folder that is
// itself a symlink, pointing at an external drive or a synced folder, is an
// ordinary setup, and it would otherwise reach here under its target's name
// and sail past a purely lexical match. Folders the user has renamed outright
// are still not recognized.
func (p Policy) RefuseSteered(dir string) bool {
	for _, name := range personalNames {
		lexical := filepath.Join(p.Home, name)
		forms := []string{lexical}
		if resolved, err := filepath.EvalSymlinks(lexical); err == nil {
			if cleaned := filepath.Clean(resolved); cleaned != lexical {
				forms = append(forms, cleaned)
			}
		}
		for _, d := range forms {
			if session.EqualOrUnder(dir, d) || session.EqualOrUnder(d, dir) {
				return true
			}
		}
	}
	return false
}

// RefuseOwnFolder reports whether dir is a personal folder, or an ancestor
// that would swallow one.
//
// This is the rule for the route where the folder is pinned to a file the user
// acted on: it is always the requesting file's own directory, so a page cannot
// inflate it. That makes the looser test correct here, and the stricter one
// wrong: ~/Documents/GitHub/myproject must stay trustable by a file inside it.
// A folder INSIDE a personal folder is therefore allowed, which is the whole
// difference from RefuseSteered.
//
// Comparison is by identity (os.SameFile) as well as case-folded path, so a
// casing alias or a symlinked variant of a personal folder cannot slip through
// as a different spelling.
func (p Policy) RefuseOwnFolder(dir string) bool {
	candidates := make([][]string, 0, len(personalNames)+len(ownFolderExtra))
	for _, name := range personalNames {
		candidates = append(candidates, []string{name})
	}
	candidates = append(candidates, ownFolderExtra...)

	dirInfo, dirErr := os.Stat(dir)
	for _, parts := range candidates {
		target := filepath.Join(append([]string{p.Home}, parts...)...)
		if session.EqualOrUnder(target, dir) {
			return true
		}
		if dirErr == nil {
			if tInfo, err := os.Stat(target); err == nil && os.SameFile(dirInfo, tInfo) {
				return true
			}
		}
	}
	return false
}

// IsPersonal reports whether dir is exactly one of the personal folders. The
// tray picker uses it to warn before trusting one; unlike the two rules above
// it refuses nothing, because a deliberate act with a folder picker may choose
// anything Canonical accepts.
func (p Policy) IsPersonal(dir string) bool {
	for _, name := range personalNames {
		if session.EqualOrUnder(filepath.Join(p.Home, name), dir) &&
			session.EqualOrUnder(dir, filepath.Join(p.Home, name)) {
			return true
		}
	}
	return false
}

// IdentityOK reports whether the directory at path is still provably the one
// that was declared. An empty pin (a platform without fingerprints) leaves the
// path as the entry's whole identity.
func IdentityOK(path, pinned string) bool {
	if pinned == "" {
		return true
	}
	return platform.DirIdentity(path) == pinned
}
