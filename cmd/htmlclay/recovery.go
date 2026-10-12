package main

import (
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/panphora/htmlclay/internal/server"
	"github.com/panphora/htmlclay/internal/session"
	"github.com/panphora/htmlclay/internal/trust"
)

// parked is a LEGACY per-folder port from before hostnames, bound with no capability at
// all: no session manager, no read roots, no versions store, no broker, and no
// file-serving code. It exists so a bookmark made under the old model answers with a
// redirect to the folder's hostname, or a page, instead of ERR_CONNECTION_REFUSED.
//
// It is a separate type from site precisely so that it is structurally
// incapable of serving a file, whatever a later change to the serve path does.
// A site with an empty session manager would still run the whole serve path: it
// would park out-of-scope requests and raise a native permission dialog for a
// bookmark that happened to land on a stale port, naming a folder the user was
// not looking at.
//
// Every path gets the same recovery bytes. The page never echoes the requested
// path and never touches the disk, so it can never be asked which of your files
// exist. An ordinary remembered port may instead answer an eligible document
// navigation with a redirect to the origin current trust gives the anchor: the
// anchor participates in that location lookup, but it never appears in the
// recovery body.
type parked struct {
	anchor string // for the location lookup and the log, never for the recovery body
	port   int
	kind   recoveryKind
	ln     net.Listener
	srv    *http.Server
}

func (p *parked) close() {
	p.srv.Close()
	p.ln.Close()
}

type recoveryKind uint8

const (
	recoveryOrdinary recoveryKind = iota
	recoveryDead
	recoveryRevoked
)

func (k recoveryKind) String() string {
	switch k {
	case recoveryDead:
		return "dead"
	case recoveryRevoked:
		return "revoked"
	}
	return "ordinary"
}

// parkPort binds port and answers it with the recovery page, or with a redirect
// to the origin current trust gives the anchor when an eligible navigation
// arrives. A port that is already taken is simply skipped: something else owns
// it now, and the origin will move to a fresh port the next time a file there is
// opened.
func (a *app) parkPort(anchor string, port int) {
	a.parkPortMode(anchor, port, recoveryPage, recoveryOrdinary)
}

// A dead source can relocate only after its own identity pin is valid again.
func (a *app) parkPortWith(anchor string, port int, page []byte) {
	a.parkPortMode(anchor, port, page, recoveryDead)
}

// ownsOrigin is the app's OriginOwner (server.EligibleNavigation): every origin this
// process answers on. That is every hostname on its port, plus each legacy per-folder
// port it still holds.
func (a *app) ownsOrigin(host string) bool {
	name, p, err := net.SplitHostPort(strings.ToLower(host))
	if err != nil {
		return false
	}
	port, _ := strconv.Atoi(p)
	if port == a.rt.port {
		if name == "localhost" || name == "127.0.0.1" || name == "::1" {
			return true
		}
		label, ok := strings.CutSuffix(name, ".localhost")
		return ok && server.ValidSlug(label)
	}
	if name != "localhost" && name != "127.0.0.1" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, parked := range a.parked {
		if parked.port == port {
			return true
		}
	}
	return false
}

func (a *app) parkPortMode(anchor string, port int, page []byte, kind recoveryKind) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		a.rt.logger.Printf("Legacy port %d for %s is taken; dropping it", port, anchor)
		return false
	}
	p := &parked{anchor: anchor, port: port, ln: ln, kind: kind}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		if kind != recoveryRevoked {
			if target, ok := a.recoveryTargetFor(anchor, r, kind == recoveryDead); ok {
				// Origin only, never stripped: the hop this redirect causes is cross-site
				// to the destination hostname, and the destination admits it only when the
				// Referer names an origin we own (server.EligibleNavigation).
				w.Header().Set("Referrer-Policy", "strict-origin")
				w.Header().Set("Location", target)
				w.WriteHeader(http.StatusFound)
				return
			}
		}
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		w.Write(page)
	})
	p.srv = &http.Server{
		Handler:           server.HostGate(handler, a.ownsOrigin, server.LoopbackHosts(port)...),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	a.mu.Lock()
	if a.stopping {
		a.mu.Unlock()
		p.close()
		return false
	}
	a.parked = append(a.parked, p)
	a.mu.Unlock()
	go func() {
		if err := p.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			a.rt.logger.Printf("Recovery listener error on %s: %v", anchor, err)
		}
	}()
	a.rt.logger.Printf("Holding legacy port %d for %s (%v); eligible navigations redirect to its current address", port, anchor, kind)
	return true
}

// Relocation registers nothing; the destination performs authorization and file lookup.
// The caller's gate or the router has already validated Host, and a retired address has
// no live site, so the same-port refusals that used to live here have nothing to refuse.
func (a *app) recoveryTargetFor(anchor string, r *http.Request, requireSource bool) (string, bool) {
	if !server.EligibleNavigation(r, a.ownsOrigin) {
		return "", false
	}
	rel := strings.TrimPrefix(r.URL.Path, "/")
	if strings.Contains(rel, "\\") || strings.HasPrefix(rel, "_/") || !strings.EqualFold(filepath.Ext(rel), ".htmlclay") {
		return "", false
	}
	for _, part := range strings.Split(rel, "/") {
		if part == ".." || part == "." {
			return "", false
		}
	}
	abs, err := server.ValidatePath(rel, a.rt.home)
	if err != nil || !session.EqualOrUnder(abs, anchor) || session.HasHiddenComponent(a.rt.home, abs) || session.EqualOrUnder(abs, a.rt.configDir) || a.rt.versions.Contains(abs) {
		return "", false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopping {
		return "", false
	}
	sourceFound := false
	for _, tf := range a.rt.cfg.TrustedFolderList() {
		if session.EqualOrUnder(anchor, tf.Path) && session.EqualOrUnder(tf.Path, anchor) {
			sourceFound = true
			if !trust.IdentityOK(tf.Path, tf.Identity, a.rt.home) {
				return "", false
			}
		}
	}
	if requireSource && !sourceFound {
		return "", false
	}
	targetAnchor, trusted := a.trustedAnchor(abs)
	if !trusted || !session.EqualOrUnder(abs, targetAnchor) {
		return "", false
	}
	s := a.siteAtLocked(targetAnchor)
	if s == nil || !s.trusted {
		return "", false
	}
	target := s.url(rel)
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	return target, true
}

// unpark releases the legacy listener holding anchor's port. It releases a listener
// only, and untrustFolder is its only caller: the folder has just lost its trust, so the
// listener is turned into a revoked one holding the same port. A revoked listener is
// never released.
func (a *app) unpark(anchor string) int {
	a.mu.Lock()
	var found *parked
	kept := a.parked[:0]
	for _, p := range a.parked {
		if p.anchor == anchor && p.kind != recoveryRevoked && found == nil {
			found = p
			continue
		}
		kept = append(kept, p)
	}
	a.parked = kept
	a.mu.Unlock()
	if found != nil {
		found.close()
		a.rt.logger.Printf("Released remembered port %d for %s", found.port, anchor)
		return found.port
	}
	return 0
}

// startLegacyRedirects binds every per-folder port the config still remembers from
// before hostnames, so an old bookmark answers with a redirect to the folder's new
// address. A port that is taken is dropped from the config: the bookmark it protected
// already answers with a stranger, and re-checking it every launch buys nothing. The
// app's own port can never be held this way, since adoptPort bound it first.
func (a *app) startLegacyRedirects() {
	changed := false
	for anchor, port := range a.rt.cfg.SitePortList() {
		if port == a.rt.port {
			a.rt.cfg.ForgetSitePort(anchor)
			changed = true
			continue
		}
		page, kind := recoveryPage, recoveryOrdinary
		if a.retiredIsDead(anchor) {
			page, kind = deadFolderPage, recoveryDead
		}
		if !a.parkPortMode(anchor, port, page, kind) {
			a.rt.cfg.ForgetSitePort(anchor)
			changed = true
		}
	}
	if changed {
		a.mu.Lock()
		a.saveConfig("dropping taken legacy ports")
		a.mu.Unlock()
	}
}

var recoveryPage = []byte(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>HTML Clay</title>
<style>
  :root { color-scheme: light dark; }
  body { margin:0; min-height:100vh; display:flex; align-items:center; justify-content:center;
         font:15px/1.55 -apple-system,system-ui,Segoe UI,sans-serif; background:#f6f6f7; color:#1c1c1e; }
  @media (prefers-color-scheme: dark) { body { background:#141416; color:#f2f2f7; } }
  main { max-width:29rem; padding:2rem; }
  h1 { font-size:1.15rem; margin:0 0 .75rem; }
  p { margin:0 0 .75rem; }
  ul { margin:0; padding-left:1.15rem; }
  li { margin-bottom:.4rem; }
</style>
</head>
<body>
<main>
  <h1>Nothing is open at this address</h1>
  <p>HTML Clay is running and holding this address for you, but no file is being served here right now.</p>
  <ul>
    <li>Open the file from Finder or your file manager to find its current address.</li>
    <li>For bookmarks that reopen without setup, trust the file's project folder in the HTML Clay menu. If its address is remembered and a trusted folder still covers it, HTML Clay can take you to its current address.</li>
  </ul>
</main>
</body>
</html>
`)

var deadFolderPage = []byte(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>HTML Clay</title>
<style>
  :root { color-scheme: light dark; }
  body { margin:0; min-height:100vh; display:flex; align-items:center; justify-content:center;
         font:15px/1.55 -apple-system,system-ui,Segoe UI,sans-serif; background:#f6f6f7; color:#1c1c1e; }
  @media (prefers-color-scheme: dark) { body { background:#141416; color:#f2f2f7; } }
  main { max-width:29rem; padding:2rem; }
  h1 { font-size:1.15rem; margin:0 0 .75rem; }
  p { margin:0 0 .75rem; }
  ul { margin:0; padding-left:1.15rem; }
  li { margin-bottom:.4rem; }
</style>
</head>
<body>
<main>
  <h1>This trusted folder needs approving again</h1>
  <p>HTML Clay can no longer confirm that the folder at this location is the one you trusted.
     It may have been moved, deleted, or replaced. Nothing is served at this address until you approve it again.</p>
  <ul>
    <li>If the folder is still there and you trust it, open the HTML Clay menu, choose
        <strong>Trusted Folders</strong> &rsaquo; <strong>Trust a Folder…</strong>, and select the same folder.</li>
    <li>Then reload this page. HTML Clay will use the folder's current address.</li>
  </ul>
</main>
</body>
</html>
`)
