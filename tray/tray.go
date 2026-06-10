package tray

import (
	_ "embed"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"fyne.io/systray"
	"github.com/panphora/htmlclay/browser"
	"github.com/panphora/htmlclay/config"
	"github.com/panphora/htmlclay/platform"
)

//go:embed icon.png
var iconBytes []byte

type UpdateInfo struct {
	Version string
	URL     string
}

type Tray struct {
	cfg        *config.Config
	onQuit     func()
	updateCh   <-chan UpdateInfo
	updateItem *systray.MenuItem
	updateURL  string
}

func Run(cfg *config.Config, onQuit func(), updateCh <-chan UpdateInfo) {
	t := &Tray{cfg: cfg, onQuit: onQuit, updateCh: updateCh}
	systray.Run(t.onReady, t.onExit)
}

func (t *Tray) onReady() {
	systray.SetIcon(iconBytes)
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

	appItem := systray.AddMenuItemCheckbox("App Mode", "", t.cfg.Mode == "app")
	browserItem := systray.AddMenuItemCheckbox("Browser Mode", "", t.cfg.Mode == "browser")
	systray.AddSeparator()

	loginItem := systray.AddMenuItemCheckbox("Start on Login", "", t.cfg.StartOnLogin)
	systray.AddSeparator()

	quitItem := systray.AddMenuItem("Quit", "")

	go func() {
		for {
			select {
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
}

func (t *Tray) setMode(mode string, check, uncheck *systray.MenuItem) {
	prev := t.cfg.Mode
	t.cfg.Mode = mode
	if err := t.cfg.Save(); err != nil {
		t.cfg.Mode = prev
		return
	}
	check.Check()
	uncheck.Uncheck()
}

func (t *Tray) toggleLoginItem(loginItem *systray.MenuItem) {
	newVal := !t.cfg.StartOnLogin
	execPath, err := os.Executable()
	if err != nil || execPath == "" {
		fmt.Fprintf(os.Stderr, "[htmlclay] cannot determine executable path: %v\n", err)
		return
	}

	if err := platform.SetLoginItem(newVal, execPath); err != nil {
		return
	}

	t.cfg.StartOnLogin = newVal
	if err := t.cfg.Save(); err != nil {
		t.cfg.StartOnLogin = !newVal
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
