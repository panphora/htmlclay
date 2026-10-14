package main

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/panphora/htmlclay/internal/server"
	"github.com/panphora/htmlclay/internal/session"
	"github.com/panphora/htmlclay/internal/trust"
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
//     at one hostname on the app's port.

// site is one origin: one anchor folder, one hostname on the app's port, one session
// manager, one hosted server. A trusted folder always has exactly one site. A file
// outside every trusted folder gets an ad-hoc site scoped to its own folder.
//
// Sites are held in a slice, not a map keyed by anchor: an anchor is not unique (files
// loose in the home directory never install a read root, so every one of them anchors at
// home). The slug is unique, and bySlug is what the router looks sites up by.
type site struct {
	anchor string
	// key is what the slug is remembered under: the anchor, or the file itself for a file
	// loose in home, where the anchor is shared.
	key       string
	slug      string
	freshSlug bool // the slug was allocated by this build and not yet published
	host      string
	trusted   bool
	sessions  *session.Manager
	srv       *server.Server
	// arm opens the site's read root the first time a request is routed to it, rather than
	// when the site is built. os.OpenRoot pins a directory against rename and delete on
	// Windows (see close), so with Start on Login and every trusted folder built at launch,
	// eager arming would pin every project folder from boot to quit. Keyed on the site,
	// never on a requested path, so it can never become an oracle for whether some
	// particular file exists.
	arm     func()
	armOnce sync.Once
	// closed is set by close before anything is torn down. The router never arms or
	// dispatches a closed site, and the session manager refuses every capability after
	// RevokeAll, so a request that looked the site up a moment before close cannot bring
	// it back. ctx is cancelled by close so in-flight streams end.
	closed atomic.Bool
	ctx    context.Context
	cancel context.CancelFunc
}

func (s *site) url(rel string) string { return fileURL(s.host, rel) }

// close releases everything the site holds, including the read-root capability
// handles. closed first, so the router stops dispatching; then the context, so open
// streams return; then the server (broker, wire, AI edits); then RevokeAll, which
// closes the handles and marks the manager so nothing can be granted again. Revoking
// matters even on discard paths that registered nothing, because Windows refuses to
// remove a directory while a handle to it is open.
func (s *site) close() {
	s.closed.Store(true)
	s.cancel()
	s.srv.Close()
	s.sessions.RevokeAll()
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
		if !trust.IdentityOK(tf.Path, tf.Identity, a.rt.home) {
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
// (internal/server/trustrequest.go), and a stat here would turn that ordering into an
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

// buildSite assembles a site without publishing it, so a caller can still abandon it
// cleanly if registration fails.
// The slug is allocated here, in memory; the caller saves the config when it publishes.
func (a *app) buildSite(anchor, key string, trusted bool) *site {
	a.mu.Lock()
	fresh := a.rt.cfg.SiteSlug(key) == ""
	slug := a.slugForLocked(key)
	a.mu.Unlock()
	sessions := session.NewManagerWithHome(a.rt.home)
	sessions.SetGuard(a.rt.guard)

	s := &site{
		anchor:    anchor,
		key:       key,
		slug:      slug,
		freshSlug: fresh,
		host:      siteHost(slug, a.rt.port),
		trusted:   trusted,
		sessions:  sessions,
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.arm = func() {
		// The parameter, deliberately, not s.trusted: this runs on a request goroutine
		// holding no lock, and s.trusted is written under a.mu by adoptLocked. Nothing is
		// lost by reading the value frozen at build time, because adoption installs the
		// root itself. A closed site never arms, and a revoked manager refuses anyway.
		if !trusted || s.closed.Load() {
			return
		}
		if iErr := sessions.InstallTrustedRoot(anchor); iErr != nil && !errors.Is(iErr, session.ErrRevoked) {
			a.rt.logger.Printf("Could not install trusted root %s: %v", anchor, iErr)
		}
	}

	srv := server.NewHosted(s.host, a.ownsOrigin, sessions, a.rt.logger, a.rt.versions, a.rt.ls)
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
		AIEditEnabled:  a.rt.cfg.AIEditEnabled,
		AIEditEngines:  a.rt.cfg.AIEditEngines,
		Profile:        a.rt.cfg.SharedPerson,
	})
	s.srv = srv
	return s
}

// originKey names what a file's slug is remembered under. A file loose in home anchors at
// home like every other such file, so its own path is the key.
func (a *app) originKey(absPath, anchor string) string {
	if anchor == a.rt.home {
		return absPath
	}
	return anchor
}

// discardPendingLocked closes a site that was built and never published, and drops its
// slug if this build allocated it, so a refused open records nothing. Caller holds a.mu.
func (a *app) discardPendingLocked(s *site) {
	if s.freshSlug {
		a.rt.cfg.DropSiteSlug(s.key)
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
			len(root) == bestLen && s.slug < best.slug:
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
	key := a.originKey(absPath, anchor)
	pending := a.buildSite(anchor, key, trusted)

	a.mu.Lock()
	// Another open may have produced a usable site while we were building.
	if a.stopping {
		a.discardPendingLocked(pending)
		a.mu.Unlock()
		pending.close()
		return nil, "", false
	}
	if s, rel, ok := a.lookupLocked(absPath); ok {
		// Same accumulation as the early return above: a concurrent open
		// registered this file while we were building, and this call's
		// provenance still has to land on it.
		_, _ = a.registerLocked(s, absPath, via)
		a.discardPendingLocked(pending)
		a.mu.Unlock()
		pending.close()
		return s, rel, true
	}
	if s := a.siteForLocked(absPath); s != nil {
		rel, ok := a.registerLocked(s, absPath, via)
		a.discardPendingLocked(pending)
		a.mu.Unlock()
		pending.close()
		return s, rel, ok
	}
	// Publish only once registration has succeeded, so a refused open never leaves a
	// site that nothing tracks. The slug is saved in the same critical section that
	// publishes the site, under the lock shutdown takes to set stopping, so a config
	// write can never land after shutdown has returned.
	rel, ok := a.registerLocked(pending, absPath, via)
	if !ok {
		a.discardPendingLocked(pending)
		a.mu.Unlock()
		pending.close()
		return nil, "", false
	}
	a.publishLocked(pending)
	a.saveConfig("publishing " + pending.slug)
	a.mu.Unlock()

	a.rt.logger.Printf("Site started for %s at http://%s", anchor, pending.host)
	return pending, rel, true
}

// routeTrusted is the server's auto-registration seam (Hooks.Route). It routes
// the file and attaches its helper dispatcher through serveURL, which is the
// only reason a trusted-folder document can use a helper it already has
// permission for. Original contract below: route
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
	return a.serveURL(absPath)
}

// startSites builds every live trusted folder's site before argv is processed, so a URL
// bookmarked before the last quit answers on the first launch after it. What a hostname
// answers with is the whole of the feature:
//
//	live trusted folder      its own site: the file, editable, no dialog
//	nested under a broader   a redirect to the broader folder's hostname for an
//	trusted folder           eligible navigation, the recovery page otherwise
//	remembered ad-hoc key    a redirect to the hostname current trust gives the
//	                         requested file, the recovery page otherwise; nothing armed
//	dead trusted folder      the dead-folder recovery page, never a site
//	retired slug             the recovery page, always
//
// Only the first row needs a site. The router (serveRetired) answers the rest from the
// config, so nothing is bound for them. Legacy per-folder ports are bound afterwards by
// startLegacyRedirects.
func (a *app) startSites() {
	for _, tf := range a.rt.cfg.TrustedFolderList() {
		if info, err := os.Stat(tf.Path); err != nil || !info.IsDir() {
			a.rt.logger.Printf("Trusted folder %s is missing; its address answers with a recovery page", tf.Path)
			continue
		}
		if !trust.IdentityOK(tf.Path, tf.Identity, a.rt.home) {
			a.rt.logger.Printf("Trusted folder %s failed its identity check; its address answers with a recovery page", tf.Path)
			continue
		}
		if anchor, ok := a.trustedAnchor(tf.Path); ok && anchor != tf.Path {
			a.rt.logger.Printf("Trusted folder %s is covered by %s; its remembered origin uses navigation recovery", tf.Path, anchor)
			continue
		}
		s := a.buildSite(tf.Path, tf.Path, true)
		a.mu.Lock()
		a.publishLocked(s)
		a.saveConfig("startup for " + s.slug)
		a.mu.Unlock()
		a.rt.logger.Printf("Trusted folder %s at http://%s", tf.Path, s.host)
	}
	a.startLegacyRedirects()
}

// shutdown stops the server, every site and every legacy listener, and releases every
// capability handle.
func (a *app) shutdown() {
	a.rt.logger.Printf("Shutting down...")

	a.mu.Lock()
	a.stopping = true
	sites := append([]*site(nil), a.sites...)
	parkedPorts := append([]*parked(nil), a.parked...)
	a.parked = nil
	a.mu.Unlock()

	for _, p := range parkedPorts {
		p.close()
	}

	// Close the shared live-sync streams once, before the HTTP drain, so graceful
	// shutdown does not wait on open SSE connections.
	a.rt.ls.Shutdown()

	// Release each site's parked permission requests, wire streams and AI edits before
	// draining, or the drain waits on them until its budget and then force-closes.
	for _, s := range sites {
		s.closed.Store(true)
		s.cancel()
		s.srv.Shutdown(context.Background())
	}
	if a.rt.httpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), server.ShutdownBudget)
		if err := a.rt.httpServer.Shutdown(ctx); err != nil {
			a.rt.logger.Printf("Graceful shutdown timed out (%v), forcing close", err)
			a.rt.httpServer.Close()
		}
		cancel()
	}
	// RevokeAll closes the read-root capability handles, so it runs only once no request
	// can still be reading through one.
	for _, s := range sites {
		s.sessions.RevokeAll()
	}
	a.rt.bound.close()
}

func fileURL(host, relPath string) string {
	// relPath is a decoded filesystem-relative path, so assigning it to Path
	// escapes a literal percent exactly once. ToSlash is still needed on Windows,
	// where filepath.Rel hands back backslashes: Path would encode those as %5C
	// and every relative asset in the page would resolve against the server root.
	return (&url.URL{
		Scheme: "http",
		Host:   host,
		Path:   "/" + filepath.ToSlash(relPath),
	}).String()
}
