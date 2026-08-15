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
	"strings"

	"github.com/panphora/htmlclay/platform"
	"github.com/panphora/htmlclay/session"
)

// equalOrUnderFold is EqualOrUnder with capitalization always ignored, whatever
// the disk underneath does.
//
// The tables below are a rule about which folders a person keeps their life in,
// not a statement about path identity, and two spellings of Documents are the
// same folder to whoever reads the dialog. session.EqualOrUnder follows the home
// volume's own rule instead, which is correct for containment and wrong here: on
// a case-sensitive disk it refuses ~/Documents while letting ~/documents past,
// and on Linux a lowercase personal folder is an ordinary setup rather than a
// trick. Refusing both spellings costs nothing, because the tray picker ignores
// these rules entirely and can still trust either one deliberately.
func equalOrUnderFold(path, root string) bool {
	return session.EqualOrUnder(strings.ToLower(path), strings.ToLower(root))
}

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
	"Dropbox", "Google Drive", "Nextcloud", "Sync", "Box", "OneDrive",
}

// syncRootPrefixes match a folder by the start of its name rather than the whole
// of it, because a work or school OneDrive mounts as "OneDrive - <Organisation>"
// and the organisation is not knowable from here. Matched case-folded against
// what is actually in the home directory, so the table does not have to guess.
var syncRootPrefixes = []string{"onedrive"}

// redirectedNames are the personal folders Windows moves INSIDE a sync root when
// folder backup is on, which is the default on Windows 11. There ~/Documents
// frequently does not exist at all and the user's real documents live at
// ~/OneDrive/Documents, so a sync root's children by these names carry the same
// protection the top-level names do. Without this the table would guard a folder
// that is not there and miss the one that is.
var redirectedNames = []string{"Desktop", "Documents", "Pictures"}

// ownFolderExtra are refused on the RefuseOwnFolder route only. A file sitting
// directly in ~/Documents/GitHub asking to trust its own folder would take the
// whole checkout tree; one level further down is an ordinary project.
var ownFolderExtra = [][]string{{"Documents", "GitHub"}}

// personalDirs resolves the tables against the home directory that is really on
// disk: every fixed name, plus any folder whose name marks it as a sync root.
// The second return is just the sync roots, because their children need the
// treatment redirectedNames describes.
//
// Reading the directory is safe here in a way it would not be on the
// auto-registration path: these rules only ever run behind a dialog the user is
// about to see, never as part of deciding what to serve, so nothing a page can
// time is affected by what home contains.
func (p Policy) personalDirs() (dirs []string, syncRoots []string) {
	listed := map[string]bool{}
	for _, name := range personalNames {
		dir := filepath.Join(p.Home, name)
		dirs = append(dirs, dir)
		listed[strings.ToLower(name)] = true
		if isSyncRootName(name) {
			syncRoots = append(syncRoots, dir)
		}
	}
	entries, err := os.ReadDir(p.Home)
	if err != nil {
		return dirs, syncRoots
	}
	for _, entry := range entries {
		name := entry.Name()
		if listed[strings.ToLower(name)] || !isSyncRootName(name) {
			continue
		}
		dir := filepath.Join(p.Home, name)
		dirs = append(dirs, dir)
		syncRoots = append(syncRoots, dir)
	}
	return dirs, syncRoots
}

// personalAndRedirected is personalDirs plus the personal folders redirected
// into a sync root. It is what "one of your main personal folders" means once
// the folder backup case is taken seriously, so the two rules that ask that
// question and the tray's warning all read the same list.
func (p Policy) personalAndRedirected() []string {
	dirs, syncRoots := p.personalDirs()
	for _, root := range syncRoots {
		for _, name := range redirectedNames {
			dirs = append(dirs, filepath.Join(root, name))
		}
	}
	return dirs
}

func isSyncRootName(name string) bool {
	lower := strings.ToLower(name)
	for _, prefix := range syncRootPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func resolve(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

// onDiskSpelling replaces path with the capitalization the filesystem itself
// reports for it, asked of the OS by handle.
//
// EvalSymlinks keeps the caller's spelling of every component that is not a
// symlink, and on the read-prompt route the PAGE steers that spelling by choosing
// which asset it asks for. Without this, one directory could be stored as two
// trusted folders under two casings, and untrusting the one the user recognizes
// left the other granting write over the same tree.
//
// The answer is accepted only when it still names the same directory in both
// directions. A handle-to-path lookup may re-spell the path; it must never be
// able to redirect the grant somewhere else, so an unexpected form (a substituted
// drive, a Windows device path) falls back to the input rather than moving it.
func onDiskSpelling(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return path
	}
	defer f.Close()
	actual, err := platform.RealPath(f)
	if err != nil {
		return path
	}
	actual = filepath.Clean(actual)
	if !session.EqualOrUnder(actual, path) || !session.EqualOrUnder(path, actual) {
		return path
	}
	return actual
}

// Canonical resolves and validates a folder the user asked to trust, returning
// the canonical path to store. The folder must resolve, sit strictly inside
// home (so home itself is refused), carry no hidden component, and not be
// HTML Clay's own config/versions tree. Storing the same canonical form the
// session manager keys its roots on is what keeps live-revoke able to find the
// root later.
//
// It is also where one directory is reduced to one spelling: every door that
// records a trusted folder passes through here, so normalizing the
// capitalization at this single point is what stops the list, the remembered
// ports, and siteAtLocked from each seeing two folders where there is one.
func (p Policy) Canonical(dir string) (string, error) {
	resolved, err := resolve(dir)
	if err != nil {
		return "", fmt.Errorf("cannot resolve folder: %w", err)
	}
	resolved = onDiskSpelling(resolved)
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
// Documents. One click would then durably trust everything the user owns.
//
// Note what that means in practice, because it is broader than it first reads
// and the honest case does pay for it: ANYTHING under a personal folder is
// refused on this route, so someone whose projects live in ~/Documents or
// ~/Desktop never sees this button. The other two doors are still open to them,
// the banner on a file they opened (RefuseOwnFolder, which allows exactly that
// case) and the tray picker, which consults none of this because choosing a
// folder from a picker is already a deliberate act.
//
// dir arrives already symlink-resolved, so each name is compared in both its
// lexical and its resolved form. A Downloads or Documents folder that is
// itself a symlink, pointing at an external drive or a synced folder, is an
// ordinary setup, and it would otherwise reach here under its target's name
// and sail past a purely lexical match. Folders the user has renamed outright
// are still not recognized.
func (p Policy) RefuseSteered(dir string) bool {
	personal, _ := p.personalDirs()
	for _, lexical := range personal {
		forms := []string{lexical}
		if resolved, err := filepath.EvalSymlinks(lexical); err == nil {
			if cleaned := filepath.Clean(resolved); cleaned != lexical {
				forms = append(forms, cleaned)
			}
		}
		for _, d := range forms {
			if equalOrUnderFold(dir, d) || equalOrUnderFold(d, dir) {
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
	targets := p.personalAndRedirected()
	for _, parts := range ownFolderExtra {
		targets = append(targets, filepath.Join(append([]string{p.Home}, parts...)...))
	}

	dirInfo, dirErr := os.Stat(dir)
	for _, target := range targets {
		if equalOrUnderFold(target, dir) {
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
	for _, target := range p.personalAndRedirected() {
		if equalOrUnderFold(target, dir) && equalOrUnderFold(dir, target) {
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
