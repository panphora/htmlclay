package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
//
// The counts and the reload at the end are what stop this passing while proving
// nothing. Without them a build whose mutators all returned early, or whose Save
// wrote nothing, would look exactly as green as a correct one.
func TestConcurrentMutatorsAndSaveAreRaceFree(t *testing.T) {
	baseDir := t.TempDir()
	cfg, _, err := LoadFrom(baseDir, noIdentity)
	if err != nil {
		t.Fatal(err)
	}

	// A folder only the identity goroutine ever touches, so its final identity is
	// deterministic. Every other trusted folder is added and removed concurrently,
	// and a re-add installs an identity of its own, which would mask a
	// SetTrustedIdentity that did nothing at all.
	const pinned = "/trusted/pinned"
	if !cfg.AddTrustedFolder(pinned, "initial") {
		t.Fatal("the pinned folder was already present")
	}

	const iters = 300
	var mu sync.Mutex
	var saves, saveErrs int
	noteSave := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			saveErrs++
			return
		}
		saves++
	}

	// A start barrier, so the four goroutines contend from the same instant
	// instead of trickling out behind however long the spawn loop took.
	start := make(chan struct{})
	var wg sync.WaitGroup
	run := func(f func(i int)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < iters; i++ {
				f(i)
			}
		}()
	}

	run(func(i int) {
		cfg.RememberSitePort(fmt.Sprintf("/root/%d", i%8), i)
		noteSave(cfg.Save())
	})
	run(func(i int) {
		d := fmt.Sprintf("/trusted/%d", i%8)
		if !cfg.AddTrustedFolder(d, fmt.Sprintf("id:%d", i)) {
			cfg.RemoveTrustedFolder(d)
		}
		noteSave(cfg.Save())
	})
	run(func(i int) {
		// i%2 == 1, so the last iteration leaves it TRUE. With the sense flipped the
		// final value is false, which is also the default, and a setter that did
		// nothing would be indistinguishable from one that worked.
		cfg.SetStartOnLogin(i%2 == 1)
		cfg.SetTrustedIdentity(pinned, fmt.Sprintf("re:%d", i))
		noteSave(cfg.Save())
	})
	reads := 0
	run(func(i int) {
		_ = cfg.StartOnLoginEnabled()
		_ = cfg.SitePort("/root/1")
		_ = cfg.SitePortList()
		_ = cfg.TrustedFolderList()
		mu.Lock()
		reads++
		mu.Unlock()
	})

	close(start)
	wg.Wait()

	if saveErrs != 0 {
		t.Errorf("%d of %d saves failed", saveErrs, saves+saveErrs)
	}
	if want := 3 * iters; saves != want {
		t.Errorf("%d saves completed, want %d", saves, want)
	}
	if reads != iters {
		t.Errorf("%d read passes completed, want %d", reads, iters)
	}

	// Every one of those saves marshalled a map another goroutine was writing. If
	// any of them had torn, the file on disk would not parse back, and the eight
	// ports and eight trusted folders the mutators settled on would not survive.
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded, _, err := LoadFrom(baseDir, noIdentity)
	if err != nil {
		t.Fatalf("the config written under concurrent mutation does not load back: %v", err)
	}
	// Compared against the concrete expected state, not against the in-memory
	// config. Comparing the two would let a mutator that did nothing pass, because
	// both sides would then be equally empty.
	ports := reloaded.SitePortList()
	for k := 0; k < 8; k++ {
		dir := fmt.Sprintf("/root/%d", k)
		// The port goroutine runs its loop sequentially, so the surviving value is
		// the last i below iters with i%8 == k.
		want := iters - 1
		for want%8 != k {
			want--
		}
		got, ok := ports[dir]
		if !ok {
			t.Errorf("%s is missing from the reloaded ports", dir)
			continue
		}
		if got != want {
			t.Errorf("%s reloaded as port %d, want %d", dir, got, want)
		}
	}
	if len(ports) != 8 {
		t.Errorf("reloaded %d site ports, want exactly 8", len(ports))
	}

	// The transient folders pin RemoveTrustedFolder, which nothing else here
	// touches. Their goroutine runs sequentially, so the toggling is deterministic:
	// /trusted/0..3 are visited 38 times (even, so they end absent) and /trusted/4..7
	// 37 times (odd, so they end present carrying the identity their last add set).
	live := map[string]string{}
	for _, tf := range reloaded.TrustedFolderList() {
		live[tf.Path] = tf.Identity
	}
	for k := 0; k < 8; k++ {
		dir := fmt.Sprintf("/trusted/%d", k)
		identity, present := live[dir]
		if k < 4 {
			if present {
				t.Errorf("%s survived, but its adds and removes cancel out", dir)
			}
			continue
		}
		if !present {
			t.Errorf("%s is missing, but its last visit was an add", dir)
			continue
		}
		lastAdd := iters - 1
		for lastAdd%8 != k {
			lastAdd--
		}
		if want := fmt.Sprintf("id:%d", lastAdd); identity != want {
			t.Errorf("%s carries identity %q, want %q", dir, identity, want)
		}
	}

	var pinnedIdentity string
	var found bool
	for _, tf := range reloaded.TrustedFolderList() {
		if tf.Path == pinned {
			pinnedIdentity, found = tf.Identity, true
		}
	}
	if !found {
		t.Fatalf("%s did not survive the round trip", pinned)
	}
	if want := fmt.Sprintf("re:%d", iters-1); pinnedIdentity != want {
		t.Errorf("%s reloaded with identity %q, want %q", pinned, pinnedIdentity, want)
	}
	if !reloaded.StartOnLoginEnabled() {
		t.Error("start-on-login reloaded false, but the last write set it true")
	}
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

func TestAIEditDefaultsOn(t *testing.T) {
	baseDir := t.TempDir()
	cfg, _, err := LoadFrom(baseDir, noIdentity)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.AIEditEnabled() {
		t.Error("a fresh config should read as AI editing on")
	}

	baseDir = t.TempDir()
	writeConfigJSON(t, baseDir, `{"startOnLogin":true}`)
	cfg, _, err = LoadFrom(baseDir, noIdentity)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.AIEditEnabled() {
		t.Error("a config with no aiEdit key should read as AI editing on")
	}
}

func TestAIEditOffSurvivesSaveAndLoad(t *testing.T) {
	baseDir := t.TempDir()
	cfg, _, _ := LoadFrom(baseDir, noIdentity)
	cfg.SetAIEditEnabled(false)
	if err := cfg.Save(); err != nil {
		t.Fatalf("save error: %v", err)
	}

	loaded, _, err := LoadFrom(baseDir, noIdentity)
	if err != nil {
		t.Fatalf("load error: %v", err)
	}
	if loaded.AIEditEnabled() {
		t.Error("an explicit off should survive save and load")
	}

	loaded.SetAIEditEnabled(true)
	if err := loaded.Save(); err != nil {
		t.Fatalf("save error: %v", err)
	}
	again, _, err := LoadFrom(baseDir, noIdentity)
	if err != nil {
		t.Fatalf("load error: %v", err)
	}
	if !again.AIEditEnabled() {
		t.Error("an explicit on should survive save and load")
	}
}

func TestAIEditEnginesLoadAndCopy(t *testing.T) {
	baseDir := t.TempDir()
	writeConfigJSON(t, baseDir, `{"aiEdit":{"default":"codex","engines":{"echo":["sh","-c","cat"]}}}`)
	cfg, _, err := LoadFrom(baseDir, noIdentity)
	if err != nil {
		t.Fatalf("load error: %v", err)
	}

	def, engines := cfg.AIEditEngines()
	if def != "codex" {
		t.Errorf("expected the default engine codex, got %q", def)
	}
	if len(engines) != 1 {
		t.Fatalf("expected one user engine, got %v", engines)
	}
	echo := engines["echo"]
	if len(echo) != 3 || echo[0] != "sh" || echo[1] != "-c" || echo[2] != "cat" {
		t.Errorf("expected echo argv [sh -c cat], got %v", echo)
	}
	if !cfg.AIEditEnabled() {
		t.Error("a config with no enabled key should read as AI editing on")
	}

	echo[0] = "tampered"
	engines["added"] = []string{"x"}

	def, again := cfg.AIEditEngines()
	if def != "codex" {
		t.Errorf("the default engine changed: %q", def)
	}
	if len(again) != 1 || again["echo"][0] != "sh" {
		t.Errorf("the returned map or slice aliases the config's copy: %v", again)
	}
}

func TestAIEditOffKeepsEngines(t *testing.T) {
	baseDir := t.TempDir()
	writeConfigJSON(t, baseDir, `{"aiEdit":{"default":"codex","engines":{"echo":["sh","-c","cat"]}}}`)
	cfg, _, err := LoadFrom(baseDir, noIdentity)
	if err != nil {
		t.Fatalf("load error: %v", err)
	}

	cfg.SetAIEditEnabled(false)

	def, engines := cfg.AIEditEngines()
	if def != "codex" {
		t.Errorf("turning AI editing off dropped the default engine: %q", def)
	}
	if len(engines) != 1 || engines["echo"][0] != "sh" {
		t.Errorf("turning AI editing off dropped the user engines: %v", engines)
	}
}

// readProfileOnDisk returns the profile stored in config.json, so a test can tell
// a rollback that only touched memory from one that reached disk.
func readProfileOnDisk(t *testing.T, baseDir string) Profile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(DirFrom(baseDir), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var onDisk struct {
		Profile *Profile `json:"profile"`
	}
	if err := json.Unmarshal(data, &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk.Profile == nil {
		return Profile{}
	}
	return *onDisk.Profile
}

// Every existing install, and every fresh one, starts with no profile: a config
// file that never carried the key must read as off.
func TestProfileDefaultsOff(t *testing.T) {
	baseDir := t.TempDir()
	writeConfigJSON(t, baseDir, `{"startOnLogin":true}`)
	cfg, _, err := LoadFrom(baseDir, noIdentity)
	if err != nil {
		t.Fatalf("load error: %v", err)
	}
	if got := cfg.ProfileState(); got != (Profile{}) {
		t.Errorf("a config with no profile read as %+v, want the zero value", got)
	}
	if cfg.ProfileState().Shareable() {
		t.Error("a profile that was never set up must not be shareable")
	}
}

func TestProfileRoundTrip(t *testing.T) {
	baseDir := t.TempDir()
	cfg, _, err := LoadFrom(baseDir, noIdentity)
	if err != nil {
		t.Fatal(err)
	}
	cfg.SetStartOnLogin(true)
	cfg.RememberSitePort("/root/sites", 12345)

	id, err := NewProfileID()
	if err != nil {
		t.Fatalf("mint error: %v", err)
	}
	if err := cfg.UpdateProfile(Profile{Enabled: true, ID: id, Name: "  Ada   Chen "}); err != nil {
		t.Fatalf("update error: %v", err)
	}

	loaded, _, err := LoadFrom(baseDir, noIdentity)
	if err != nil {
		t.Fatal(err)
	}
	want := Profile{Enabled: true, ID: id, Name: "Ada Chen"}
	if got := loaded.ProfileState(); got != want {
		t.Errorf("profile reloaded as %+v, want %+v", got, want)
	}
	if !loaded.ProfileState().Shareable() {
		t.Error("an enabled profile with a valid id and name must be shareable")
	}
	if !loaded.StartOnLoginEnabled() {
		t.Error("saving the profile dropped start-on-login")
	}
	if got := loaded.SitePort("/root/sites"); got != 12345 {
		t.Errorf("saving the profile dropped the remembered port, got %d", got)
	}
}

func TestProfileIDShape(t *testing.T) {
	first, err := NewProfileID()
	if err != nil {
		t.Fatalf("mint error: %v", err)
	}
	if len(first) != 22 {
		t.Errorf("id %q is %d characters, want 22", first, len(first))
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`).MatchString(first) {
		t.Errorf("id %q is not 22 unpadded base64url characters", first)
	}
	second, err := NewProfileID()
	if err != nil {
		t.Fatalf("second mint error: %v", err)
	}
	if second == first {
		t.Errorf("two mints returned the same id %q", first)
	}
}

// The name rules are ClayJS's, so a name this app accepts is never silently
// replaced by the browser's own prompt. The length limit counts UTF-16 code
// units, not runes or bytes, because that is what the client measures.
func TestProfileNameRules(t *testing.T) {
	accepted := []struct {
		in   string
		want string
	}{
		{"Ada", "Ada"},
		{"Zoë O'Brien", "Zoë O'Brien"},
		{strings.Repeat("a", 120), strings.Repeat("a", 120)},
	}
	for _, c := range accepted {
		got, err := CleanProfileName(c.in)
		if err != nil {
			t.Errorf("CleanProfileName(%q) errored: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("CleanProfileName(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	rejected := []struct {
		in   string
		want error
	}{
		{"", ErrProfileNameEmpty},
		{"   ", ErrProfileNameEmpty},
		{"ada@example.com", ErrProfileNameEmail},
		{strings.Repeat("a", 121), ErrProfileNameLong},
		{strings.Repeat("😀", 61), ErrProfileNameLong},
	}
	for _, c := range rejected {
		if got, err := CleanProfileName(c.in); err != c.want {
			t.Errorf("CleanProfileName(%q) = %q, %v; want error %v", c.in, got, err, c.want)
		}
	}
}

// Go's whitespace set and JavaScript's differ at both ends: Go splits on U+0085 (NEL),
// which ClayJS keeps, and keeps U+FEFF, which ClayJS trims away. A name only one of the
// two accepts is a name the document silently replaces with the browser's own prompt, so
// the app must use ClayJS's set exactly.
func TestProfileNameWhitespaceMatchesClayJS(t *testing.T) {
	rejected := []string{"\ufeff", " \u00a0\u3000 ", "\u2000\u200a"}
	for _, in := range rejected {
		if got, err := CleanProfileName(in); err != ErrProfileNameEmpty {
			t.Errorf("CleanProfileName(%q) = %q, %v; want %v", in, got, err, ErrProfileNameEmpty)
		}
	}

	accepted := []struct {
		in   string
		want string
	}{
		{"Ada\ufeffChen", "Ada Chen"},
		{"Ada\u200bChen", "Ada\u200bChen"},
		{"Ada\u0085Chen", "Ada\u0085Chen"},
	}
	for _, c := range accepted {
		got, err := CleanProfileName(c.in)
		if err != nil {
			t.Errorf("CleanProfileName(%q) errored: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("CleanProfileName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A record already on disk that fails the rules must still be switchable off. Sharing
// used to be stoppable only by writing a record the setter re-validated, so a damaged id
// or name left the person unable to turn it off at all.
func TestProfileDamagedRecordCanBeTurnedOff(t *testing.T) {
	baseDir := t.TempDir()
	writeConfigJSON(t, baseDir, `{"profile":{"enabled":true,"id":"abc","name":"ada@example.com"}}`)
	cfg, _, err := LoadFrom(baseDir, noIdentity)
	if err != nil {
		t.Fatal(err)
	}
	damaged := Profile{Enabled: true, ID: "abc", Name: "ada@example.com"}
	if got := cfg.ProfileState(); got != damaged {
		t.Fatalf("loaded profile is %+v, want the damaged record %+v", got, damaged)
	}
	if _, _, ok := cfg.SharedPerson(); ok {
		t.Error("a damaged record was shared")
	}

	off := Profile{Enabled: false, ID: "abc", Name: "ada@example.com"}
	if err := cfg.UpdateProfile(off); err != nil {
		t.Fatalf("turning a damaged profile off failed: %v", err)
	}
	if got := cfg.ProfileState(); got != off {
		t.Errorf("state after turning off is %+v, want %+v", got, off)
	}
	if _, _, ok := cfg.SharedPerson(); ok {
		t.Error("a profile that was turned off was shared")
	}
	if got := readProfileOnDisk(t, baseDir); got != off {
		t.Errorf("on disk after turning off is %+v, want %+v", got, off)
	}
}

// Turning sharing back on is where a damaged record has to be caught: it is the one
// direction that makes the person's identity visible to documents.
func TestProfileTurningOnStillValidates(t *testing.T) {
	for _, c := range []struct {
		name string
		body string
		want error
	}{
		{"a damaged name", `{"profile":{"enabled":false,"id":"abc","name":"ada@example.com"}}`, ErrProfileNameEmail},
		{"a damaged id", `{"profile":{"enabled":false,"id":"abc","name":"Ada"}}`, ErrProfileIDMalformed},
	} {
		t.Run(c.name, func(t *testing.T) {
			baseDir := t.TempDir()
			writeConfigJSON(t, baseDir, c.body)
			cfg, _, err := LoadFrom(baseDir, noIdentity)
			if err != nil {
				t.Fatal(err)
			}
			before := cfg.ProfileState()
			on := before
			on.Enabled = true
			if err := cfg.UpdateProfile(on); err != c.want {
				t.Errorf("enabling a record with %s returned %v, want %v", c.name, err, c.want)
			}
			if got := cfg.ProfileState(); got != before {
				t.Errorf("a rejected update changed the state to %+v, want %+v", got, before)
			}
		})
	}
}

// The id is the person, so renaming and turning sharing off and on again must
// never mint a new one: a new id would orphan everything already attributed.
func TestProfileRenameAndToggleKeepID(t *testing.T) {
	baseDir := t.TempDir()
	cfg, _, err := LoadFrom(baseDir, noIdentity)
	if err != nil {
		t.Fatal(err)
	}
	id, err := NewProfileID()
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.UpdateProfile(Profile{Enabled: true, ID: id, Name: "Ada"}); err != nil {
		t.Fatal(err)
	}

	for _, next := range []Profile{
		{Enabled: true, ID: id, Name: "Grace"},
		{Enabled: false, ID: id, Name: "Grace"},
		{Enabled: true, ID: id, Name: "Grace"},
	} {
		if err := cfg.UpdateProfile(next); err != nil {
			t.Fatalf("update error: %v", err)
		}
		if got := cfg.ProfileState(); got != next {
			t.Errorf("state after update is %+v, want %+v", got, next)
		}
	}
	if got := cfg.ProfileState().ID; got != id {
		t.Errorf("the id changed to %q, want %q", got, id)
	}
}

func TestProfileIncompleteOrDamaged(t *testing.T) {
	baseDir := t.TempDir()
	cfg, _, err := LoadFrom(baseDir, noIdentity)
	if err != nil {
		t.Fatal(err)
	}
	id, err := NewProfileID()
	if err != nil {
		t.Fatal(err)
	}
	good := Profile{Enabled: true, ID: id, Name: "Ada"}
	if err := cfg.UpdateProfile(good); err != nil {
		t.Fatal(err)
	}

	if err := cfg.UpdateProfile(Profile{Enabled: true, Name: "Ada"}); err != ErrProfileIncomplete {
		t.Errorf("enabling with no id returned %v, want %v", err, ErrProfileIncomplete)
	}
	if got := cfg.ProfileState(); got != good {
		t.Errorf("a rejected incomplete profile changed the state to %+v", got)
	}
	if err := cfg.UpdateProfile(Profile{Enabled: true, ID: "bad id!", Name: "Ada"}); err != ErrProfileIDMalformed {
		t.Errorf("a damaged id returned %v, want %v", err, ErrProfileIDMalformed)
	}
	if got := cfg.ProfileState(); got != good {
		t.Errorf("a rejected damaged id changed the state to %+v", got)
	}
}

// A profile update is a promise about what is on disk. A write that fails must
// leave both the memory and the file at the previous record rather than a state
// the user never agreed to and that no reload will reproduce.
func TestProfileFailedWriteKeepsOldRecord(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod does not deny writes on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root can write to a read-only directory")
	}

	baseDir := t.TempDir()
	cfg, _, err := LoadFrom(baseDir, noIdentity)
	if err != nil {
		t.Fatal(err)
	}
	id, err := NewProfileID()
	if err != nil {
		t.Fatal(err)
	}
	good := Profile{Enabled: true, ID: id, Name: "Ada"}
	if err := cfg.UpdateProfile(good); err != nil {
		t.Fatal(err)
	}

	dir := DirFrom(baseDir)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	if err := cfg.UpdateProfile(Profile{Enabled: true, ID: id, Name: "Grace"}); err == nil {
		t.Fatal("UpdateProfile reported success with an unwritable config directory")
	}
	if got := cfg.ProfileState(); got != good {
		t.Errorf("in memory after a failed write: %+v, want %+v", got, good)
	}
	if got := readProfileOnDisk(t, baseDir); got != good {
		t.Errorf("on disk after a failed write: %+v, want %+v", got, good)
	}
}

// Readers run on the serving path while the tray writes. Under -race this is the
// test that catches a torn profile: a reader must never see a half-updated id or
// name, because a document would then stamp someone who does not exist.
func TestProfileConcurrentReads(t *testing.T) {
	baseDir := t.TempDir()
	cfg, _, err := LoadFrom(baseDir, noIdentity)
	if err != nil {
		t.Fatal(err)
	}
	id, err := NewProfileID()
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.UpdateProfile(Profile{Enabled: true, ID: id, Name: "Ada"}); err != nil {
		t.Fatal(err)
	}

	names := []string{"Ada", "Grace"}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			if err := cfg.UpdateProfile(Profile{Enabled: true, ID: id, Name: names[i%2]}); err != nil {
				t.Errorf("update %d failed: %v", i, err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			got := cfg.ProfileState()
			if !got.Enabled || got.ID != id || (got.Name != names[0] && got.Name != names[1]) {
				t.Errorf("read %d saw an incomplete profile: %+v", i, got)
				return
			}
		}
	}()
	wg.Wait()
}
