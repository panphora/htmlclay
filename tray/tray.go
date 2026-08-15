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

// listPollInterval is how often a submenu re-renders from its authoritative
// list. A folder can be trusted by the permission dialog or by a page request,
// neither of which has a handle to the tray, so the menu is refreshed by polling
// rather than pushed to. Two seconds is well below the time it takes a user to
// open the submenu, and the render is a cheap no-op when nothing changed.
const listPollInterval = 2 * time.Second

//go:embed icon.png
var iconBytes []byte

//go:embed icon-template.png
var iconTemplateBytes []byte

type UpdateInfo struct {
	Version string
	URL     string
}

// Row is one entry in a list submenu. Label is what the user sees, Path is what
// the hook is called with. They are carried separately because they differ: a
// folder that is missing or whose identity no longer matches is labelled as
// such. Recovering the path by stripping that marker off the label was the old
// protocol, and it broke for anyone who named a folder to end in the marker.
type Row struct {
	Path  string
	Label string
}

// TrustedFolderHooks are the app operations the Trusted Folders submenu drives.
// Each returns the authoritative list so the tray always re-renders from truth
// rather than tracking state itself. Add pops the native folder picker and
// trusts the choice; Remove untrusts one (asking first, app-side, because it
// closes a live origin); List is the initial render and the poll.
type TrustedFolderHooks struct {
	List   func() []Row
	Add    func() []Row
	Remove func(path string) []Row
}

// slotCount is the number of pre-allocated submenu rows. systray cannot delete
// menu items, so a list is rendered into a fixed pool of rows that are shown,
// retitled, and hidden. Beyond this many entries the extras stay active but are
// not shown (logged on render).
const slotCount = 16

type slot struct {
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

	trusted     *TrustedFolderHooks
	trustedMenu *listMenu
}

func Run(cfg *config.Config, onOpenExample func(), onOpenBackups func(), onQuit func(), updateCh <-chan UpdateInfo, trusted *TrustedFolderHooks) {
	t := &Tray{
		cfg:           cfg,
		onOpenExample: onOpenExample,
		onOpenBackups: onOpenBackups,
		onQuit:        onQuit,
		updateCh:      updateCh,
		trusted:       trusted,
	}
	systray.Run(t.onReady, t.onExit)
}

// listMenu renders an authoritative list into a fixed slot pool, with an
// optional add row, per-row click actions, and polling for lists that change
// outside the tray.
type listMenu struct {
	mu         sync.Mutex
	empty      *systray.MenuItem
	overflow   *systray.MenuItem
	slots      []*slot
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
	lm.slots = make([]*slot, slotCount)
	for i := range lm.slots {
		item := menu.AddSubMenuItem("", "")
		item.Hide()
		lm.slots[i] = &slot{item: item}
	}
	lm.overflow = menu.AddSubMenuItem("", "")
	lm.overflow.Disable()
	lm.overflow.Hide()
	return lm, addItem
}

// render reconciles the slot pool with rows. An entry keeps its existing slot
// across renders and only vacated slots are reused, so a click already queued
// against a row stays bound to the entry that was in it. Compacting the rows
// instead would let one entry's queued click land on whatever entry shifted into
// its row, which with a destructive action is the difference between untrusting
// the folder you clicked and untrusting a different one. Safe to call from any
// goroutine.
func (lm *listMenu) render(rows []Row) {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	label := make(map[string]string, len(rows))
	for _, r := range rows {
		label[r.Path] = r.Label
	}
	// Vacate slots whose entry is gone; leave every other entry in place, and
	// refresh its label so a folder that has since gone missing says so.
	assigned := make(map[string]bool, len(rows))
	for _, s := range lm.slots {
		if s.path == "" {
			continue
		}
		if l, ok := label[s.path]; ok {
			s.item.SetTitle(l)
			assigned[s.path] = true
			continue
		}
		s.path = ""
		s.item.Hide()
	}
	// Place new entries into the lowest free slots.
	overflow := 0
	for _, r := range rows {
		if assigned[r.Path] {
			continue
		}
		placed := false
		for _, s := range lm.slots {
			if s.path == "" {
				s.path = r.Path
				s.item.SetTitle(r.Label)
				s.item.SetTooltip(lm.rowTooltip)
				s.item.Show()
				assigned[r.Path] = true
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
	// The pool is fixed because systray cannot delete rows. Say so in the menu
	// rather than let the extras disappear silently: they are still trusted and
	// still granting silent reads and writes.
	if overflow > 0 {
		lm.overflow.SetTitle(fmt.Sprintf("…and %d more not shown", overflow))
		lm.overflow.Show()
		fmt.Fprintf(os.Stderr, "[htmlclay] %d trusted folders exceed %d tray rows; the extras stay active but are not listed\n",
			len(rows), len(lm.slots))
	} else {
		lm.overflow.Hide()
	}
}

// watch spawns the per-row click goroutines and, when poll is set, the poller
// that keeps the menu in step with entries created outside the tray.
func (lm *listMenu) watch(onRow func(string) []Row, poll func() []Row) {
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
				// In its own goroutine: removing asks for confirmation first,
				// and a blocking dialog here would freeze every other row.
				go func() {
					if rows := onRow(p); rows != nil {
						lm.render(rows)
					}
				}()
			}
		}()
	}
	if poll != nil {
		go func() {
			ticker := time.NewTicker(listPollInterval)
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
}

// buildTrustedMenu adds the Trusted Folders submenu: an add row, and rows that
// untrust their folder on click. Polled, because the permission dialog and a
// page's own request both trust folders with no handle to the tray.
func (t *Tray) buildTrustedMenu() *systray.MenuItem {
	if t.trusted == nil {
		return nil
	}
	lm, addItem := newListMenu(
		"Trusted Folders",
		"Folders whose HTML Clay files open editable with no prompts",
		"No trusted folders yet",
		"Trust a Folder…",
		"Pick a folder to trust",
	)
	lm.rowTooltip = "Click to stop trusting this folder"
	t.trustedMenu = lm
	if t.trusted.List != nil {
		lm.render(t.trusted.List())
	}
	return addItem
}

// watchTrustedMenu wires the add flow (in its own goroutine, because the folder
// picker blocks until the user answers) and the row/poll machinery.
func (t *Tray) watchTrustedMenu(addItem *systray.MenuItem) {
	if t.trusted == nil || t.trustedMenu == nil {
		return
	}
	if addItem != nil && t.trusted.Add != nil {
		go func() {
			for range addItem.ClickedCh {
				go func() {
					if rows := t.trusted.Add(); rows != nil {
						t.trustedMenu.render(rows)
					}
				}()
			}
		}()
	}
	t.trustedMenu.watch(t.trusted.Remove, t.trusted.List)
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
