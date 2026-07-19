package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/panphora/htmlclay/browser"
	"github.com/panphora/htmlclay/config"
	"github.com/panphora/htmlclay/logging"
	"github.com/panphora/htmlclay/platform"
	"github.com/panphora/htmlclay/server"
	"github.com/panphora/htmlclay/session"
	"github.com/panphora/htmlclay/tray"
	"github.com/panphora/htmlclay/update"
)

var version = "1.1.0"

//go:embed example.htmlclay
var exampleHTML []byte

func fatal(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "[htmlclay] "+format+"\n", args...)
	os.Exit(1)
}

func resolveSymlinks(absPath string) (string, error) {
	resolved, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

type app struct {
	cfg      *config.Config
	logger   *logging.Logger
	sessions *session.Manager
	srv      *server.Server
	si       platform.SingleInstance
	port     int
	noTray   bool
}

func migrateConfigDir() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	oldDir := filepath.Join(home, ".htmlclay")

	newDir, err := config.Dir()
	if err != nil {
		return
	}

	if oldDir == newDir {
		return
	}

	oldInfo, err := os.Stat(oldDir)
	if err != nil || !oldInfo.IsDir() {
		return
	}

	if _, err := os.Stat(newDir); err == nil {
		return
	}

	if err := os.MkdirAll(filepath.Dir(newDir), 0755); err != nil {
		return
	}

	if err := os.Rename(oldDir, newDir); err != nil {
		fmt.Fprintf(os.Stderr, "[htmlclay] Could not migrate config from %s to %s: %v\n", oldDir, newDir, err)
	} else {
		fmt.Fprintf(os.Stderr, "[htmlclay] Migrated config from %s to %s\n", oldDir, newDir)
	}
}

func main() {
	appMode := flag.Bool("app", false, "Open in App Mode (chromeless window)")
	browserMode := flag.Bool("browser", false, "Open in Browser Mode")
	noTray := flag.Bool("no-tray", false, "Run without system tray (signal-based shutdown)")
	flag.Parse()

	migrateConfigDir()

	fmt.Fprintln(os.Stderr, "[htmlclay] Starting up...")

	a := &app{noTray: *noTray}
	a.initConfig()
	defer a.si.Unlock()

	a.initLogger()
	defer a.logger.Close()

	a.startServer()

	// Apply one-shot CLI mode overrides after ResolvePort has persisted the
	// config, so an ephemeral -app/-browser run does not rewrite the saved mode.
	if *appMode {
		a.cfg.Mode = "app"
	} else if *browserMode {
		a.cfg.Mode = "browser"
	}

	a.refreshLoginItem()

	args := flag.Args()
	if len(args) > 0 {
		a.logger.Printf("Opening file: %s", args[0])
		a.openFile(args[0])
	}

	a.si.OnFileReceived(func(path string) {
		a.logger.Printf("Received file from another instance: %s", path)
		a.openFile(path)
	})

	// macOS delivers Finder double-clicks as Apple Events, not argv; this hooks
	// them into the same open path. No-op on other platforms.
	platform.OnOpenFile(func(path string) {
		a.logger.Printf("Received open-file event: %s", path)
		a.openFile(path)
	})

	updateCh := make(chan tray.UpdateInfo, 1)
	go func() {
		if info := update.Check(version, update.DefaultVersionURL); info != nil {
			a.logger.Printf("Update available: v%s at %s", info.Version, info.URL)
			updateCh <- tray.UpdateInfo{Version: info.Version, URL: info.URL}
		}
	}()

	a.run(updateCh)
}

func (a *app) initConfig() {
	if err := config.EnsureDir(); err != nil {
		fatal("Error creating config dir: %v", err)
	}
	configDir, err := config.Dir()
	if err != nil {
		fatal("Error resolving config dir: %v", err)
	}
	fmt.Fprintf(os.Stderr, "[htmlclay] Config dir: %s\n", configDir)

	a.si = platform.NewSingleInstance(configDir)
	isPrimary, err := a.si.TryLock()
	if err != nil {
		fatal("Error checking single instance: %v", err)
	}

	if !isPrimary {
		fmt.Fprintln(os.Stderr, "[htmlclay] Another instance running, forwarding file...")
		args := flag.Args()
		if len(args) > 0 {
			filePath, err := filepath.Abs(args[0])
			if err != nil {
				fatal("Error resolving path: %v", err)
			}
			if err := a.si.SendFilePath(filePath); err != nil {
				fatal("Error sending file to running instance: %v", err)
			}
		}
		os.Exit(0)
	}
	fmt.Fprintln(os.Stderr, "[htmlclay] Primary instance, proceeding...")

	cfg, err := config.Load()
	if err != nil {
		fatal("Error loading config: %v", err)
	}
	fmt.Fprintf(os.Stderr, "[htmlclay] Config loaded: mode=%s\n", cfg.Mode)
	a.cfg = cfg
}

func (a *app) initLogger() {
	configDir, err := config.Dir()
	if err != nil {
		fatal("Error resolving config dir: %v", err)
	}
	logPath := filepath.Join(configDir, "htmlclay.log")
	logger, err := logging.NewDualWrite(logPath)
	if err != nil {
		fatal("Error creating logger: %v", err)
	}
	a.logger = logger
	a.logger.Printf("Logger initialized at %s", logPath)
}

func (a *app) startServer() {
	ln, err := a.cfg.ResolvePort()
	if err != nil {
		fatal("Error resolving port: %v", err)
	}
	a.port = ln.Addr().(*net.TCPAddr).Port
	a.logger.Printf("Port resolved: %d", a.port)

	sessions, err := session.NewManager()
	if err != nil {
		fatal("Error creating session manager: %v", err)
	}
	a.sessions = sessions

	a.srv = server.New(ln, sessions, a.logger)
	go func() {
		if err := a.srv.Start(); err != nil {
			fatal("Server error: %v", err)
		}
	}()

	a.logger.Printf("Server started on 127.0.0.1:%d", a.port)
	a.logger.Printf("Launch mode: %s", a.cfg.Mode)
}

func (a *app) openFile(filePath string) {
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		a.logger.Printf("Error resolving path: %v", err)
		return
	}

	if _, err := os.Stat(absPath); os.IsNotExist(err) {
		a.logger.Printf("File not found: %s", absPath)
		return
	}

	absPath, err = resolveSymlinks(absPath)
	if err != nil {
		a.logger.Printf("Error resolving symlinks: %v", err)
		return
	}

	if existing, ok := a.sessions.LookupByPath(absPath); ok {
		a.logger.Printf("File already open, re-launching window: %s", absPath)
		a.launchBrowser(fileURL(a.port, existing.RelPath))
		return
	}

	f, err := a.sessions.Register(absPath)
	if err != nil {
		a.logger.Printf("Error registering file: %v", err)
		if errors.Is(err, session.ErrOutsideHome) {
			msg := fmt.Sprintf("%s is outside your home folder. HTML Clay only opens files inside %s.",
				filepath.Base(absPath), a.sessions.HomeDir())
			go func() {
				if nErr := platform.Notify("HTML Clay can't open this file", msg); nErr != nil {
					a.logger.Printf("Could not show notification: %v", nErr)
				}
			}()
		}
		return
	}

	fileURL := fileURL(a.port, f.RelPath)
	a.logger.Printf("Serving %s at %s", f.Name, fileURL)

	a.launchBrowser(fileURL)
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
		a.logger.Printf("Error resolving home dir: %v", err)
		return
	}
	path := filepath.Join(home, "htmlclay", "examples", "welcome.htmlclay")
	if err := ensureExampleFile(path); err != nil {
		a.logger.Printf("Error creating example file: %v", err)
		return
	}
	a.openFile(path)
}

func (a *app) run(updateCh <-chan tray.UpdateInfo) {
	if a.noTray {
		a.logger.Printf("Running without tray, waiting for signal...")
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		<-sigCh
		a.shutdown()
	} else {
		a.logger.Printf("Starting system tray...")
		tray.Run(a.cfg, a.openExample, func() {
			a.shutdown()
		}, updateCh)
		a.logger.Printf("Tray exited")
	}
}

func (a *app) refreshLoginItem() {
	if !a.cfg.StartOnLogin {
		return
	}
	execPath, err := os.Executable()
	if err != nil || execPath == "" {
		return
	}
	// Re-register on every launch so a moved or updated binary keeps a valid path.
	if err := platform.SetLoginItem(true, execPath); err != nil {
		a.logger.Printf("Could not refresh login item: %v", err)
	}
}

func (a *app) shutdown() {
	a.logger.Printf("Shutting down...")
	a.sessions.RevokeAll()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := a.srv.Shutdown(ctx); err != nil {
		a.logger.Printf("Graceful shutdown timed out (%v), forcing close", err)
		a.srv.Close()
	}
}

func fileURL(port int, relPath string) string {
	base := fmt.Sprintf("http://127.0.0.1:%d/", port)
	result, err := url.JoinPath(base, relPath)
	if err != nil {
		return base + relPath
	}
	return result
}

func (a *app) launchBrowser(targetURL string) {
	if a.cfg.Mode == "app" {
		if a.tryAppMode(targetURL) {
			return
		}
	}
	a.logger.Printf("Opening in default browser: %s", targetURL)
	if err := browser.OpenURL(targetURL); err != nil {
		a.logger.Printf("Error opening browser: %v", err)
	}
}

func (a *app) tryAppMode(targetURL string) bool {
	chromePath := browser.FindChromium()
	if chromePath == "" {
		a.logger.Printf("No Chromium found, falling back to default browser")
		return false
	}
	a.logger.Printf("Launching Chrome App Mode: %s", chromePath)
	configDir, err := config.Dir()
	if err != nil {
		a.logger.Printf("Error resolving config dir: %v, falling back to browser", err)
		return false
	}
	profileDir := filepath.Join(configDir, "chrome-profile")
	if _, err := browser.LaunchAppMode(chromePath, targetURL, profileDir); err != nil {
		a.logger.Printf("App Mode failed: %v, falling back to browser", err)
		return false
	}
	return true
}
