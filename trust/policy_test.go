package trust

import (
	"os"
	"path/filepath"
	"testing"
)

// The two refusals and the tray's warning, side by side, which is the shape that
// makes an asymmetry between them visible. They are deliberately different:
// RefuseSteered is the strict one, for the route where a PAGE aims the folder,
// and it refuses anything under a personal folder as well as the folder itself;
// RefuseOwnFolder is the looser one, for the route where the folder is pinned to
// a file the user opened, so an ordinary project inside a personal folder stays
// trustable.
//
// SECURITY.md promises both page routes refuse "your main personal folders …
// plus Dropbox and the other sync folders". v1.3.0 shipped without OneDrive,
// which is the most widely deployed sync folder there is, so one click could
// durably write-trust the whole tree while Dropbox beside it was refused. On
// Windows with folder backup on, the default since Windows 11, that tree also
// holds the real Desktop, Documents and Pictures, and ~/Documents often does not
// exist at all — so the table was guarding a folder that was not there and
// missing the one that was.
func TestPersonalAndSyncFolderRefusals(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{
		"OneDrive/Documents",
		"OneDrive/project",
		"OneDrive - Contoso/Documents",
		"OneDrive - Contoso/project",
		"Dropbox/myproject",
		"Documents/GitHub/myproject",
		"code/site",
	} {
		if err := os.MkdirAll(filepath.Join(home, filepath.FromSlash(rel)), 0755); err != nil {
			t.Fatal(err)
		}
	}
	p := Policy{Home: home}

	for _, tc := range []struct {
		rel       string
		steered   bool
		ownFolder bool
		personal  bool
		why       string
	}{
		{"OneDrive", true, true, true, "a sync root is a personal folder"},
		{"OneDrive - Contoso", true, true, true, "a work OneDrive mounts under the organisation's name"},
		{"OneDrive/Documents", true, true, true, "folder backup puts the real Documents here"},
		{"OneDrive - Contoso/Documents", true, true, true, "same, under the work mount"},
		{"OneDrive/project", true, false, false, "an ordinary project in a sync root stays trustable by its own file"},
		{"Dropbox", true, true, true, "the control: unchanged behaviour"},
		{"Dropbox/myproject", true, false, false, "the control: a project in a sync root"},
		{"Documents", true, true, true, "a plain personal folder"},
		{"Documents/GitHub", true, true, false, "the whole checkout tree is too broad for one click"},
		{"Documents/GitHub/myproject", true, false, false, "one level further down is an ordinary project"},
		{"code/site", false, false, false, "an ordinary project outside every personal folder"},
	} {
		dir := filepath.Join(home, filepath.FromSlash(tc.rel))
		if got := p.RefuseSteered(dir); got != tc.steered {
			t.Errorf("RefuseSteered(~/%s) = %v, want %v (%s)", tc.rel, got, tc.steered, tc.why)
		}
		if got := p.RefuseOwnFolder(dir); got != tc.ownFolder {
			t.Errorf("RefuseOwnFolder(~/%s) = %v, want %v (%s)", tc.rel, got, tc.ownFolder, tc.why)
		}
		if got := p.IsPersonal(dir); got != tc.personal {
			t.Errorf("IsPersonal(~/%s) = %v, want %v (%s)", tc.rel, got, tc.personal, tc.why)
		}
	}
}

// A sync root is recognised whether or not it exists on disk, because the fixed
// names come from the table; only the prefix-matched variants need a directory
// to be found in. This pins that a machine with no OneDrive at all still refuses
// the name, so a folder created later cannot be trusted through a stale answer.
func TestSyncRootRefusedEvenWhenAbsentFromDisk(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := Policy{Home: home}
	dir := filepath.Join(home, "OneDrive")
	if !p.RefuseSteered(dir) || !p.RefuseOwnFolder(dir) {
		t.Error("~/OneDrive must be refused on both page routes even when it is not on disk")
	}
}

// Capitalization is not a way around the tables. equalOrUnderFold folds case on
// every platform rather than following the disk, because these names are a rule
// about which folders a person keeps their life in, not a claim about path
// identity: on a case-sensitive disk ~/documents is an ordinary setup, not a
// trick, and it must be refused just the same.
func TestRefusalsFoldCaseOnEveryPlatform(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := Policy{Home: home}
	for _, rel := range []string{"documents", "DOWNLOADS", "onedrive", "dropbox"} {
		dir := filepath.Join(home, rel)
		if !p.RefuseSteered(dir) {
			t.Errorf("RefuseSteered(~/%s) = false, want true", rel)
		}
		if !p.RefuseOwnFolder(dir) {
			t.Errorf("RefuseOwnFolder(~/%s) = false, want true", rel)
		}
	}
}

// A folder that CONTAINS a relocated personal folder is refused on both page
// routes, and the tray picker warns about the relocated folder itself.
//
// Moving Documents to an external drive or a synced folder and leaving a symlink
// behind is an ordinary setup. Before 1.4.0 only RefuseSteered resolved the
// symlink, so a file sitting in ~/relocated could ask to trust its own folder and
// take the whole real Documents tree with one click, and picking the real folder
// from the tray produced no warning at all. All three rules now read the resolved
// forms from personalDirs.
func TestRefusalsSeeAFolderThatContainsARelocatedPersonalFolder(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	relocated := filepath.Join(home, "relocated", "Documents")
	for _, dir := range []string{relocated, filepath.Join(relocated, "project"), filepath.Join(home, "code", "site")} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	// Skip on the error, not on GOOS: Go asks for the unprivileged-create flag, so
	// this often works on a Windows runner, and Windows is the platform where a
	// redirected personal folder is the default rather than the exception.
	if err := os.Symlink(relocated, filepath.Join(home, "Documents")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	p := Policy{Home: home}

	ancestor := filepath.Join(home, "relocated")
	if !p.RefuseOwnFolder(ancestor) {
		t.Error("a folder containing the real Documents must not be trustable by a file inside it")
	}
	if !p.RefuseSteered(ancestor) {
		t.Error("the control: RefuseSteered already caught this and must keep doing so")
	}
	if !p.IsPersonal(relocated) {
		t.Error("the tray picker must warn when the folder picked IS the real Documents")
	}

	for _, tc := range []struct{ dir, why string }{
		{filepath.Join(relocated, "project"), "an ordinary project inside the real Documents stays trustable by its own file"},
		{filepath.Join(home, "code", "site"), "a project outside every personal folder is untouched"},
	} {
		if p.RefuseOwnFolder(tc.dir) {
			t.Errorf("RefuseOwnFolder(%s) = true, want false (%s)", tc.dir, tc.why)
		}
	}
}

// Canonical stores the spelling the filesystem itself reports, so one directory
// can only ever enter the trusted list one way.
//
// EvalSymlinks preserves the caller's casing of every non-symlink component, and
// on the read-prompt route the page chooses that casing by choosing which asset to
// request. Before this, two spellings meant two trusted entries over one folder,
// and untrusting the one the user recognized left the other granting write.
func TestCanonicalStoresTheOnDiskSpelling(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	onDisk := filepath.Join(home, "projects", "Site")
	if err := os.MkdirAll(onDisk, 0755); err != nil {
		t.Fatal(err)
	}
	// Ask this filesystem rather than runtime.GOOS: whether two spellings name one
	// directory is a property of the volume the folder is on, and on a
	// case-sensitive volume they really are two folders and must stay two.
	variant := filepath.Join(home, "projects", "site")
	if _, err := os.Stat(variant); err != nil {
		t.Skip("case-sensitive filesystem: one directory cannot be reached by two spellings")
	}

	got, err := Policy{Home: home}.Canonical(variant)
	if err != nil {
		t.Fatal(err)
	}
	if got != onDisk {
		t.Errorf("Canonical(%q) = %q, want the on-disk spelling %q", variant, got, onDisk)
	}
}

// Home itself is never a trust candidate, whatever the tables say: Canonical
// refuses it before any refusal rule is consulted.
func TestCanonicalRefusesHomeItself(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := Policy{Home: home}
	if _, err := p.Canonical(home); err == nil {
		t.Error("the home directory must not canonicalize as a trust candidate")
	}
}
