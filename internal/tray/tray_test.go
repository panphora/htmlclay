package tray

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"fyne.io/systray"
	"github.com/panphora/htmlclay/internal/config"
	"github.com/panphora/htmlclay/internal/testutil"
)

func TestIconEmbedded(t *testing.T) {
	for _, c := range []struct {
		name string
		data []byte
	}{
		{"icon.png", iconBytes},
		{"icon-template.png", iconTemplateBytes},
	} {
		if len(c.data) == 0 {
			t.Fatalf("%s not embedded", c.name)
		}
		if c.data[0] != 0x89 || c.data[1] != 'P' || c.data[2] != 'N' || c.data[3] != 'G' {
			t.Fatalf("embedded %s is not a valid PNG", c.name)
		}
	}
}

// Every trusted folder gets a clickable row, however many there are.
//
// The pool used to be fixed at slotCount, and a row click is untrustFolder's only
// caller, so entries past the sixteenth stayed trusted with no way to revoke them
// from the menu. Asserting "every row has a label" would have passed the whole
// time; the click is the part that was missing, so drive one.
func TestEveryRowIsClickableBeyondTheInitialPool(t *testing.T) {
	lm, _ := newListMenu("Trusted Folders", "", "No folders yet", "", "")

	const count = slotCount + 20
	rows := make([]Row, count)
	for i := range rows {
		rows[i] = Row{Path: fmt.Sprintf("/home/me/project-%02d", i), Label: fmt.Sprintf("project-%02d", i)}
	}

	clicked := make(chan string, 1)
	lm.watch(func(path string) []Row {
		clicked <- path
		return nil
	}, nil)
	lm.render(rows)

	lm.mu.Lock()
	placed := map[string]*slot{}
	for _, s := range lm.slots {
		if s.path != "" {
			placed[s.path] = s
		}
	}
	lm.mu.Unlock()
	if len(placed) != count {
		t.Fatalf("%d of %d rows got a slot", len(placed), count)
	}

	// The last row is the one a fixed pool could never place.
	last := rows[count-1]
	placed[last.Path].item.ClickedCh <- struct{}{}
	got := testutil.Receive(t, 10*time.Second, "a row click to reach the remove hook", clicked)
	if got != last.Path {
		t.Errorf("clicking the last row reported %q, want %q", got, last.Path)
	}
}

// Growing the pool must not move an entry that is already placed. A click can sit
// queued against a row while a render runs, and a slot whose meaning shifted would
// untrust a different folder than the one the user clicked.
func TestGrowingThePoolLeavesPlacedEntriesWhereTheyAre(t *testing.T) {
	lm, _ := newListMenu("Trusted Folders", "", "No folders yet", "", "")

	first := make([]Row, slotCount)
	for i := range first {
		first[i] = Row{Path: fmt.Sprintf("/home/me/first-%02d", i), Label: "first"}
	}
	lm.render(first)

	lm.mu.Lock()
	before := make([]string, len(lm.slots))
	for i, s := range lm.slots {
		before[i] = s.path
	}
	lm.mu.Unlock()

	lm.render(append(append([]Row(nil), first...), Row{Path: "/home/me/one-more", Label: "one more"}))

	lm.mu.Lock()
	defer lm.mu.Unlock()
	if len(lm.slots) <= len(before) {
		t.Fatalf("the pool should have grown past %d, got %d", len(before), len(lm.slots))
	}
	for i, path := range before {
		if lm.slots[i].path != path {
			t.Errorf("slot %d moved from %q to %q; a queued click would land on the wrong folder", i, path, lm.slots[i].path)
		}
	}
}

func TestProgramsMenuPassesTheRegistrationIDToItsRowHook(t *testing.T) {
	const id = "7ccd34a6061ca7bb37404e60e740c448"
	clicked := make(chan string, 1)
	tr := &Tray{helpers: &HelperHooks{
		List: func() []Row {
			return []Row{{Path: id, Label: "search  (missing)"}}
		},
		Row: func(got string) []Row {
			clicked <- got
			return nil
		},
	}}
	tr.buildHelpersMenu()
	tr.watchHelpersMenu(nil)

	tr.helpersMenu.mu.Lock()
	var item *systray.MenuItem
	for _, s := range tr.helpersMenu.slots {
		if s.path == id {
			item = s.item
			break
		}
	}
	tr.helpersMenu.mu.Unlock()
	if item == nil {
		t.Fatal("program did not receive a menu row")
	}
	item.ClickedCh <- struct{}{}
	if got := testutil.Receive(t, 2*time.Second, "the program row hook", clicked); got != id {
		t.Fatalf("row hook received %q, want registration ID %q", got, id)
	}
}

// The notice row is how a Linux desktop with no permission dialogs says so, and
// it is also the row that must not appear anywhere else. An empty notice adding
// a blank disabled row would put one at the top of every menu on every platform.
func TestNoticeRowAppearsOnlyWhenThereIsSomethingToSay(t *testing.T) {
	if item := (&Tray{}).addNotice(); item != nil {
		t.Errorf("an empty notice added a row: %s", item)
	}

	const notice = "HTML Clay cannot show permission dialogs on this desktop. Install zenity or kdialog."
	item := (&Tray{notice: notice}).addNotice()
	if item == nil {
		t.Fatal("a notice must get a row of its own")
	}
	if !strings.Contains(item.String(), notice) {
		t.Errorf("the row does not carry the notice: %s", item)
	}
	if !item.Disabled() {
		t.Error("the notice row must be disabled; there is nothing behind it to click")
	}
}

// The Configure an Open File submenu lists one row per open document that
// declares a program, and a click hands the row's own key to the app, which is
// what lets a bookmark-opened document be configured from the tray.
func TestHelpersMenuConfigureAnOpenFilePassesTheRowKey(t *testing.T) {
	const key = "/home/me/page.htmlclay\x00search"
	clicked := make(chan string, 1)
	tr := &Tray{helpers: &HelperHooks{
		List: func() []Row { return nil },
		Documents: func() []Row {
			return []Row{{Path: key, Label: "~/page.htmlclay: search (approval needed: /usr/local/bin/search)"}}
		},
		Document: func(got string) []Row {
			clicked <- got
			return nil
		},
	}}
	tr.buildHelpersMenu()
	tr.watchHelpersMenu(nil)
	if tr.helperDocumentsMenu == nil {
		t.Fatal("the Programs submenu did not offer configuring an open file")
	}

	tr.helperDocumentsMenu.mu.Lock()
	var item *systray.MenuItem
	for _, s := range tr.helperDocumentsMenu.slots {
		if s.path == key {
			item = s.item
			break
		}
	}
	tr.helperDocumentsMenu.mu.Unlock()
	if item == nil {
		t.Fatal("the open document did not receive a row")
	}
	item.ClickedCh <- struct{}{}
	if got := testutil.Receive(t, 2*time.Second, "the open file row hook", clicked); got != key {
		t.Fatalf("row hook received %q, want the document key %q", got, key)
	}
}

// profileReply is one scripted answer to the Display Name dialog.
type profileReply struct {
	value string
	ok    bool
}

type profileAlert struct {
	title   string
	message string
}

// profileHarness drives the Profile submenu's operations with scripted answers
// instead of native dialogs, and records what it was asked.
type profileHarness struct {
	actions *profileActions
	prompts []string
	alerts  []profileAlert
}

func newProfileHarness(t *testing.T, cfg *config.Config, replies ...profileReply) *profileHarness {
	t.Helper()
	h := &profileHarness{}
	next := 0
	h.actions = &profileActions{
		cfg: cfg,
		prompt: func(title, message, initial string) (string, bool, error) {
			h.prompts = append(h.prompts, message)
			if next >= len(replies) {
				t.Errorf("the Profile submenu asked for a name with no scripted answer left")
				return "", false, nil
			}
			r := replies[next]
			next++
			return r.value, r.ok, nil
		},
		alert: func(title, message string) {
			h.alerts = append(h.alerts, profileAlert{title: title, message: message})
		},
	}
	return h
}

// profileTestConfig gives a test its own config file on disk, since a profile
// update is only real once it has been written.
func profileTestConfig(t *testing.T) (*config.Config, string) {
	t.Helper()
	baseDir := t.TempDir()
	cfg, _, err := config.LoadFrom(baseDir, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	return cfg, baseDir
}

// profileTestConfigFrom loads a config file written by hand, which is the only way to
// start a test from a profile the app itself would never have written.
func profileTestConfigFrom(t *testing.T, body string) (*config.Config, string) {
	t.Helper()
	baseDir := t.TempDir()
	dir := config.DirFrom(baseDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.LoadFrom(baseDir, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	return cfg, baseDir
}

// profileOnDisk reloads the config the submenu wrote, which is the only proof a
// toggle reached the file rather than just memory.
func profileOnDisk(t *testing.T, baseDir string) config.Profile {
	t.Helper()
	cfg, _, err := config.LoadFrom(baseDir, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	return cfg.ProfileState()
}

func TestProfileFirstEnableAsksSavesAndMintsOnce(t *testing.T) {
	cfg, baseDir := profileTestConfig(t)
	h := newProfileHarness(t, cfg, profileReply{value: "Ada", ok: true})

	if !h.actions.toggle() {
		t.Fatal("turning the profile on reported it off")
	}
	got := cfg.ProfileState()
	if !got.Enabled || got.Name != "Ada" || len(got.ID) != 22 {
		t.Errorf("state after enabling is %+v, want an enabled Ada with a 22-character id", got)
	}
	if !got.Shareable() {
		t.Errorf("the saved profile is not shareable: %+v", got)
	}
	if len(h.prompts) != 1 {
		t.Errorf("asked for a name %d times, want 1", len(h.prompts))
	}
	if onDisk := profileOnDisk(t, baseDir); onDisk != got {
		t.Errorf("reloaded from disk as %+v, want %+v", onDisk, got)
	}
}

// Cancelling the first name prompt must leave no trace: not enabled, and no
// profile record on disk that a later launch would read as consent.
func TestProfileCancelWritesNothing(t *testing.T) {
	cfg, baseDir := profileTestConfig(t)
	h := newProfileHarness(t, cfg, profileReply{value: "", ok: false})

	if h.actions.toggle() {
		t.Error("a cancelled toggle reported the profile on")
	}
	if got := cfg.ProfileState(); got != (config.Profile{}) {
		t.Errorf("state after cancelling is %+v, want the zero value", got)
	}
	data, err := os.ReadFile(filepath.Join(config.DirFrom(baseDir), "config.json"))
	if err == nil {
		if strings.Contains(string(data), `"profile"`) {
			t.Errorf("a cancelled toggle wrote a profile to disk: %s", data)
		}
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestProfileInvalidNameIsAskedAgain(t *testing.T) {
	cfg, _ := profileTestConfig(t)
	h := newProfileHarness(t, cfg, profileReply{value: "a@b.co", ok: true}, profileReply{value: "Ada", ok: true})

	if !h.actions.toggle() {
		t.Fatal("turning the profile on reported it off")
	}
	if len(h.prompts) != 2 {
		t.Fatalf("asked for a name %d times, want 2", len(h.prompts))
	}
	if !strings.HasPrefix(h.prompts[1], "Use a name, not an email address.") {
		t.Errorf("the second prompt said %q, want it to lead with the rejection", h.prompts[1])
	}
	if got := cfg.ProfileState(); !got.Enabled || got.Name != "Ada" {
		t.Errorf("state after the corrected name is %+v, want an enabled Ada", got)
	}
}

func TestProfileToggleOffAndOnKeepsIDWithoutAsking(t *testing.T) {
	cfg, _ := profileTestConfig(t)
	h := newProfileHarness(t, cfg, profileReply{value: "Ada", ok: true})

	if !h.actions.toggle() {
		t.Fatal("turning the profile on reported it off")
	}
	id := cfg.ProfileState().ID

	if h.actions.toggle() {
		t.Error("toggling off reported the profile on")
	}
	if got := cfg.ProfileState(); got.Enabled || got.ID != id {
		t.Errorf("state after toggling off is %+v, want off with id %q", got, id)
	}

	if !h.actions.toggle() {
		t.Error("toggling back on reported the profile off")
	}
	got := cfg.ProfileState()
	if !got.Enabled || got.ID != id || got.Name != "Ada" {
		t.Errorf("state after toggling back on is %+v, want enabled Ada with id %q", got, id)
	}
	if len(h.prompts) != 1 {
		t.Errorf("toggling asked for a name %d times, want only the first", len(h.prompts))
	}
}

// A rename is a name change and nothing else: the id and the on/off state it was
// sharing under both survive it.
func TestProfileRenameKeepsIDAndEnabledState(t *testing.T) {
	cfg, _ := profileTestConfig(t)
	h := newProfileHarness(t, cfg, profileReply{value: "Ada", ok: true}, profileReply{value: "Grace", ok: true})
	if !h.actions.toggle() {
		t.Fatal("turning the profile on reported it off")
	}
	id := cfg.ProfileState().ID
	h.actions.rename()
	if got := cfg.ProfileState(); !got.Enabled || got.ID != id || got.Name != "Grace" {
		t.Errorf("renaming an enabled profile gave %+v, want enabled Grace with id %q", got, id)
	}

	offCfg, _ := profileTestConfig(t)
	offH := newProfileHarness(t, offCfg, profileReply{value: "Ada", ok: true}, profileReply{value: "Grace", ok: true})
	offH.actions.toggle()
	offH.actions.toggle()
	offID := offCfg.ProfileState().ID
	offH.actions.rename()
	if got := offCfg.ProfileState(); got.Enabled || got.ID != offID || got.Name != "Grace" {
		t.Errorf("renaming a disabled profile gave %+v, want off Grace with id %q", got, offID)
	}

	freshCfg, _ := profileTestConfig(t)
	freshH := newProfileHarness(t, freshCfg, profileReply{value: "Ada", ok: true})
	freshH.actions.rename()
	got := freshCfg.ProfileState()
	if got.Enabled || got.Name != "Ada" || len(got.ID) != 22 {
		t.Errorf("renaming with no profile yet gave %+v, want off Ada with a minted 22-character id", got)
	}
}

// A profile that is on but damaged reads as off in the menu and is not shared, so the
// click that looks like "turn it on" is really "repair it": the name is asked for again
// and the unusable id is replaced.
func TestProfileDamagedRecordIsRepairedByToggling(t *testing.T) {
	cfg, baseDir := profileTestConfigFrom(t, `{"profile":{"enabled":true,"id":"abc","name":"ada@example.com"}}`)
	if cfg.ProfileState().Shareable() {
		t.Fatal("the damaged record read as shareable")
	}
	h := newProfileHarness(t, cfg, profileReply{value: "Ada", ok: true})

	if !h.actions.toggle() {
		t.Fatal("toggling a damaged profile on reported it off")
	}
	got := cfg.ProfileState()
	if !got.Enabled || got.Name != "Ada" || len(got.ID) != 22 || !config.ValidProfileID(got.ID) {
		t.Errorf("state after repairing is %+v, want an enabled Ada with a fresh 22-character id", got)
	}
	if !got.Shareable() {
		t.Errorf("the repaired profile is not shareable: %+v", got)
	}
	if len(h.prompts) != 1 {
		t.Errorf("asked for a name %d times, want 1", len(h.prompts))
	}
	if onDisk := profileOnDisk(t, baseDir); onDisk != got {
		t.Errorf("reloaded from disk as %+v, want %+v", onDisk, got)
	}
}

// A rename is the only way out of a damaged id for a profile that is already off, so it
// mints a replacement exactly as it does for a record that never had one.
func TestProfileRenameReplacesADamagedID(t *testing.T) {
	cfg, baseDir := profileTestConfigFrom(t, `{"profile":{"enabled":false,"id":"abc","name":"Grace"}}`)
	h := newProfileHarness(t, cfg, profileReply{value: "Ada", ok: true})

	h.actions.rename()
	got := cfg.ProfileState()
	if got.Enabled || got.Name != "Ada" || len(got.ID) != 22 || !config.ValidProfileID(got.ID) {
		t.Errorf("renaming a damaged record gave %+v, want off Ada with a fresh 22-character id", got)
	}
	if onDisk := profileOnDisk(t, baseDir); onDisk != got {
		t.Errorf("reloaded from disk as %+v, want %+v", onDisk, got)
	}
}

// A rename or toggle the user cannot save must say so rather than leave the menu
// claiming a name the app will not use.
func TestProfileSaveFailureAlerts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod does not deny writes on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root can write to a read-only directory")
	}

	cfg, baseDir := profileTestConfig(t)
	dir := config.DirFrom(baseDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	h := newProfileHarness(t, cfg, profileReply{value: "Ada", ok: true})
	if h.actions.toggle() {
		t.Error("a toggle that could not save reported the profile on")
	}
	if len(h.alerts) != 1 || h.alerts[0].title != "Could not save your profile" {
		t.Errorf("alerts were %+v, want exactly one titled %q", h.alerts, "Could not save your profile")
	}
	if got := cfg.ProfileState(); got != (config.Profile{}) {
		t.Errorf("state after a failed save is %+v, want the zero value", got)
	}
}
