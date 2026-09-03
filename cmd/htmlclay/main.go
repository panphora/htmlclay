package main

import (
	_ "embed"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/panphora/htmlclay/internal/config"
	"github.com/panphora/htmlclay/internal/logging"
	"github.com/panphora/htmlclay/internal/platform"
	"github.com/panphora/htmlclay/internal/server"
	"github.com/panphora/htmlclay/internal/session"
	"github.com/panphora/htmlclay/internal/tray"
	"github.com/panphora/htmlclay/internal/trust"
	"github.com/panphora/htmlclay/internal/update"
	"github.com/panphora/htmlclay/internal/versions"
)

var version = "1.8.0"

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

// appRuntime holds everything shared across every origin: config, logging, the
// versions store, the single-instance lock, and the one live-sync runtime. Each
// origin becomes its own site (see sites.go) on its own port, so the browser
// treats unrelated trees as separate origins; the runtime is what they share.
type appRuntime struct {
	cfg       *config.Config
	logger    *logging.Logger
	versions  *versions.Store
	si        platform.SingleInstance
	ls        *server.LiveSync
	home      string
	configDir string
	guard     func(dir string) bool
	// policy holds the rules about which folders may be trusted, and on whose
	// say-so. Pure functions; see package trust.
	policy trust.Policy
	// confirm, when set, overrides the native read-permission dialog. Production
	// leaves it nil (the server uses the real dialog); tests inject a decision.
	confirm func(title, message string, allowTrust bool) (platform.ConfirmChoice, error)
	// confirmTrust overrides the two-button dialog behind a page's request to
	// trust its own folder. It is a separate seam from confirm on purpose: a test
	// that auto-approves read grants must not silently auto-approve the
	// write-granting dialog too.
	confirmTrust func(title, message, affirmative string) (bool, error)
	// notify, when set, overrides the native notification. Production leaves it nil
	// (a real banner); tests capture the message instead of putting one on the
	// user's screen. Unlike confirm, this is read live from background goroutines
	// rather than snapshotted at buildSite, so set it before opening any site.
	notify func(title, message string) error
}

type app struct {
	rt       *appRuntime
	mu       sync.Mutex
	sites    []*site
	parked   []*parked
	stopping bool
	noTray   bool
	// dialogAdvice is non-empty when this machine cannot raise a permission
	// dialog at all. It is read once at startup and shown in two places, so it
	// is held rather than asked for again: platform.MissingDialogAdvice runs
	// exec.LookPath, and the tray builds its menu long after the check.
	dialogAdvice string
	// loaded records what config.Load had to do to the file on disk, so a
	// one-time upgrade (deleting App Mode's browser profile) can run after the
	// logger exists rather than during config parsing.
	loaded config.Result
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
	// The wire CLI is a client of a running app, so it must return before
	// anything below: initConfig takes the single-instance lock, and flag.Parse
	// stops at the first non-flag argument, which would leave a subcommand's own
	// flags unparsed and forward "wire" to openFile as a path.
	if code, handled := wireDispatch(os.Args); handled {
		os.Exit(code)
	}

	noTray := flag.Bool("no-tray", false, "Run without system tray (signal-based shutdown)")
	unregister := flag.Bool("unregister", false, "Remove the file associations and the Start on Login entry, then exit")
	flag.Parse()

	// Before initConfig, and so before the single-instance lock, on purpose. A
	// launch that finds the lock held forwards its arguments to the running app
	// and exits, so an --unregister placed after it would quietly do nothing
	// whenever HTML Clay happened to be running, which is the state a person is
	// most likely to be in while uninstalling.
	if *unregister {
		os.Exit(runUnregister())
	}

	migrateConfigDir()

	fmt.Fprintln(os.Stderr, "[htmlclay] Starting up...")

	a := &app{rt: &appRuntime{}, noTray: *noTray}
	a.initConfig()
	defer a.rt.si.Unlock()

	a.initLogger()
	defer a.rt.logger.Close()

	a.startRuntime()
	a.finishUpgrade()

	a.refreshLoginItem()
	a.refreshFileTypes()
	a.warnMissingDialogs()

	// Bind every remembered port before argv is processed, so a URL bookmarked
	// before the last quit answers on the first launch after it. A file named on
	// the command line then routes onto a site that is already listening.
	a.startSites()

	args := flag.Args()
	if len(args) > 0 {
		a.rt.logger.Printf("Opening file: %s", args[0])
		a.openFile(args[0])
	}

	a.rt.si.OnFileReceived(func(path string) {
		a.rt.logger.Printf("Received file from another instance: %s", path)
		a.openFile(path)
	})

	// macOS delivers Finder double-clicks as Apple Events, not argv; this hooks
	// them into the same open path. No-op on other platforms.
	platform.OnOpenFile(func(path string) {
		a.rt.logger.Printf("Received open-file event: %s", path)
		a.openFile(path)
	})

	updateCh := make(chan tray.UpdateInfo, 1)
	go func() {
		if info := update.Check(version, update.DefaultVersionURL); info != nil {
			a.rt.logger.Printf("Update available: v%s at %s", info.Version, info.URL)
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

	a.rt.si = platform.NewSingleInstance(configDir)
	isPrimary, err := a.rt.si.TryLock()
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
			if err := a.rt.si.SendFilePath(filePath); err != nil {
				fatal("Error sending file to running instance: %v", err)
			}
		}
		os.Exit(0)
	}
	fmt.Fprintln(os.Stderr, "[htmlclay] Primary instance, proceeding...")

	cfg, res, err := config.Load(platform.DirIdentity)
	if err != nil {
		fatal("Error loading config: %v", err)
	}
	fmt.Fprintf(os.Stderr, "[htmlclay] Config loaded: %d trusted folder(s)\n", len(cfg.TrustedFolderList()))
	a.rt.cfg = cfg
	a.loaded = res
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
	a.rt.logger = logger
	a.rt.logger.Printf("Logger initialized at %s", logPath)
}

// startRuntime builds the process-wide state shared by every site. It binds no
// port; startSites and route bind their own.
func (a *app) startRuntime() {
	home, err := os.UserHomeDir()
	if err != nil {
		fatal("Error resolving home directory: %v", err)
	}
	// Normalize once so containment checks and user-facing messages agree with
	// the session manager, which resolves symlinks in home internally.
	if resolved, rErr := resolveSymlinks(home); rErr == nil {
		home = resolved
	}
	a.rt.home = home

	configDir, err := config.Dir()
	if err != nil {
		fatal("Error resolving config dir: %v", err)
	}
	a.rt.configDir = configDir
	if resolved, rErr := resolveSymlinks(configDir); rErr == nil {
		a.rt.configDir = resolved
	}

	a.rt.versions = versions.New(filepath.Join(configDir, "versions"))
	// Prune once at startup; every later pass is opportunistic after a backup.
	go a.rt.versions.PruneAll()

	a.rt.ls = server.NewLiveSync(a.rt.versions, a.rt.logger)

	// A grant must not cover htmlclay's own config tree in either direction: not
	// a grant inside it, and not a grant of an ancestor that swallows it. The
	// versions dir lives under the config dir, so one check covers both. The
	// serve path denies the same tree structurally (Server.SetInternalDir); this
	// guard just stops the grant from being offered in the first place.
	forbidden := a.rt.configDir
	a.rt.guard = func(dir string) bool {
		return session.EqualOrUnder(dir, forbidden) || session.EqualOrUnder(forbidden, dir)
	}
	a.rt.policy = trust.Policy{Home: a.rt.home, Guard: a.rt.guard}

	a.rt.logger.Printf("Runtime ready (home=%s)", a.rt.home)
}

// finishUpgrade runs the one-time work a config from an older version implies.
// It lives here rather than in config because it touches the filesystem and
// talks to the user, and because this is already where one-shot upgrade work
// (migrateConfigDir) lives.
func (a *app) finishUpgrade() {
	// Both migrations rewrote the config in memory only. Persisting it here is
	// what makes them one-time. Without this they are re-derived on every Load,
	// and nothing else on a settled launch saves: trustFolder, untrustFolder, the
	// Start-on-Login toggle and rememberPort are the only writers, and a launch
	// that trusts nothing new and keeps its port touches none of them. A pin that
	// never lands is a pin re-taken from whatever directory sits at that path at
	// the time, which is exactly the swap it exists to catch.
	if a.loaded.PromotedLegacy || a.loaded.PinnedIdentities {
		if err := a.rt.cfg.Save(); err != nil {
			a.rt.logger.Printf("Could not persist the upgraded config: %v", err)
		}
	}
	if a.loaded.PromotedLegacy {
		a.rt.logger.Printf("Promoted legacy read-only trusted folders to trusted folders")
	}
	if a.loaded.PinnedIdentities {
		a.rt.logger.Printf("Pinned trusted folders that were recorded without an identity fingerprint")
	}
	if !a.loaded.HadAppMode {
		return
	}
	// App Mode ran pages in a private Chromium profile. That profile is now
	// unreachable, so anything a page stored in it (localStorage, IndexedDB) is
	// already gone from the user's point of view; leaving the directory on disk
	// only hides that. Delete it and say so once.
	profile := filepath.Join(a.rt.configDir, "chrome-profile")
	if _, err := os.Stat(profile); err != nil {
		return
	}
	if err := os.RemoveAll(profile); err != nil {
		a.rt.logger.Printf("Could not remove the old App Mode profile at %s: %v", profile, err)
		return
	}
	a.rt.logger.Printf("Removed the old App Mode browser profile at %s", profile)
	go func() {
		msg := "HTML Clay now always opens files in your normal browser, so App Mode is gone.\n\n" +
			"Its separate browser profile has been removed. If a page saved anything in that " +
			"profile's own storage, it is no longer available."
		if err := a.notifyUser("HTML Clay", msg); err != nil {
			a.rt.logger.Printf("Could not notify about the removed App Mode profile: %v", err)
		}
	}()
}

func (a *app) run(updateCh <-chan tray.UpdateInfo) {
	if a.noTray {
		a.rt.logger.Printf("Running without tray, waiting for signal...")
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		<-sigCh
		a.shutdown()
		return
	}
	a.rt.logger.Printf("Starting system tray...")
	tray.Run(a.rt.cfg, version, a.openExample, a.openBackups, func() {
		a.shutdown()
	}, updateCh, &tray.TrustedFolderHooks{
		List:   a.trustedFolderRows,
		Add:    a.pickAndTrustFolder,
		Remove: a.removeTrustedFolder,
	}, a.dialogAdvice)
	a.rt.logger.Printf("Tray exited")
}

// runUnregister undoes what a launch registers: the file associations and the
// Start on Login entry. It touches nothing else. config.json stays, because the
// trusted-folder list is the record of what the user granted rather than part of
// the installation, and so do the saved versions under the config directory.
func runUnregister() int {
	code := 0
	if err := platform.UnregisterFileTypes(); err != nil {
		fmt.Fprintf(os.Stderr, "[htmlclay] Could not remove the file associations: %v\n", err)
		code = 1
	}
	if err := platform.SetLoginItem(false, ""); err != nil {
		fmt.Fprintf(os.Stderr, "[htmlclay] Could not remove the Start on Login entry: %v\n", err)
		code = 1
	}
	if code == 0 {
		// "set up itself" is the honest wording on all three platforms: on Windows
		// that is four registry keys, and on macOS and Linux it is none, because
		// the app bundle and the freedesktop MIME database own those.
		fmt.Fprintln(os.Stderr, "[htmlclay] Removed the Start on Login entry and every file association HTML Clay set up itself.")
	}
	fmt.Fprintln(os.Stderr, "[htmlclay] Your files, your trusted folders and your saved versions are untouched.")
	fmt.Fprintln(os.Stderr, "[htmlclay] Starting HTML Clay again puts them back, so delete the program now if you are done with it.")
	return code
}

// warnMissingDialogs turns a silent failure into an instruction. On a desktop
// with neither zenity nor kdialog every permission prompt fails closed, which is
// correct, but the result the user sees is a file that will not open and nothing
// on screen saying why.
//
// Three channels, because each one can be absent on exactly the machine that
// needs it: the log always works, the tray row is permanent and needs no
// notification daemon, and the banner is the only one that appears without the
// user going looking, but it runs through notify-send, which the same minimal
// desktop may not have either.
func (a *app) warnMissingDialogs() {
	a.dialogAdvice = platform.MissingDialogAdvice()
	if a.dialogAdvice == "" {
		return
	}
	a.rt.logger.Printf("%s", a.dialogAdvice)
	msg := a.dialogAdvice + "\n\nUntil then, a file opened from outside a trusted folder is refused, " +
		"because the prompt that would allow it cannot be shown. Trusted folders themselves still work: " +
		"add one from the HTML Clay menu and its files open with no prompt at all."
	go func() {
		if err := a.notifyUser("HTML Clay", msg); err != nil {
			a.rt.logger.Printf("Could not show the missing-dialog notice: %v", err)
		}
	}()
}

// refreshFileTypes re-registers the file associations on every launch, for the
// reason refreshLoginItem re-registers the login item: a binary the user moved
// leaves a stored path pointing at nothing. Failure is logged and the launch
// carries on, since the launch running this is usually a double-click on a file
// and that file still has to open.
func (a *app) refreshFileTypes() {
	execPath, err := os.Executable()
	if err != nil || execPath == "" {
		return
	}
	if err := platform.RegisterFileTypes(execPath); err != nil {
		a.rt.logger.Printf("Could not register the .htmlclay file association: %v", err)
	}
}

func (a *app) refreshLoginItem() {
	if !a.rt.cfg.StartOnLoginEnabled() {
		return
	}
	execPath, err := os.Executable()
	if err != nil || execPath == "" {
		return
	}
	// Re-register on every launch so a moved or updated binary keeps a valid path.
	if err := platform.SetLoginItem(true, execPath); err != nil {
		a.rt.logger.Printf("Could not refresh login item: %v", err)
	}
}

// notifyUser sends a best-effort native message through the runtime seam, so a
// test asserting that the user was told never puts a real banner on their screen.
// Mirrors the confirm seam, for the same reason.
func (a *app) notifyUser(title, message string) error {
	if a.rt.notify != nil {
		return a.rt.notify(title, message)
	}
	return platform.Notify(title, message)
}
