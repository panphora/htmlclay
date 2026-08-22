package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// noIdentity stands in for a platform that cannot fingerprint a directory, which
// is what most of these tests want: the pin is irrelevant to what they assert.
func noIdentity(string) string { return "" }

func TestLoadDefaults(t *testing.T) {
	baseDir := t.TempDir()
	cfg, res, err := LoadFrom(baseDir, noIdentity)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.StartOnLogin != false {
		t.Error("expected StartOnLogin false")
	}
	if got := cfg.TrustedFolderList(); len(got) != 0 {
		t.Errorf("expected no trusted folders, got %v", got)
	}
	if res.HadAppMode || res.PromotedLegacy {
		t.Errorf("a fresh config should need no migration, got %+v", res)
	}
}

func TestSaveAndLoad(t *testing.T) {
	baseDir := t.TempDir()
	cfg, _, _ := LoadFrom(baseDir, noIdentity)
	cfg.SetStartOnLogin(true)
	cfg.RememberSitePort("/root/sites", 12345)
	if err := cfg.Save(); err != nil {
		t.Fatalf("save error: %v", err)
	}

	loaded, _, err := LoadFrom(baseDir, noIdentity)
	if err != nil {
		t.Fatalf("load error: %v", err)
	}
	if loaded.StartOnLogin != true {
		t.Error("expected StartOnLogin true")
	}
	if got := loaded.SitePort("/root/sites"); got != 12345 {
		t.Errorf("expected remembered port 12345, got %d", got)
	}
}

func TestLoadCorruptRecoversToDefaults(t *testing.T) {
	baseDir := t.TempDir()
	dir := DirFrom(baseDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{not valid json"), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := LoadFrom(baseDir, noIdentity)
	if err != nil {
		t.Fatalf("a corrupt config should not error, got: %v", err)
	}
	if cfg.StartOnLogin != false {
		t.Error("expected the default StartOnLogin false")
	}
	if got := cfg.TrustedFolderList(); len(got) != 0 {
		t.Errorf("expected the default empty trusted list, got %v", got)
	}
}

// A corrupt config must not brick startup, and it must not silently erase what
// the user granted either: the bad file is moved aside, so it is recoverable and
// so the user can be told, rather than overwritten by the next Save.
func TestCorruptConfigIsQuarantinedNotErased(t *testing.T) {
	baseDir := t.TempDir()
	dir := DirFrom(baseDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"trustedFolders": "not-an-array"`), 0600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := LoadFrom(baseDir, noIdentity); err != nil {
		t.Fatalf("a corrupt config should not error, got: %v", err)
	}

	matches, err := filepath.Glob(filepath.Join(dir, "config.json.corrupt-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly one quarantined copy, got %v", matches)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the corrupt config.json must be moved aside, not left in place (stat err = %v)", err)
	}
}

func TestSaveIsAtomicNoTempLeft(t *testing.T) {
	baseDir := t.TempDir()
	cfg, _, _ := LoadFrom(baseDir, noIdentity)
	cfg.SetStartOnLogin(true)
	if err := cfg.Save(); err != nil {
		t.Fatalf("save error: %v", err)
	}

	entries, err := os.ReadDir(DirFrom(baseDir))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".config-") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}

	info, err := os.Stat(filepath.Join(DirFrom(baseDir), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Errorf("expected config.json mode 0600, got %v", info.Mode().Perm())
	}
}

func TestEnsureDir(t *testing.T) {
	baseDir := t.TempDir()
	dir := DirFrom(baseDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("MkdirAll error: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("dir not created: %v", err)
	}
	if !info.IsDir() {
		t.Error("expected directory")
	}
}

func TestTrustedFolderAddRemoveRoundTrip(t *testing.T) {
	baseDir := t.TempDir()
	cfg, _, _ := LoadFrom(baseDir, noIdentity)

	dirA := filepath.Join(baseDir, "sites")
	dirB := filepath.Join(baseDir, "projects")
	if err := os.MkdirAll(dirA, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dirB, 0755); err != nil {
		t.Fatal(err)
	}

	if !cfg.AddTrustedFolder(dirA, "") {
		t.Error("adding a new folder should report added")
	}
	if cfg.AddTrustedFolder(dirA, "") {
		t.Error("adding a duplicate should report not-added")
	}
	cfg.AddTrustedFolder(dirB, "")
	if err := cfg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, _, err := LoadFrom(baseDir, noIdentity)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded.TrustedFolderList()) != 2 {
		t.Fatalf("expected 2 trusted folders after reload, got %v", loaded.TrustedFolderList())
	}

	if _, ok := loaded.RemoveTrustedFolder(dirA); !ok {
		t.Error("removing a present folder should report removed")
	}
	if _, ok := loaded.RemoveTrustedFolder(dirA); ok {
		t.Error("removing an absent folder should report not-removed")
	}
	if err := loaded.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	reloaded, _, _ := LoadFrom(baseDir, noIdentity)
	list := reloaded.TrustedFolderList()
	if len(list) != 1 || list[0].Path != dirB {
		t.Errorf("expected only %q to remain, got %v", dirB, list)
	}
}

// A trusted folder whose directory is gone SURVIVES a save/load round trip. The
// entry is the record of a standing write grant, so it must surface as dead in
// the tray rather than silently vanish; pruning it on load erased what the user
// granted the moment a volume was unmounted.
func TestMissingTrustedFolderSurvivesLoad(t *testing.T) {
	baseDir := t.TempDir()
	cfg, _, _ := LoadFrom(baseDir, noIdentity)

	real := filepath.Join(baseDir, "real")
	if err := os.MkdirAll(real, 0755); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(baseDir, "deleted")
	cfg.AddTrustedFolder(real, "")
	cfg.AddTrustedFolder(gone, "")
	if err := cfg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, _, err := LoadFrom(baseDir, noIdentity)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	list := loaded.TrustedFolderList()
	if len(list) != 2 {
		t.Fatalf("load must not prune the missing folder, got %v", list)
	}
	byPath := map[string]bool{}
	for _, tf := range list {
		byPath[tf.Path] = true
	}
	if !byPath[real] || !byPath[gone] {
		t.Errorf("both entries must survive, got %v", list)
	}
}

// The one Config is shared across the route, tray, and Trusted-Folders goroutines.
// Before the mutex, a SitePorts write concurrent with Save's marshal panicked with
// "concurrent map iteration and map write", and a TrustedFolders append tore under
// marshal. Run under -race; it must be clean and must not panic.
func TestConcurrentMutatorsAndSaveAreRaceFree(t *testing.T) {
	baseDir := t.TempDir()
	cfg, _, _ := LoadFrom(baseDir, noIdentity)

	const iters = 300
	var wg sync.WaitGroup
	run := func(f func(i int)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				f(i)
			}
		}()
	}

	run(func(i int) {
		cfg.RememberSitePort(fmt.Sprintf("/root/%d", i%8), i)
		_ = cfg.Save()
	})
	run(func(i int) {
		d := fmt.Sprintf("/trusted/%d", i%8)
		if !cfg.AddTrustedFolder(d, fmt.Sprintf("id:%d", i)) {
			cfg.RemoveTrustedFolder(d)
		}
		_ = cfg.Save()
	})
	run(func(i int) {
		cfg.SetStartOnLogin(i%2 == 0)
		cfg.SetTrustedIdentity(fmt.Sprintf("/trusted/%d", i%8), fmt.Sprintf("re:%d", i))
		_ = cfg.Save()
	})
	run(func(i int) {
		_ = cfg.StartOnLoginEnabled()
		_ = cfg.SitePort("/root/1")
		_ = cfg.SitePortList()
		_ = cfg.TrustedFolderList()
	})

	wg.Wait()
}

// Trusted folders round-trip with their identity fingerprints, and dead entries
// survive Load: a trusted folder is a standing write grant, and the record of it
// must not silently vanish because the directory is momentarily missing.
func TestTrustedFoldersRoundTripAndNoPrune(t *testing.T) {
	base := t.TempDir()
	cfg, _, err := LoadFrom(base, noIdentity)
	if err != nil {
		t.Fatal(err)
	}

	live := t.TempDir()
	dead := filepath.Join(t.TempDir(), "gone")

	if !cfg.AddTrustedFolder(live, "1:42") {
		t.Fatal("first add reported already-present")
	}
	if cfg.AddTrustedFolder(live, "1:42") {
		t.Fatal("duplicate add reported success")
	}
	if !cfg.AddTrustedFolder(dead, "9:99") {
		t.Fatal("second add failed")
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	loaded, _, err := LoadFrom(base, noIdentity)
	if err != nil {
		t.Fatal(err)
	}
	list := loaded.TrustedFolderList()
	if len(list) != 2 {
		t.Fatalf("got %d trusted folders after load, want 2 (dead entries must NOT be pruned)", len(list))
	}
	byPath := map[string]string{}
	for _, tf := range list {
		byPath[tf.Path] = tf.Identity
	}
	if byPath[live] != "1:42" || byPath[dead] != "9:99" {
		t.Fatalf("identities did not round-trip: %v", byPath)
	}

	if _, ok := loaded.RemoveTrustedFolder(dead); !ok {
		t.Fatal("remove reported not-present")
	}
	if _, ok := loaded.RemoveTrustedFolder(dead); ok {
		t.Fatal("second remove reported success")
	}
	if got := len(loaded.TrustedFolderList()); got != 1 {
		t.Fatalf("got %d after remove, want 1", got)
	}
}

// A 1.2.0 config's read-only trusted folders are widened into trusted folders
// once, on Load, and the key is dropped by the next Save. The regression that
// matters is the rest of the file: a migration that rebuilt the config from
// defaults promoted the folders and reset every unrelated setting with them.
func TestLegacyTrustedFoldersArePromoted(t *testing.T) {
	base := t.TempDir()
	dir := DirFrom(base)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	legacy := t.TempDir()

	raw, err := json.Marshal(map[string]any{
		"mode":           "app",
		"startOnLogin":   true,
		"port":           0,
		"trustedFolders": []string{legacy},
		"sitePorts":      map[string]int{legacy: 51000},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}

	cfg, res, err := LoadFrom(base, func(d string) string { return "id:" + d })
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	list := cfg.TrustedFolderList()
	if len(list) != 1 || list[0].Path != legacy {
		t.Fatalf("legacy folder was not promoted: %v", list)
	}
	if list[0].Identity != "id:"+legacy {
		t.Errorf("a promoted folder must be pinned to the directory that is there now, got %q", list[0].Identity)
	}
	if !res.PromotedLegacy {
		t.Error("Result.PromotedLegacy must report the one-time widening")
	}
	if !res.HadAppMode {
		t.Error("Result.HadAppMode must report a config that still carried App Mode")
	}
	if !cfg.StartOnLoginEnabled() {
		t.Error("the migration reset StartOnLogin")
	}
	if got := cfg.SitePort(legacy); got != 51000 {
		t.Errorf("the migration dropped the remembered port: got %d, want 51000", got)
	}

	if err := cfg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk map[string]json.RawMessage
	if err := json.Unmarshal(data, &onDisk); err != nil {
		t.Fatal(err)
	}
	if _, ok := onDisk["trustedFolders"]; ok {
		t.Errorf("the legacy key must be gone after Save: %s", data)
	}
	if _, ok := onDisk["workspaceFolders"]; !ok {
		t.Errorf("the promoted folders must be written under the merged key: %s", data)
	}
}

// Every remembered port becomes a bound listener at startup, so the map is
// capped. A trusted folder's entry is never evicted: it is the bookmark
// contract, and losing it moves an origin the user has bookmarked.
func TestSitePortsAreCappedButTrustedFoldersSurvive(t *testing.T) {
	base := t.TempDir()
	cfg, _, err := LoadFrom(base, noIdentity)
	if err != nil {
		t.Fatal(err)
	}

	trusted := t.TempDir()
	cfg.AddTrustedFolder(trusted, "")
	cfg.RememberSitePort(trusted, 51000)
	for i := 0; i < 40; i++ {
		cfg.RememberSitePort(filepath.Join(base, fmt.Sprintf("adhoc-%02d", i)), 40000+i)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	loaded, _, err := LoadFrom(base, noIdentity)
	if err != nil {
		t.Fatal(err)
	}
	ports := loaded.SitePortList()
	if len(ports) > sitePortCap {
		t.Errorf("remembered ports = %d, want at most %d", len(ports), sitePortCap)
	}
	if got := ports[trusted]; got != 51000 {
		t.Errorf("a trusted folder's port must never be evicted: got %d, want 51000", got)
	}
}

// A config that already holds one folder under two spellings is healed on load,
// keeping the first entry and its pin.
//
// v1.3.0 could write this: the list compared paths byte for byte, and the read
// prompt's Trust button let a page choose the casing by choosing which asset it
// asked for. Untrusting the spelling the user recognized then left the other entry
// granting write over the same tree, so normalizing new entries is not enough on
// its own; the ones already on disk have to go.
func TestDuplicateTrustedSpellingsAreHealedOnLoad(t *testing.T) {
	baseDir := t.TempDir()
	work := t.TempDir()
	onDisk := filepath.Join(work, "Site")
	if err := os.MkdirAll(onDisk, 0755); err != nil {
		t.Fatal(err)
	}
	// The volume decides whether these are one directory or two, so ask it rather
	// than assuming from runtime.GOOS. On a case-sensitive filesystem they really
	// are two folders and both entries must survive.
	variant := filepath.Join(work, "site")
	if _, err := os.Stat(variant); err != nil {
		t.Skip("case-sensitive filesystem: one directory cannot be reached by two spellings")
	}

	cfg, _, err := LoadFrom(baseDir, noIdentity)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AddTrustedFolder(onDisk, "pin-for-the-real-one")
	cfg.AddTrustedFolder(variant, "pin-added-by-a-page")
	if got := len(cfg.TrustedFolderList()); got != 2 {
		t.Fatalf("precondition: the mutators are byte-exact, so both spellings should be present, got %d", got)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	reloaded, _, err := LoadFrom(baseDir, noIdentity)
	if err != nil {
		t.Fatal(err)
	}
	list := reloaded.TrustedFolderList()
	if len(list) != 1 {
		t.Fatalf("one directory must hold one entry after a load, got %d: %v", len(list), list)
	}
	if list[0].Path != onDisk || list[0].Identity != "pin-for-the-real-one" {
		t.Errorf("the first entry and its pin must be the survivor, got %+v", list[0])
	}
}

// A dead entry stats nothing, so it can only ever match its own byte-exact path.
// Two dead entries stay two, because the tray is the record of what was granted
// and merging them would quietly drop one.
func TestDeadTrustedEntriesAreNotMergedOnLoad(t *testing.T) {
	baseDir := t.TempDir()
	cfg, _, err := LoadFrom(baseDir, noIdentity)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AddTrustedFolder(filepath.Join(t.TempDir(), "gone-a"), "a")
	cfg.AddTrustedFolder(filepath.Join(t.TempDir(), "gone-b"), "b")
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded, _, err := LoadFrom(baseDir, noIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(reloaded.TrustedFolderList()); got != 2 {
		t.Errorf("two missing folders are two dead entries, got %d", got)
	}
}

// A Windows config written before DirIdentity answered there carries trusted
// folders with no pin. They must keep working and quietly gain one, because an
// unpinned entry is a standing write grant the user made and turning it dead on
// upgrade would revoke it without telling them.
func TestLoadPinsTrustedFoldersThatHaveNone(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "project")
	if err := os.MkdirAll(project, 0755); err != nil {
		t.Fatal(err)
	}
	writeConfigJSON(t, base, fmt.Sprintf(`{"workspaceFolders":[{"path":%q}]}`, project))

	cfg, res, err := LoadFrom(base, func(d string) string { return "id:" + d })
	if err != nil {
		t.Fatalf("load error: %v", err)
	}
	if !res.PinnedIdentities {
		t.Error("Load should report that it pinned an entry that had no fingerprint")
	}
	list := cfg.TrustedFolderList()
	if len(list) != 1 {
		t.Fatalf("expected the entry to survive, got %v", list)
	}
	if list[0].Identity != "id:"+project {
		t.Errorf("entry pinned to %q, want the directory that is there now", list[0].Identity)
	}
}

// A dead entry has to stay dead. Pinning it to whatever now sits at its path is
// the one outcome the pin exists to prevent, reached through the upgrade path.
func TestLoadLeavesAMissingFolderUnpinned(t *testing.T) {
	base := t.TempDir()
	gone := filepath.Join(base, "deleted-project")
	writeConfigJSON(t, base, fmt.Sprintf(`{"workspaceFolders":[{"path":%q}]}`, gone))

	cfg, res, err := LoadFrom(base, func(d string) string { return "id:" + d })
	if err != nil {
		t.Fatalf("load error: %v", err)
	}
	if res.PinnedIdentities {
		t.Error("a folder that is not on disk must not be pinned")
	}
	list := cfg.TrustedFolderList()
	if len(list) != 1 || list[0].Identity != "" {
		t.Errorf("expected one entry with no pin, got %v", list)
	}
}

// An entry that already has a pin is never re-pinned. Re-deriving it on every
// load would hand a replaced folder's grant to the newcomer at the next launch.
func TestLoadDoesNotRepinAnEntryThatHasOne(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "project")
	if err := os.MkdirAll(project, 0755); err != nil {
		t.Fatal(err)
	}
	writeConfigJSON(t, base, fmt.Sprintf(`{"workspaceFolders":[{"path":%q,"identity":"pin-from-when-it-was-trusted"}]}`, project))

	cfg, res, err := LoadFrom(base, func(d string) string { return "id:" + d })
	if err != nil {
		t.Fatalf("load error: %v", err)
	}
	if res.PinnedIdentities {
		t.Error("nothing needed pinning, so Load should not report that it pinned")
	}
	if list := cfg.TrustedFolderList(); len(list) != 1 || list[0].Identity != "pin-from-when-it-was-trusted" {
		t.Errorf("the stored pin was replaced: %v", list)
	}
}

func writeConfigJSON(t *testing.T, baseDir, body string) {
	t.Helper()
	dir := DirFrom(baseDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}
