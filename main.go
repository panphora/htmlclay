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
	"sort"
	"sync"
	"syscall"

	"github.com/panphora/htmlclay/browser"
	"github.com/panphora/htmlclay/config"
	"github.com/panphora/htmlclay/logging"
	"github.com/panphora/htmlclay/platform"
	"github.com/panphora/htmlclay/server"
	"github.com/panphora/htmlclay/session"
	"github.com/panphora/htmlclay/tray"
	"github.com/panphora/htmlclay/update"
	"github.com/panphora/htmlclay/versions"
)

var version = "1.2.0"

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

// appRuntime holds everything shared across every opened tree: config, logging,
// the versions store, the single-instance lock, and the one live-sync runtime.
// Each opened tree becomes its own site (below) on its own port, so the browser
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
	// confirm, when set, overrides the native permission dialog. Production leaves
	// it nil (the server uses the real dialog); tests inject a decision.
	confirm func(title, message string) (platform.ConfirmChoice, error)
	// notify, when set, overrides the native notification. Production leaves it nil
	// (a real banner); tests capture the message instead of putting one on the
	// user's screen. Unlike confirm, this is read live from background goroutines
	// rather than snapshotted at buildSite, so set it before opening any site.
	notify func(title, message string) error
	// modeOverride is a one-shot -app/-browser flag. It is kept out of cfg so a
	// later tray toggle that saves the config cannot accidentally persist it.
	modeOverride string
}

// site is one contiguous readable tree served on its own loopback port. A grant
// widens the site's reads (same port, same origin); an unrelated file opened
// elsewhere becomes a new site on a new port.
//
// Sites are held in a slice, not a map keyed by root: a root is not unique
// (files loose in the home directory never install a read root, so every one of
// them anchors at home) and a map write silently dropped the previous live site,
// orphaning its listener and letting one file be registered twice.
type site struct {
	root     string
	ln       net.Listener
	port     int
	sessions *session.Manager
	srv      *server.Server
}

func (s *site) start(logger *logging.Logger) {
	go func() {
		if err := s.srv.Start(); err != nil {
			logger.Printf("Server error on site %s: %v", s.root, err)
		}
	}()
}

func (s *site) close() {
	s.srv.Close()
	s.ln.Close()
}

type app struct {
	rt       *appRuntime
	mu       sync.Mutex
	sites    []*site
	stopping bool
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

	a := &app{rt: &appRuntime{}, noTray: *noTray}
	a.initConfig()
	defer a.rt.si.Unlock()

	a.initLogger()
	defer a.rt.logger.Close()

	a.startRuntime()

	if *appMode {
		a.rt.modeOverride = "app"
	} else if *browserMode {
		a.rt.modeOverride = "browser"
	}
	a.rt.logger.Printf("Launch mode: %s", a.mode())

	a.refreshLoginItem()

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

	cfg, err := config.Load()
	if err != nil {
		fatal("Error loading config: %v", err)
	}
	fmt.Fprintf(os.Stderr, "[htmlclay] Config loaded: mode=%s\n", cfg.Mode)
	a.rt.cfg = cfg
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
// port; sites bind their own lazily as files are opened.
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

	a.rt.ls = server.NewLiveSync(server.SeqPath(a.rt.versions), a.rt.logger)

	// A grant must not cover htmlclay's own config tree in either direction: not
	// a grant inside it, and not a grant of an ancestor that swallows it. The
	// versions dir lives under the config dir, so one check covers both. The
	// serve path denies the same tree structurally (Server.SetInternalDir); this
	// guard just stops the grant from being offered in the first place.
	forbidden := a.rt.configDir
	a.rt.guard = func(dir string) bool {
		return session.EqualOrUnder(dir, forbidden) || session.EqualOrUnder(forbidden, dir)
	}

	a.rt.logger.Printf("Runtime ready (home=%s)", a.rt.home)
}

func (a *app) mode() string {
	if a.rt.modeOverride != "" {
		return a.rt.modeOverride
	}
	return a.rt.cfg.CurrentMode()
}

// listenForRoot reuses the port this tree was served on last time when it is
// still free, so the page's origin (and everything the browser scoped to it)
// survives a restart.
func (a *app) listenForRoot(root string) (net.Listener, error) {
	if p := a.rt.cfg.SitePort(root); p != 0 {
		if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p)); err == nil {
			return ln, nil
		}
		a.rt.logger.Printf("Remembered port %d for %s is taken, picking another", p, root)
	}
	return net.Listen("tcp", "127.0.0.1:0")
}

// buildSite binds a port and assembles a site without publishing or starting it,
// so a caller can still abandon it cleanly if registration fails.
func (a *app) buildSite(root string) (*site, error) {
	ln, err := a.listenForRoot(root)
	if err != nil {
		return nil, err
	}
	sessions := session.NewManagerWithHome(a.rt.home)
	sessions.SetGuard(a.rt.guard)
	srv := server.NewWithLiveSync(ln, sessions, a.rt.logger, a.rt.versions, a.rt.ls)
	srv.SetInternalDir(a.rt.configDir)
	srv.SetSiteLabel(filepath.Base(root))
	if a.rt.confirm != nil {
		srv.SetConfirm(a.rt.confirm)
	}
	srv.SetTrustFolder(a.trustFromPrompt)
	return &site{
		root:     root,
		ln:       ln,
		port:     ln.Addr().(*net.TCPAddr).Port,
		sessions: sessions,
		srv:      srv,
	}, nil
}

func (a *app) rememberPort(root string, port int) {
	if a.rt.cfg.SitePort(root) == port {
		return
	}
	a.rt.cfg.RememberSitePort(root, port)
	if err := a.rt.cfg.Save(); err != nil {
		a.rt.logger.Printf("Could not persist port for %s: %v", root, err)
	}
}

// lookupLocked finds the site where absPath is already registered. Caller holds a.mu.
func (a *app) lookupLocked(absPath string) (*site, string, bool) {
	for _, s := range a.sites {
		if f, ok := s.sessions.LookupByPath(absPath); ok {
			return s, f.RelPath, true
		}
	}
	return nil, "", false
}

// siteForLocked returns the site that should host an explicitly-opened absPath, or
// nil if none already covers it through an EXPLICITLY OPENED folder. Caller holds a.mu.
//
// Only opened roots qualify. A read grant (and, later, a trusted-folder root) widens
// READS but must never host a new explicit open: registering a file there would mint
// its save token on an origin another page already controls, turning read-only access
// into write access to a file opened later. So a file covered only by a grant returns
// nil here, and route anchors it on its own fresh origin rooted at its own folder.
// Among opened roots the most specific wins, ties broken by port, so selection is
// deterministic regardless of map order. Siblings under one opened folder still share
// that site; only grant-only coverage is refused.
func (a *app) siteForLocked(absPath string) *site {
	var best *site
	var bestLen int
	for _, s := range a.sites {
		root, _, opened, ok := s.sessions.AssetRootOpened(absPath)
		if !ok || !opened {
			continue
		}
		switch {
		case best == nil,
			len(root) > bestLen,
			len(root) == bestLen && s.port < best.port:
			best, bestLen = s, len(root)
		}
	}
	return best
}

// seedTrustedRootsLocked pre-installs a silent read root for every trusted folder
// this site's anchor lives inside, so a page opened from within a trusted folder
// reads that folder's whole tree with no permission prompt. Only trusted folders
// that actually contain the anchor are seeded: opening a file in ~/Downloads never
// grants reads to an unrelated trusted ~/sites. Caller holds a.mu.
func (a *app) seedTrustedRootsLocked(sessions *session.Manager, anchor string) {
	for _, tf := range a.rt.cfg.TrustedFolderList() {
		if !session.EqualOrUnder(anchor, tf) {
			continue
		}
		if err := sessions.InstallTrustedRoot(tf); err != nil {
			a.rt.logger.Printf("Could not seed trusted root %s for %s: %v", tf, anchor, err)
		}
	}
}

// canonicalTrusted resolves and validates a folder the user asked to trust,
// returning the canonical path to store. It enforces the same rules a grant does:
// the folder must resolve, sit strictly inside home (so home itself is refused),
// carry no hidden component, and not be htmlclay's own config/versions tree. Storing
// the same canonical form InstallTrustedRoot keys on keeps live-revoke able to find
// the root later.
func (a *app) canonicalTrusted(dir string) (string, error) {
	resolved, err := resolveSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("cannot resolve folder: %w", err)
	}
	canonical, ok := session.ContainWithinHome(a.rt.home, resolved)
	if !ok {
		return "", fmt.Errorf("%s is outside your home folder", resolved)
	}
	if session.HasHiddenComponent(a.rt.home, canonical) {
		return "", fmt.Errorf("%s is a hidden folder", canonical)
	}
	if a.rt.guard(canonical) {
		return "", fmt.Errorf("%s is used by HTML Clay and can't be trusted", canonical)
	}
	return canonical, nil
}

// trustFolder marks dir as trusted: validates it, records it in config, and
// live-seeds it into every already-open site whose anchor sits inside it. It
// returns a human-readable error for the tray to surface, nil on success (or if
// dir was already trusted).
func (a *app) trustFolder(dir string) error {
	canonical, err := a.canonicalTrusted(dir)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.rt.cfg.AddTrustedFolder(canonical) {
		return nil
	}
	if err := a.rt.cfg.Save(); err != nil {
		a.rt.cfg.RemoveTrustedFolder(canonical)
		return fmt.Errorf("could not save config: %w", err)
	}
	for _, s := range a.sites {
		if session.EqualOrUnder(s.root, canonical) {
			if err := s.sessions.InstallTrustedRoot(canonical); err != nil {
				a.rt.logger.Printf("Could not live-seed trusted root %s into site %s: %v", canonical, s.root, err)
			}
		}
	}
	a.rt.logger.Printf("Trusted folder added: %s", canonical)
	return nil
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

// trustFromPrompt adds a folder to the trusted list on behalf of a permission
// dialog the user answered with "Trust this folder". It is deliberately stricter
// than the tray's own Add Trusted Folder flow: here the PAGE chooses the folder
// being offered, by picking which out-of-scope assets to request, so it can aim
// the common ancestor at one of the personal folders where files the user never
// wrote tend to land, and offer one-click trust for the whole tree. Trusting one
// of those from the tray is still allowed, because that takes a deliberate act
// with a folder picker.
//
// The caller has already installed the session read root, so refusing here costs
// only durability: the page keeps working now and asks again next launch. The
// user is told, because they asked for something permanent and got something
// temporary.
func (a *app) trustFromPrompt(dir string) error {
	if a.isProtectedHomeRoot(dir) {
		a.reportTrustRefused(dir, "A page picked this folder, and it is one of your main personal folders.")
		return fmt.Errorf("%s cannot be trusted from a permission prompt", dir)
	}
	if err := a.trustFolder(dir); err != nil {
		a.reportTrustRefused(dir, err.Error())
		return err
	}
	return nil
}

// isProtectedHomeRoot reports whether dir is one of the top-level personal folders
// in home, sits inside one, or is an ancestor that would swallow one.
//
// These are refused on the prompt route only. The PAGE chooses the folder named in
// that dialog by picking which out-of-scope assets to request, and it can inflate
// the choice: two requests under different subfolders make their common ancestor
// the whole of Documents. One click would then durably trust everything the user
// owns. A real project folder is essentially never one of these exactly, and a
// subfolder such as ~/Documents/projects/site is still trustable, so this costs
// the honest case nothing. Picking one of these from the tray still works, because
// that takes a deliberate act with a folder picker.
//
// dir arrives already symlink-resolved, so each name is compared in both its
// lexical and its resolved form. A Downloads or Documents folder that is itself a
// symlink, pointing at an external drive or a synced folder, is an ordinary setup,
// and it would otherwise reach here under its target's name and sail past a purely
// lexical match. Folders the user has renamed outright are still not recognized.
func (a *app) isProtectedHomeRoot(dir string) bool {
	for _, name := range []string{"Desktop", "Documents", "Downloads", "Library", "Movies", "Music", "Pictures", "Public"} {
		lexical := filepath.Join(a.rt.home, name)
		forms := []string{lexical}
		if resolved, err := filepath.EvalSymlinks(lexical); err == nil {
			if cleaned := filepath.Clean(resolved); cleaned != lexical {
				forms = append(forms, cleaned)
			}
		}
		for _, d := range forms {
			if session.EqualOrUnder(dir, d) || session.EqualOrUnder(d, dir) {
				return true
			}
		}
	}
	return false
}

// reportTrustRefused tells the user why a folder was not remembered. Staying
// silent is its own bug: they asked for something durable and got something that
// lasts until they quit, so without this the folder simply starts asking again
// with no explanation. It covers ordinary failures too, such as a config file
// that cannot be written, not only the protected-folder policy refusal.
//
// The notification is detached because this runs on the broker's prompt goroutine,
// which must not block: a notification that wedges (on Windows it is a modal that
// waits to be clicked) would leave the broker marked as prompting, and the site
// would never raise another permission dialog for the rest of its life.
func (a *app) reportTrustRefused(dir, reason string) {
	msg := dir + " was not added to your trusted folders.\n\n" + reason +
		"\n\nIt stays readable until you quit. You can add it yourself from the HTML Clay menu."
	go func() {
		if nErr := a.notifyUser("HTML Clay", msg); nErr != nil {
			a.rt.logger.Printf("Could not notify about the refused trust of %s: %v", dir, nErr)
		}
	}()
}

// untrustFolder removes dir from the trusted list and live-revokes the seeded
// trusted root from every open site. dir is the canonical path the tray shows (the
// value trustFolder stored), so it matches InstallTrustedRoot's key directly.
func (a *app) untrustFolder(dir string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.rt.cfg.RemoveTrustedFolder(dir) {
		return nil
	}
	if err := a.rt.cfg.Save(); err != nil {
		a.rt.cfg.AddTrustedFolder(dir)
		return fmt.Errorf("could not save config: %w", err)
	}
	for _, s := range a.sites {
		// One "Trust this folder" click sets both provenance flags on a single read
		// root, so clearing trust alone would leave the granted flag holding the root
		// open and the folder still readable. Removing a folder from the trusted list
		// takes back everything that click gave. A folder the user explicitly OPENED
		// survives, because that root is what the page is being served from.
		s.sessions.RevokeTrustedRoot(dir)
		s.sessions.RevokeReadRoot(dir)
	}
	a.rt.logger.Printf("Trusted folder removed: %s", dir)
	return nil
}

// trustedFolders returns a snapshot of the configured trusted folders for the tray
// to render. The list is guarded by the config's own lock, so this needs no a.mu.
func (a *app) trustedFolders() []string {
	return a.rt.cfg.TrustedFolderList()
}

// activeGrants returns the read roots that exist purely as runtime grants
// (granted, not opened, not trusted) across all live sites, deduped and sorted.
func (a *app) activeGrants() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.activeGrantsLocked()
}

// activeGrantsLocked is activeGrants without taking a.mu, so a mutation and the
// snapshot that reports its result can share one lock hold. Caller must hold a.mu.
func (a *app) activeGrantsLocked() []string {
	seen := map[string]bool{}
	for _, s := range a.sites {
		for _, r := range s.sessions.ReadRoots() {
			if r.Granted && !r.Opened && !r.Trusted {
				seen[r.Path] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// revokeGrant withdraws the grant at path from every live site, returning the
// updated grant list for the tray. The revoke and the snapshot share one lock hold
// so the returned list authoritatively reflects the revoke rather than a state that
// could have shifted in a gap between the two.
func (a *app) revokeGrant(path string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range a.sites {
		s.sessions.RevokeReadRoot(path)
	}
	return a.activeGrantsLocked()
}

// pickAndTrustFolder pops the native folder picker, trusts the chosen folder, and
// returns the updated trusted list for the tray. A cancelled picker or a rejected
// folder returns the current list unchanged (surfacing any error as a
// notification), so the tray always re-renders from truth.
func (a *app) pickAndTrustFolder() []string {
	dir, ok, err := platform.SelectFolder("Choose a folder to trust. HTML opened from inside it will serve and self-save with no permission prompts.")
	if err != nil {
		a.rt.logger.Printf("Folder picker failed: %v", err)
		return a.trustedFolders()
	}
	if !ok {
		return a.trustedFolders()
	}
	if err := a.trustFolder(dir); err != nil {
		a.rt.logger.Printf("Could not trust folder %s: %v", dir, err)
		go func() {
			if nErr := a.notifyUser("HTML Clay can't trust this folder", err.Error()); nErr != nil {
				a.rt.logger.Printf("Could not show notification: %v", nErr)
			}
		}()
	}
	return a.trustedFolders()
}

// removeTrustedFolder untrusts dir and returns the updated list for the tray.
func (a *app) removeTrustedFolder(dir string) []string {
	if err := a.untrustFolder(dir); err != nil {
		a.rt.logger.Printf("Could not untrust folder %s: %v", dir, err)
	}
	return a.trustedFolders()
}

func (a *app) registerLocked(s *site, absPath string) (string, bool) {
	f, err := s.sessions.Register(absPath)
	if err != nil {
		a.rt.logger.Printf("Error registering file: %v", err)
		return "", false
	}
	return f.RelPath, true
}

// route resolves absPath to the site that should serve it, registering the file
// and creating a site if needed. It returns the site and the file's path
// relative to home.
func (a *app) route(absPath string) (*site, string, bool) {
	a.mu.Lock()
	if a.stopping {
		a.mu.Unlock()
		return nil, "", false
	}
	if s, rel, ok := a.lookupLocked(absPath); ok {
		a.mu.Unlock()
		a.rt.logger.Printf("File already open, re-launching window: %s", absPath)
		return s, rel, true
	}
	if s := a.siteForLocked(absPath); s != nil {
		rel, ok := a.registerLocked(s, absPath)
		a.mu.Unlock()
		return s, rel, ok
	}
	a.mu.Unlock()

	// Bind outside the lock. It is the one call on this path that can block on a
	// stalled mount, and shutdown must never queue behind it.
	root := filepath.Dir(absPath)
	pending, err := a.buildSite(root)
	if err != nil {
		a.rt.logger.Printf("Error creating site for %s: %v", absPath, err)
		return nil, "", false
	}

	a.mu.Lock()
	// Another open may have produced a usable site while we were binding.
	if a.stopping {
		a.mu.Unlock()
		pending.close()
		return nil, "", false
	}
	if s, rel, ok := a.lookupLocked(absPath); ok {
		a.mu.Unlock()
		pending.close()
		return s, rel, true
	}
	if s := a.siteForLocked(absPath); s != nil {
		rel, ok := a.registerLocked(s, absPath)
		a.mu.Unlock()
		pending.close()
		return s, rel, ok
	}
	// Publish only once registration has succeeded, so a refused open never
	// leaves a server listening that nothing tracks or shuts down.
	rel, ok := a.registerLocked(pending, absPath)
	if !ok {
		a.mu.Unlock()
		pending.close()
		return nil, "", false
	}
	a.seedTrustedRootsLocked(pending.sessions, root)
	a.sites = append(a.sites, pending)
	pending.start(a.rt.logger)
	port := pending.port
	a.mu.Unlock()

	a.rt.logger.Printf("Site started for %s on 127.0.0.1:%d", root, port)
	a.rememberPort(root, port)
	return pending, rel, true
}

func (a *app) openFile(filePath string) {
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		a.rt.logger.Printf("Error resolving path: %v", err)
		return
	}

	if _, err := os.Stat(absPath); os.IsNotExist(err) {
		a.rt.logger.Printf("File not found: %s", absPath)
		return
	}

	absPath, err = resolveSymlinks(absPath)
	if err != nil {
		a.rt.logger.Printf("Error resolving symlinks: %v", err)
		return
	}

	// Refuse an out-of-home file before any site exists. Registration would
	// reject it anyway, but by then a port was bound and a server started.
	if _, ok := session.ContainWithinHome(a.rt.home, absPath); !ok {
		a.rt.logger.Printf("Refusing file outside home: %s", absPath)
		msg := fmt.Sprintf("%s is outside your home folder. HTML Clay only opens files inside %s.",
			filepath.Base(absPath), a.rt.home)
		go func() {
			if nErr := a.notifyUser("HTML Clay can't open this file", msg); nErr != nil {
				a.rt.logger.Printf("Could not show notification: %v", nErr)
			}
		}()
		return
	}

	s, rel, ok := a.route(absPath)
	if !ok {
		return
	}

	target := fileURL(s.port, rel)
	a.rt.logger.Printf("Serving %s at %s", filepath.Base(absPath), target)
	a.launchBrowser(target)
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
		a.rt.logger.Printf("Error resolving home dir: %v", err)
		return
	}
	path := filepath.Join(home, "htmlclay", "examples", "welcome.htmlclay")
	if err := ensureExampleFile(path); err != nil {
		a.rt.logger.Printf("Error creating example file: %v", err)
		return
	}
	a.openFile(path)
}

// openBackups opens the versions folder in the platform file manager. This is the
// discovery mechanism that makes the plain-folder backup design usable: a version
// can be double-clicked straight from Finder.
func (a *app) openBackups() {
	dir, err := a.rt.versions.Dir()
	if err != nil {
		a.rt.logger.Printf("Error resolving backups dir: %v", err)
		return
	}
	if err := browser.OpenURL(dir); err != nil {
		a.rt.logger.Printf("Error opening backups folder: %v", err)
	}
}

func (a *app) run(updateCh <-chan tray.UpdateInfo) {
	if a.noTray {
		a.rt.logger.Printf("Running without tray, waiting for signal...")
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		<-sigCh
		a.shutdown()
	} else {
		a.rt.logger.Printf("Starting system tray...")
		tray.Run(a.rt.cfg, a.openExample, a.openBackups, func() {
			a.shutdown()
		}, updateCh, &tray.TrustedFolderHooks{
			List:   a.trustedFolders,
			Add:    a.pickAndTrustFolder,
			Remove: a.removeTrustedFolder,
		}, &tray.GrantHooks{List: a.activeGrants, Revoke: a.revokeGrant})
		a.rt.logger.Printf("Tray exited")
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

func (a *app) shutdown() {
	a.rt.logger.Printf("Shutting down...")

	// Mark stopping under the lock so a Finder open-file event arriving mid-quit
	// cannot create a site after the snapshot and outlive the process.
	a.mu.Lock()
	a.stopping = true
	sites := append([]*site(nil), a.sites...)
	a.mu.Unlock()

	// Close the shared live-sync streams once, before the per-site HTTP servers,
	// so graceful shutdown does not wait on open SSE connections.
	a.rt.ls.Shutdown()

	// Each site gets its own budget; one slow site must not spend the whole
	// allowance and leave the rest to be force-closed. Drain the HTTP server
	// first (which releases parked permission requests), THEN revoke: RevokeAll
	// closes the read-root capability handles, so it must not run while a request
	// could still be reading through one.
	for _, s := range sites {
		ctx, cancel := context.WithTimeout(context.Background(), server.ShutdownBudget)
		if err := s.srv.Shutdown(ctx); err != nil {
			a.rt.logger.Printf("Graceful shutdown timed out (%v), forcing close", err)
			s.srv.Close()
		}
		cancel()
		s.sessions.RevokeAll()
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
	if a.mode() == "app" {
		if a.tryAppMode(targetURL) {
			return
		}
	}
	a.rt.logger.Printf("Opening in default browser: %s", targetURL)
	if err := browser.OpenURL(targetURL); err != nil {
		a.rt.logger.Printf("Error opening browser: %v", err)
	}
}

func (a *app) tryAppMode(targetURL string) bool {
	chromePath := browser.FindChromium()
	if chromePath == "" {
		a.rt.logger.Printf("No Chromium found, falling back to default browser")
		return false
	}
	a.rt.logger.Printf("Launching Chrome App Mode: %s", chromePath)
	configDir, err := config.Dir()
	if err != nil {
		a.rt.logger.Printf("Error resolving config dir: %v, falling back to browser", err)
		return false
	}
	profileDir := filepath.Join(configDir, "chrome-profile")
	if _, err := browser.LaunchAppMode(chromePath, targetURL, profileDir); err != nil {
		a.rt.logger.Printf("App Mode failed: %v, falling back to browser", err)
		return false
	}
	return true
}
