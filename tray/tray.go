package tray

import (
	_ "embed"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"fyne.io/systray"
	"github.com/panphora/htmlclay/browser"
	"github.com/panphora/htmlclay/config"
	"github.com/panphora/htmlclay/platform"
)

// grantPollInterval is how often the Temporary Access Granted submenu re-renders
// from the authoritative grant list. Grants are created by the permission dialog
// in the broker, which has no handle to the tray, so the menu is refreshed by
// polling rather than pushed to. Two seconds is well below the time it takes a user
// to open the submenu after allowing a read, and the render is a cheap no-op when
// the list is unchanged.
const grantPollInterval = 2 * time.Second

//go:embed icon.png
var iconBytes []byte

//go:embed icon-template.png
var iconTemplateBytes []byte

type UpdateInfo struct {
	Version string
	URL     string
}

// TrustedFolderHooks are the app operations the Trusted Folders submenu drives.
// Each returns the authoritative trusted list so the tray always re-renders from
// truth rather than tracking state itself. Add pops the native folder picker and
// trusts the choice; Remove untrusts one; List is the initial render.
type TrustedFolderHooks struct {
	List   func() []string
	Add    func() []string
	Remove func(dir string) []string
}

// GrantHooks are the app operations the Temporary Access Granted submenu drives.
// List returns the current runtime grants; Revoke withdraws one and returns the
// updated list. There is no Add: grants are created by the permission dialog, not
// the tray.
type GrantHooks struct {
	List   func() []string
	Revoke func(path string) []string
}

// trustedSlotCount is the number of pre-allocated submenu rows for trusted
// folders. systray cannot delete menu items, so the list is rendered into a fixed
// pool of rows that are shown, retitled, and hidden. Beyond this many trusted
// folders the extras stay active but are not shown (logged on render).
const trustedSlotCount = 16

type trustedSlot struct {
	item *systray.MenuItem
	path string
}

type Tray struct {
	cfg           *config.Config
	onOpenExample func()
	onOpenBackups func()
	onQuit        func()
	updateCh      <-chan UpdateInfo
	updateItem    *systray.MenuItem
	updateURL     string

	trusted         *TrustedFolderHooks
	trustedEmpty    *systray.MenuItem
	trustedOverflow *systray.MenuItem
	trustedSlots    []*trustedSlot
	trustedMu       sync.Mutex

	grants        *GrantHooks
	grantEmpty    *systray.MenuItem
	grantOverflow *systray.MenuItem
	grantSlots    []*trustedSlot
	grantMu       sync.Mutex

	workspaces     *TrustedFolderHooks
	opened         *GrantHooks
	workspacesMenu *listMenu
	openedMenu     *listMenu
}

// Run starts the tray. workspaces drives the Workspace Folders submenu
// (TrustedFolderHooks shape: it has an Add flow) and opened drives the Opened
// for Editing submenu (GrantHooks shape: entries are created elsewhere, the
// tray only lists and revokes). Both are polled, because their page-request
// routes create entries with no handle to the tray.
func Run(cfg *config.Config, onOpenExample func(), onOpenBackups func(), onQuit func(), updateCh <-chan UpdateInfo, trusted *TrustedFolderHooks, grants *GrantHooks, workspaces *TrustedFolderHooks, opened *GrantHooks) {
	t := &Tray{
		cfg:           cfg,
		onOpenExample: onOpenExample,
		onOpenBackups: onOpenBackups,
		onQuit:        onQuit,
		updateCh:      updateCh,
		trusted:       trusted,
		grants:        grants,
		workspaces:    workspaces,
		opened:        opened,
	}
	systray.Run(t.onReady, t.onExit)
}

// listMenu generalizes the trusted/grant submenu pattern for the newer menus:
// an authoritative-list render into a fixed slot pool (systray cannot delete
// rows), an optional add row, per-row click actions, and polling for lists
// that change outside the tray. The slot-stability contract matches
// renderTrusted: a queued click stays bound to the row it aimed at.
type listMenu struct {
	mu         sync.Mutex
	empty      *systray.MenuItem
	overflow   *systray.MenuItem
	slots      []*trustedSlot
	rowTooltip string
}

func newListMenu(title, tooltip, emptyText, addTitle, addTooltip string) (*listMenu, *systray.MenuItem) {
	menu := systray.AddMenuItem(title, tooltip)
	var addItem *systray.MenuItem
	if addTitle != "" {
		addItem = menu.AddSubMenuItem(addTitle, addTooltip)
	}
	lm := &listMenu{}
	lm.empty = menu.AddSubMenuItem(emptyText, "")
	lm.empty.Disable()
	lm.slots = make([]*trustedSlot, trustedSlotCount)
	for i := range lm.slots {
		item := menu.AddSubMenuItem("", "")
		item.Hide()
		lm.slots[i] = &trustedSlot{item: item}
	}
	lm.overflow = menu.AddSubMenuItem("", "")
	lm.overflow.Disable()
	lm.overflow.Hide()
	return lm, addItem
}

func (lm *listMenu) render(list []string) {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	want := make(map[string]bool, len(list))
	for _, p := range list {
		want[p] = true
	}
	assigned := make(map[string]bool, len(list))
	for _, slot := range lm.slots {
		switch {
		case slot.path == "":
		case want[slot.path]:
			assigned[slot.path] = true
		default:
			slot.path = ""
			slot.item.Hide()
		}
	}
	overflow := 0
	for _, p := range list {
		if assigned[p] {
			continue
		}
		placed := false
		for _, slot := range lm.slots {
			if slot.path == "" {
				slot.path = p
				slot.item.SetTitle(p)
				slot.item.SetTooltip(lm.rowTooltip)
				slot.item.Show()
				assigned[p] = true
				placed = true
				break
			}
		}
		if !placed {
			overflow++
		}
	}
	if len(assigned) == 0 {
		lm.empty.Show()
	} else {
		lm.empty.Hide()
	}
	if overflow > 0 {
		lm.overflow.SetTitle(fmt.Sprintf("…and %d more not shown", overflow))
		lm.overflow.Show()
	} else {
		lm.overflow.Hide()
	}
}

// watch spawns the per-row click goroutines and, when poll is set, the poller
// that keeps the menu in step with entries created outside the tray.
func (lm *listMenu) watch(onRow func(string) []string, poll func() []string) {
	for i := range lm.slots {
		i := i
		go func() {
			for range lm.slots[i].item.ClickedCh {
				lm.mu.Lock()
				p := lm.slots[i].path
				lm.mu.Unlock()
				if p == "" || onRow == nil {
					continue
				}
				if list := onRow(p); list != nil {
					lm.render(list)
				}
			}
		}()
	}
	if poll != nil {
		go func() {
			ticker := time.NewTicker(grantPollInterval)
			defer ticker.Stop()
			for range ticker.C {
				lm.render(poll())
			}
		}()
	}
}

func (t *Tray) onReady() {
	// macOS uses the template (auto-inverts for light/dark menu bars); Windows and
	// Linux ignore it and fall back to the colored icon.
	systray.SetTemplateIcon(iconTemplateBytes, iconBytes)
	systray.SetTooltip("HTML Clay")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		systray.Quit()
	}()

	t.updateItem = systray.AddMenuItem("", "")
	t.updateItem.Hide()
	systray.AddSeparator()

	exampleItem := systray.AddMenuItem("Open Example File", "Create and open a sample self-saving HTML file")
	backupsItem := systray.AddMenuItem("Backups", "Open the folder holding every saved version of your files")
	systray.AddSeparator()

	addTrustedItem := t.buildTrustedMenu()
	addWorkspaceItem := t.buildWorkspaceMenu()
	systray.AddSeparator()

	t.buildGrantMenu()
	t.buildOpenedMenu()
	systray.AddSeparator()

	mode := t.cfg.CurrentMode()
	appItem := systray.AddMenuItemCheckbox("App Mode", "", mode == "app")
	browserItem := systray.AddMenuItemCheckbox("Browser Mode", "", mode == "browser")
	systray.AddSeparator()

	loginItem := systray.AddMenuItemCheckbox("Start on Login", "", t.cfg.StartOnLoginEnabled())
	systray.AddSeparator()

	quitItem := systray.AddMenuItem("Quit", "")

	go func() {
		for {
			select {
			case <-exampleItem.ClickedCh:
				go t.onOpenExample()
			case <-backupsItem.ClickedCh:
				go t.onOpenBackups()
			case <-appItem.ClickedCh:
				t.setMode("app", appItem, browserItem)
			case <-browserItem.ClickedCh:
				t.setMode("browser", browserItem, appItem)
			case <-loginItem.ClickedCh:
				t.toggleLoginItem(loginItem)
			case info := <-t.updateCh:
				t.showUpdate(info)
			case <-t.updateItem.ClickedCh:
				if t.updateURL != "" {
					if err := browser.OpenURL(t.updateURL); err != nil {
						fmt.Fprintf(os.Stderr, "[htmlclay] Error opening browser: %v\n", err)
					}
				}
			case <-quitItem.ClickedCh:
				systray.Quit()
			}
		}
	}()

	t.watchTrustedMenu(addTrustedItem)
	t.watchGrantMenu()
	t.watchWorkspaceMenu(addWorkspaceItem)
	if t.openedMenu != nil && t.opened != nil {
		t.openedMenu.watch(t.opened.Revoke, t.opened.List)
	}
}

// buildWorkspaceMenu adds the Workspace Folders submenu: an add row, and rows
// that remove their workspace on click. Polled, because the page-request route
// declares workspaces with no handle to the tray.
func (t *Tray) buildWorkspaceMenu() *systray.MenuItem {
	if t.workspaces == nil {
		return nil
	}
	lm, addItem := newListMenu(
		"Workspace Folders",
		"Folders whose HTML Clay files open editable with no prompts — and can change each other",
		"No workspace folders yet",
		"Add Workspace Folder…",
		"Pick a folder to declare a workspace",
	)
	lm.rowTooltip = "Click to remove this workspace"
	t.workspacesMenu = lm
	if t.workspaces.List != nil {
		lm.render(t.workspaces.List())
	}
	return addItem
}

// watchWorkspaceMenu wires the add flow (in its own goroutine — the folder
// picker blocks) and the row/poll machinery.
func (t *Tray) watchWorkspaceMenu(addItem *systray.MenuItem) {
	if t.workspaces == nil || t.workspacesMenu == nil {
		return
	}
	if addItem != nil && t.workspaces.Add != nil {
		go func() {
			for range addItem.ClickedCh {
				go func() {
					if list := t.workspaces.Add(); list != nil {
						t.workspacesMenu.render(list)
					}
				}()
			}
		}()
	}
	t.workspacesMenu.watch(t.workspaces.Remove, t.workspaces.List)
}

// buildOpenedMenu adds the Opened for Editing submenu: files a page opened
// with the user's approval, revocable per row. Polled, because open-request
// approvals happen on request goroutines with no handle to the tray.
func (t *Tray) buildOpenedMenu() {
	if t.opened == nil {
		return
	}
	lm, _ := newListMenu(
		"Opened for Editing",
		"Files a page opened with your approval this session",
		"Nothing opened from a page",
		"", "",
	)
	lm.rowTooltip = "Click to revoke this file's editing session"
	t.openedMenu = lm
	if t.opened.List != nil {
		lm.render(t.opened.List())
	}
}

// buildTrustedMenu adds the "Trusted Folders" submenu: an "Add" row, a disabled
// placeholder shown when the list is empty, and a fixed pool of hidden rows the
// current folders are rendered into. It returns the "Add" row (nil when no hooks
// are wired) so onReady can watch it. Clicking a folder row untrusts that folder.
func (t *Tray) buildTrustedMenu() *systray.MenuItem {
	if t.trusted == nil {
		return nil
	}
	menu := systray.AddMenuItem("Trusted Folders", "Folders whose HTML opens with no permission prompts")
	addItem := menu.AddSubMenuItem("Add Trusted Folder…", "Pick a folder to trust")
	t.trustedEmpty = menu.AddSubMenuItem("No trusted folders yet", "")
	t.trustedEmpty.Disable()
	t.trustedSlots = make([]*trustedSlot, trustedSlotCount)
	for i := range t.trustedSlots {
		item := menu.AddSubMenuItem("", "")
		item.Hide()
		t.trustedSlots[i] = &trustedSlot{item: item}
	}
	t.trustedOverflow = menu.AddSubMenuItem("", "")
	t.trustedOverflow.Disable()
	t.trustedOverflow.Hide()
	if t.trusted.List != nil {
		t.renderTrusted(t.trusted.List())
	}
	return addItem
}

// watchTrustedMenu spawns one goroutine per clickable trusted-folder row. The add
// flow runs in its own goroutine because the folder picker blocks until the user
// answers, and a blocked select loop would freeze the rest of the menu.
func (t *Tray) watchTrustedMenu(addItem *systray.MenuItem) {
	if t.trusted == nil || addItem == nil {
		return
	}
	go func() {
		for range addItem.ClickedCh {
			go t.addTrusted()
		}
	}()
	for i := range t.trustedSlots {
		i := i
		go func() {
			for range t.trustedSlots[i].item.ClickedCh {
				t.removeTrustedSlot(i)
			}
		}()
	}
}

// renderTrusted reconciles the slot pool with list. A folder keeps its existing
// slot across renders and only vacated slots are reused, so a click already queued
// against a row stays bound to the folder that was in it. Compacting the rows
// instead would let one folder's queued click land on whatever folder shifted into
// its row. Safe to call from any goroutine.
func (t *Tray) renderTrusted(list []string) {
	t.trustedMu.Lock()
	defer t.trustedMu.Unlock()

	want := make(map[string]bool, len(list))
	for _, p := range list {
		want[p] = true
	}
	// Vacate slots whose folder is no longer trusted; leave every other folder in
	// place and note which are already shown.
	assigned := make(map[string]bool, len(list))
	for _, slot := range t.trustedSlots {
		switch {
		case slot.path == "":
		case want[slot.path]:
			assigned[slot.path] = true
		default:
			slot.path = ""
			slot.item.Hide()
		}
	}
	// Place newly trusted folders into the lowest free slots.
	overflow := 0
	for _, p := range list {
		if assigned[p] {
			continue
		}
		placed := false
		for _, slot := range t.trustedSlots {
			if slot.path == "" {
				slot.path = p
				slot.item.SetTitle(p)
				slot.item.SetTooltip("Click to stop trusting this folder")
				slot.item.Show()
				assigned[p] = true
				placed = true
				break
			}
		}
		if !placed {
			overflow++
		}
	}
	if len(assigned) == 0 {
		t.trustedEmpty.Show()
	} else {
		t.trustedEmpty.Hide()
	}
	// The pool is fixed because systray cannot delete rows. Say so in the menu
	// rather than let the extras disappear silently: they are still trusted and
	// still granting silent reads.
	if overflow > 0 {
		t.trustedOverflow.SetTitle(fmt.Sprintf("…and %d more not shown", overflow))
		t.trustedOverflow.Show()
		fmt.Fprintf(os.Stderr, "[htmlclay] %d trusted folders exceed %d tray rows; the extras stay active but are not listed\n",
			len(list), len(t.trustedSlots))
	} else {
		t.trustedOverflow.Hide()
	}
}

func (t *Tray) addTrusted() {
	if t.trusted.Add == nil {
		return
	}
	if list := t.trusted.Add(); list != nil {
		t.renderTrusted(list)
	}
}

func (t *Tray) removeTrustedSlot(i int) {
	t.trustedMu.Lock()
	dir := t.trustedSlots[i].path
	t.trustedMu.Unlock()
	if dir == "" || t.trusted.Remove == nil {
		return
	}
	if list := t.trusted.Remove(dir); list != nil {
		t.renderTrusted(list)
	}
}

// buildGrantMenu adds the "Temporary Access Granted" submenu: a disabled
// placeholder shown when the list is empty, and a fixed pool of hidden rows the
// current grants are rendered into. There is no "Add" row (grants come from the
// permission dialog, not the tray). Clicking a grant row revokes that grant. The
// list starts empty at build time and is kept current by pollGrants (see
// watchGrantMenu), since grants are installed by the broker after startup.
func (t *Tray) buildGrantMenu() {
	if t.grants == nil {
		return
	}
	menu := systray.AddMenuItem("Temporary Access Granted", "Read access you allowed this session")
	t.grantEmpty = menu.AddSubMenuItem("No temporary access granted", "")
	t.grantEmpty.Disable()
	t.grantSlots = make([]*trustedSlot, trustedSlotCount)
	for i := range t.grantSlots {
		item := menu.AddSubMenuItem("", "")
		item.Hide()
		t.grantSlots[i] = &trustedSlot{item: item}
	}
	t.grantOverflow = menu.AddSubMenuItem("", "")
	t.grantOverflow.Disable()
	t.grantOverflow.Hide()
	if t.grants.List != nil {
		t.renderGrants(t.grants.List())
	}
}

// watchGrantMenu spawns one goroutine per clickable grant row and starts the
// poller that keeps the submenu in step with grants created after startup.
func (t *Tray) watchGrantMenu() {
	if t.grants == nil {
		return
	}
	for i := range t.grantSlots {
		i := i
		go func() {
			for range t.grantSlots[i].item.ClickedCh {
				t.revokeGrantSlot(i)
			}
		}()
	}
	if t.grants.List != nil {
		go t.pollGrants()
	}
}

// pollGrants re-renders the grant submenu on a fixed interval so a grant installed
// by the broker after the tray was built appears (and becomes revocable) without an
// event channel from the broker to the tray. renderGrants is idempotent, so a tick
// with no change does nothing visible.
func (t *Tray) pollGrants() {
	ticker := time.NewTicker(grantPollInterval)
	defer ticker.Stop()
	for range ticker.C {
		t.renderGrants(t.grants.List())
	}
}

// renderGrants reconciles the slot pool with list. A grant keeps its existing slot
// across renders and only vacated slots are reused, so a click already queued
// against a row stays bound to the grant that was in it. Safe to call from any
// goroutine.
func (t *Tray) renderGrants(list []string) {
	t.grantMu.Lock()
	defer t.grantMu.Unlock()

	want := make(map[string]bool, len(list))
	for _, p := range list {
		want[p] = true
	}
	// Vacate slots whose grant is gone; leave every other grant in place and note
	// which are already shown.
	assigned := make(map[string]bool, len(list))
	for _, slot := range t.grantSlots {
		switch {
		case slot.path == "":
		case want[slot.path]:
			assigned[slot.path] = true
		default:
			slot.path = ""
			slot.item.Hide()
		}
	}
	// Place newly granted roots into the lowest free slots.
	overflow := 0
	for _, p := range list {
		if assigned[p] {
			continue
		}
		placed := false
		for _, slot := range t.grantSlots {
			if slot.path == "" {
				slot.path = p
				slot.item.SetTitle(p)
				slot.item.SetTooltip("Click to revoke this access")
				slot.item.Show()
				assigned[p] = true
				placed = true
				break
			}
		}
		if !placed {
			overflow++
		}
	}
	if len(assigned) == 0 {
		t.grantEmpty.Show()
	} else {
		t.grantEmpty.Hide()
	}
	// The pool is fixed because systray cannot delete rows. Say so in the menu
	// rather than let the extras disappear silently: they are still granted and
	// still readable.
	if overflow > 0 {
		t.grantOverflow.SetTitle(fmt.Sprintf("…and %d more not shown", overflow))
		t.grantOverflow.Show()
		fmt.Fprintf(os.Stderr, "[htmlclay] %d runtime grants exceed %d tray rows; the extras stay active but are not listed\n",
			len(list), len(t.grantSlots))
	} else {
		t.grantOverflow.Hide()
	}
}

func (t *Tray) revokeGrantSlot(i int) {
	t.grantMu.Lock()
	dir := t.grantSlots[i].path
	t.grantMu.Unlock()
	if dir == "" || t.grants.Revoke == nil {
		return
	}
	if list := t.grants.Revoke(dir); list != nil {
		t.renderGrants(list)
	}
}

func (t *Tray) setMode(mode string, check, uncheck *systray.MenuItem) {
	prev := t.cfg.SetMode(mode)
	if err := t.cfg.Save(); err != nil {
		t.cfg.SetMode(prev)
		return
	}
	check.Check()
	uncheck.Uncheck()
}

func (t *Tray) toggleLoginItem(loginItem *systray.MenuItem) {
	newVal := !t.cfg.StartOnLoginEnabled()
	execPath, err := os.Executable()
	if err != nil || execPath == "" {
		fmt.Fprintf(os.Stderr, "[htmlclay] cannot determine executable path: %v\n", err)
		return
	}

	if err := platform.SetLoginItem(newVal, execPath); err != nil {
		return
	}

	t.cfg.SetStartOnLogin(newVal)
	if err := t.cfg.Save(); err != nil {
		t.cfg.SetStartOnLogin(!newVal)
		platform.SetLoginItem(!newVal, execPath)
		return
	}

	if newVal {
		loginItem.Check()
	} else {
		loginItem.Uncheck()
	}
}

func (t *Tray) showUpdate(info UpdateInfo) {
	t.updateURL = info.URL
	t.updateItem.SetTitle(fmt.Sprintf("Update available: v%s", info.Version))
	t.updateItem.SetTooltip("Click to download")
	t.updateItem.Show()
}

func (t *Tray) onExit() {
	t.onQuit()
}
