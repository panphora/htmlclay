package main

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	"github.com/panphora/htmlclay/logging"
	"github.com/panphora/htmlclay/server"
	"github.com/panphora/htmlclay/session"
	"github.com/panphora/htmlclay/trust"
)

// Invariants. All four are enforced in this file and nowhere else.
//
//  1. A file is registered in exactly one site. route() is the only funnel, so a
//     file gets exactly one token whichever door it arrived through.
//  2. A site's anchor is either a LIVE trusted folder or an ordinary directory,
//     never both.
//  3. A site holds at most one trusted read root, and it is the site's own
//     anchor. Under broadest-wins anchoring no OTHER trusted folder can contain
//     a site's anchor, because a broader one would have been the anchor. This is
//     what replaces the three copies of the EqualOrUnder seeding condition that
//     used to push roots between sites, and the bug family they carried: a
//     folder declared below an already-open site granted nothing, and one folder
//     could go live on two ports.
//  4. A live trusted folder has at most one site, so one project is one origin
//     on one port.

// site is one origin: one anchor folder, one loopback port, one session
// manager, one server. A trusted folder always has exactly one site. A file
// outside every trusted folder gets an ad-hoc site scoped to its own folder,
// which is what a double-clicked loose file has always got.
//
// Sites are held in a slice, not a map keyed by anchor: an anchor is not unique
// (files loose in the home directory never install a read root, so every one of
// them anchors at home) and a map write silently dropped the previous live site,
// orphaning its listener and letting one file be registered twice.
type site struct {
	anchor   string
	trusted  bool
	ln       net.Listener
	port     int
	sessions *session.Manager
	srv      *server.Server
	armOnce  sync.Once
}

func (s *site) start(logger *logging.Logger) {
	go func() {
		if err := s.srv.Start(); err != nil {
			logger.Printf("Server error on site %s: %v", s.anchor, err)
		}
	}()
}

// close releases everything the site holds, including the read-root capability
// handles. Same order as the graceful path in shutdown: the server goes first,
// so no request can still be reading through a handle when RevokeAll closes it.
//
// Revoking matters even though the discard paths that call this have registered
// nothing, because Windows refuses to remove a directory while a handle to it is
// open. A site that closed its listener but kept its os.Root would pin the served
// folder against rename and delete. POSIX unlink hides this on macOS and Linux.
func (s *site) close() {
	s.srv.Close()
	s.ln.Close()
	s.sessions.RevokeAll()
}

// armingListener opens the site's read root the first time anything connects to
// its port, rather than when the port is bound. os.OpenRoot pins a directory
// against rename and delete on Windows (see site.close), so with Start on Login
// and every trusted folder bound at launch, eager arming would pin every project
// folder from boot to quit and stop the user renaming their own folder.
//
// Wrapping Accept keeps the decision here, in the app that knows what the folder
// is, instead of adding a hook the server has to remember to call. It is keyed
// on the site, never on a requested path, so it can never become an oracle for
// whether some particular file exists.
type armingListener struct {
	net.Listener
	arm func()
}

func (l *armingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.arm()
	}
	return c, err
}

// anchorFor returns the folder that owns absPath's origin: the broadest live
// trusted folder containing it, else the file's own folder.
func (a *app) anchorFor(absPath string) (anchor string, trusted bool) {
	if folder, ok := a.trustedAnchor(absPath); ok {
		return folder, true
	}
	return filepath.Dir(absPath), false
}

// trustedAnchor returns the BROADEST live trusted folder containing absPath.
//
// Broadest, not most specific, so one project is one origin however deeply its
// files nest and however many folders inside it were declared separately.
// Untrusting the broad one makes the next call return the nested one, which is
// the nested folder's fallback with no bookkeeping anywhere.
//
// It reads the DECLARED list, never a site's installed roots. A site bound at
// startup has armed nothing yet, so its roots would answer no and route would
// build a second origin for a folder that already has one. Reading the config
// fact instead is what makes a folder declared below an already-open site take
// effect immediately.
//
// Ties (two declared folders with the same path length) break lexically so the
// choice is deterministic regardless of config order.
func (a *app) trustedAnchor(absPath string) (string, bool) {
	best := ""
	for _, tf := range a.rt.cfg.TrustedFolderList() {
		if !session.EqualOrUnder(absPath, tf.Path) {
			continue
		}
		if !trust.IdentityOK(tf.Path, tf.Identity) {
			continue
		}
		switch {
		case best == "",
			len(tf.Path) < len(best),
			len(tf.Path) == len(best) && tf.Path < best:
			best = tf.Path
		}
	}
	return best, best != ""
}

// trustedCovers is the server's auto-registration gate: does absPath sit inside
// a declared trusted folder?
//
// Deliberately lexical, with no filesystem access at all. shouldAutoRegister
// decides scope before it decides anything about what is on disk
// (server/trustrequest.go), and a stat here would turn that ordering into an
// existence oracle. It is only a gate on whether to TRY: the authorization is
// the trusted root install, and routeTrusted below refuses anything that does
// not anchor at a live trusted folder, so a folder whose identity pin no longer
// matches registers nothing.
//
// Anything asking whether a folder still GRANTS wants trustedLive instead.
func (a *app) trustedCovers(absPath string) bool {
	for _, tf := range a.rt.cfg.TrustedFolderList() {
		if session.EqualOrUnder(absPath, tf.Path) {
			return true
		}
	}
	return false
}

// trustedLive reports whether absPath sits inside a trusted folder whose
// identity pin still matches, which is the question "does this folder currently
// grant anything?". It stats, so it is only for callers holding a path they
// already know exists.
func (a *app) trustedLive(absPath string) bool {
	_, trusted := a.anchorFor(absPath)
	return trusted
}

// listenForAnchor reuses the port this origin was served on last time when it is
// still free, so the page's origin (and everything the browser scoped to it)
// survives a restart. When the remembered port is taken the site moves to a new
// one and route remembers that instead, which is the fallback David asked for.
func (a *app) listenForAnchor(anchor string) (net.Listener, error) {
	if p := a.rt.cfg.SitePort(anchor); p != 0 {
		if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p)); err == nil {
			return ln, nil
		}
		a.rt.logger.Printf("Remembered port %d for %s is taken, picking another", p, anchor)
	}
	return net.Listen("tcp", "127.0.0.1:0")
}

// buildSite binds a port and assembles a site without publishing or starting it,
// so a caller can still abandon it cleanly if registration fails.
func (a *app) buildSite(anchor string, trusted bool) (*site, error) {
	ln, err := a.listenForAnchor(anchor)
	if err != nil {
		return nil, err
	}
	sessions := session.NewManagerWithHome(a.rt.home)
	sessions.SetGuard(a.rt.guard)

	s := &site{
		anchor:   anchor,
		trusted:  trusted,
		port:     ln.Addr().(*net.TCPAddr).Port,
		sessions: sessions,
	}
	s.ln = &armingListener{Listener: ln, arm: func() {
		s.armOnce.Do(func() {
			// The parameter, deliberately, not s.trusted: this runs on an Accept
			// goroutine holding no lock, and s.trusted is written under a.mu by
			// adoptLocked. Nothing is lost by reading the value frozen at build
			// time, because adoption installs the root itself.
			if !trusted {
				return
			}
			if iErr := sessions.InstallTrustedRoot(anchor); iErr != nil {
				a.rt.logger.Printf("Could not install trusted root %s: %v", anchor, iErr)
			}
		})
	}}

	srv := server.NewWithLiveSync(s.ln, sessions, a.rt.logger, a.rt.versions, a.rt.ls)
	srv.SetInternalDir(a.rt.configDir)
	srv.SetSiteLabel(filepath.Base(anchor))
	srv.SetHooks(server.Hooks{
		Confirm:        a.rt.confirm,
		TrustedCovers:  a.trustedCovers,
		TrustedLive:    a.trustedLive,
		Route:          a.routeTrusted,
		MayTrustFolder: a.mayTrustFromPrompt,
		TrustFolder:    a.trustFromPrompt,
		TrustRequest:   a.trustFromPage,
	})
	s.srv = srv
	return s, nil
}

func (a *app) rememberPort(anchor string, port int) {
	// A file loose in the home directory anchors at home (installReadRoot
	// refuses home, so it can hold no root of its own), and every other loose
	// file anchors there too. Remembering that port would have each loose file
	// overwrite the last one's entry, and then bind a recovery listener at
	// startup on a port that belonged to whichever file was opened most
	// recently. Home is never an origin worth remembering.
	if anchor == a.rt.home {
		return
	}
	if a.rt.cfg.SitePort(anchor) == port {
		return
	}
	a.rt.cfg.RememberSitePort(anchor, port)
	if err := a.rt.cfg.Save(); err != nil {
		a.rt.logger.Printf("Could not persist port for %s: %v", anchor, err)
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

// siteAtLocked returns the live site anchored exactly at folder, or nil. This is
// invariant 4's lookup: one folder, one site. Caller holds a.mu.
func (a *app) siteAtLocked(folder string) *site {
	for _, s := range a.sites {
		if s.anchor == folder {
			return s
		}
	}
	return nil
}

// siteForLocked returns the live site that must host a new registration of
// absPath, or nil when route must build one. Caller holds a.mu.
//
// Two rules, with genuinely different inputs. Conflating them is what produced
// the inert-folder bug: rule 1 reads the declared config fact and never looks at
// any site's installed roots, because a site bound at startup has armed none yet.
func (a *app) siteForLocked(absPath string) *site {
	if folder, ok := a.trustedAnchor(absPath); ok {
		return a.siteAtLocked(folder)
	}
	return a.openedSiteForLocked(absPath)
}

// openedSiteForLocked is the ad-hoc rule: the most specific site whose
// EXPLICITLY OPENED folder already contains absPath, ties broken by port so
// selection is deterministic regardless of slice order. Caller holds a.mu.
//
// A read grant (and a trusted-folder root) widens READS but must never host a
// new registration: registering a file there would mint its save token on an
// origin another page already controls, turning read-only access into write
// access to a file opened later. So a file covered only by a grant returns nil
// here, and route anchors it on its own fresh origin rooted at its own folder.
func (a *app) openedSiteForLocked(absPath string) *site {
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

func (a *app) registerLocked(s *site, absPath string, via session.Provenance) (string, bool) {
	f, err := s.sessions.Register(absPath, via)
	if err != nil {
		a.rt.logger.Printf("Error registering file: %v", err)
		return "", false
	}
	return f.RelPath, true
}

// route resolves absPath to the site that should serve it, registering the file
// with the given provenance and creating a site if needed. It returns the site
// and the file's path relative to home. Every registration in the process goes
// through here (the OS open, the banner's trust request, trusted-folder
// auto-registration), so a file is only ever registered in one site and gets
// exactly one token, whichever door it arrived through.
func (a *app) route(absPath string, via session.Provenance) (*site, string, bool) {
	a.mu.Lock()
	if a.stopping {
		a.mu.Unlock()
		return nil, "", false
	}
	if s, rel, ok := a.lookupLocked(absPath); ok {
		// A second door onto a file this site already holds: a trusted-folder
		// file the user has now opened themselves, say. Re-register so the new
		// provenance accumulates onto the existing entry, which is what lets a
		// file survive revoking one of the ways it was reached. Register
		// returns the same token and the same path for a path it already
		// holds, so this only ever adds a flag.
		_, _ = a.registerLocked(s, absPath, via)
		a.mu.Unlock()
		a.rt.logger.Printf("File already open, re-launching window: %s", absPath)
		return s, rel, true
	}
	if s := a.siteForLocked(absPath); s != nil {
		rel, ok := a.registerLocked(s, absPath, via)
		a.mu.Unlock()
		return s, rel, ok
	}
	a.mu.Unlock()

	anchor, trusted := a.anchorFor(absPath)
	// Release the recovery listener holding this anchor's remembered port, or
	// binding below would find its own port taken and move the origin, breaking
	// the bookmark that startSites bound the port to preserve.
	a.unpark(anchor)

	// Bind outside the lock. It is the one call on this path that can block on a
	// stalled mount, and shutdown must never queue behind it.
	pending, err := a.buildSite(anchor, trusted)
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
		// Same accumulation as the early return above: a concurrent open
		// registered this file while we were binding, and this call's
		// provenance still has to land on it.
		_, _ = a.registerLocked(s, absPath, via)
		a.mu.Unlock()
		pending.close()
		return s, rel, true
	}
	if s := a.siteForLocked(absPath); s != nil {
		rel, ok := a.registerLocked(s, absPath, via)
		a.mu.Unlock()
		pending.close()
		return s, rel, ok
	}
	// Publish only once registration has succeeded, so a refused open never
	// leaves a server listening that nothing tracks or shuts down.
	rel, ok := a.registerLocked(pending, absPath, via)
	if !ok {
		a.mu.Unlock()
		pending.close()
		return nil, "", false
	}
	a.sites = append(a.sites, pending)
	pending.start(a.rt.logger)
	port := pending.port
	a.mu.Unlock()

	a.rt.logger.Printf("Site started for %s on 127.0.0.1:%d", anchor, port)
	a.rememberPort(anchor, port)
	return pending, rel, true
}

// routeTrusted is the server's auto-registration seam (Hooks.Route): route
// absPath and report where it serves, so the serving site can redirect when the
// registration landed on another origin.
//
// It refuses anything that does not anchor at a LIVE trusted folder. That is
// where the identity pin is enforced: trustedCovers is a cheap lexical gate that
// must not touch the filesystem, so this is the one place a folder replaced or
// swapped for a symlink since declaration stops granting. Refusing here costs
// nothing visible: the file falls back to serving read-only with the banner.
func (a *app) routeTrusted(absPath string) (string, bool) {
	if _, trusted := a.anchorFor(absPath); !trusted {
		return "", false
	}
	s, rel, ok := a.route(absPath, session.ViaTrusted)
	if !ok {
		return "", false
	}
	return fileURL(s.port, rel), true
}

// startSites binds every remembered port before argv is processed, so a URL
// bookmarked before the last quit answers on the first launch after it. What a
// bound port answers with is the whole of the feature:
//
//	live trusted folder      its own site: the file, editable, no dialog
//	nested under a broader   a recovery page, because the broader folder owns
//	trusted folder           the tree and the file lives on its origin now
//	remembered ad-hoc root   a recovery page. No roots armed, nothing registered
//	dead trusted folder      nothing at all. The URL stays dead and the tray says
//	                         why, because serving it would serve whatever now
//	                         sits at that path
func (a *app) startSites() {
	settled := map[string]bool{}
	for _, tf := range a.rt.cfg.TrustedFolderList() {
		if info, err := os.Stat(tf.Path); err != nil || !info.IsDir() {
			a.rt.logger.Printf("Trusted folder %s is missing; leaving its port unbound", tf.Path)
			settled[tf.Path] = true
			continue
		}
		if !trust.IdentityOK(tf.Path, tf.Identity) {
			a.rt.logger.Printf("Trusted folder %s failed its identity check; leaving its port unbound", tf.Path)
			settled[tf.Path] = true
			continue
		}
		// Invariant 4: only the broadest of a nested set gets a site. A shadowed
		// folder falls through to the parking loop, so a bookmark made before the
		// broader folder was declared still answers with a page.
		if anchor, ok := a.trustedAnchor(tf.Path); ok && anchor != tf.Path {
			continue
		}
		s, err := a.buildSite(tf.Path, true)
		if err != nil {
			a.rt.logger.Printf("Could not bind a port for trusted folder %s: %v", tf.Path, err)
			continue
		}
		a.mu.Lock()
		a.sites = append(a.sites, s)
		a.mu.Unlock()
		s.start(a.rt.logger)
		settled[tf.Path] = true
		a.rememberPort(tf.Path, s.port)
		a.rt.logger.Printf("Trusted folder %s listening on 127.0.0.1:%d", tf.Path, s.port)
	}

	for anchor, port := range a.rt.cfg.SitePortList() {
		if settled[anchor] {
			continue
		}
		a.parkPort(anchor, port)
	}
}

// shutdown stops every listener and releases every capability handle.
func (a *app) shutdown() {
	a.rt.logger.Printf("Shutting down...")

	// Mark stopping under the lock so a Finder open-file event arriving mid-quit
	// cannot create a site after the snapshot and outlive the process.
	a.mu.Lock()
	a.stopping = true
	sites := append([]*site(nil), a.sites...)
	parkedPorts := append([]*parked(nil), a.parked...)
	a.parked = nil
	a.mu.Unlock()

	for _, p := range parkedPorts {
		p.close()
	}

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
		// Close the listener directly, the way site.close does. Shutdown and Close
		// only release listeners Serve has registered, and start() runs Serve on a
		// goroutine, so a site built moments before quitting can reach here with its
		// socket still held by nothing the server knows about. It is then released
		// later, by Serve's own defer, which is after the port was supposed to be free.
		s.ln.Close()
		s.sessions.RevokeAll()
	}
}

func fileURL(port int, relPath string) string {
	base := fmt.Sprintf("http://127.0.0.1:%d/", port)
	// relPath comes from filepath.Rel, so on Windows its separators are
	// backslashes. url.JoinPath percent-encodes those as %5C rather than reading
	// them as separators, which collapses the whole path into ONE segment: a page
	// at /Documents%5CGitHub%5Cnotes%5Cx.htmlclay resolves a relative
	// "vendor/clay.js" against the server root, so every relative asset in a
	// .htmlclay file 404s. Only the generated URL was ever affected -- the server
	// already accepts forward slashes, because ValidatePath joins them onto the
	// home dir and Windows takes either separator.
	result, err := url.JoinPath(base, filepath.ToSlash(relPath))
	if err != nil {
		return base + filepath.ToSlash(relPath)
	}
	return result
}
