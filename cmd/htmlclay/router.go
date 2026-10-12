package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/panphora/htmlclay/internal/server"
	"github.com/panphora/htmlclay/internal/session"
	"github.com/panphora/htmlclay/internal/trust"
)

// serveHost is the one handler on the one port. It routes by the Host header and nothing
// else: an issued slug reaches its site, a retired slug reaches the recovery flow, the
// bare loopback names reach a fixed page, and every other name is refused, which is the
// DNS-rebinding defense the per-port servers had. It never takes a.mu.
func (a *app) serveHost(w http.ResponseWriter, r *http.Request) {
	name, port, err := net.SplitHostPort(r.Host)
	if err != nil || port != strconv.Itoa(a.rt.port) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" && !server.EligibleNavigation(r, a.ownsOrigin) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	name = strings.ToLower(name)
	if name == "localhost" || name == "127.0.0.1" || name == "::1" {
		if apiPath, ok := scriptAPIPath(r); ok {
			if s := a.siteServingPath(apiPath); s != nil {
				a.dispatch(w, r, s, "")
				return
			}
			writeNoFolder(w)
			return
		}
		recoveryHeaders(w)
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write(indexPage)
		return
	}
	slug, ok := strings.CutSuffix(name, ".localhost")
	if !ok || !server.ValidSlug(slug) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	a.hostMu.RLock()
	s := a.bySlug[slug]
	a.hostMu.RUnlock()
	if s == nil {
		a.serveRetired(w, r, slug)
		return
	}
	a.dispatch(w, r, s, slug)
}

// dispatch hands r to a live site: it arms the site on its first request, ties the
// request's context to the site's so closing the site ends it, and rewrites Host to the
// site's own so its gate and same-origin checks see the hostname the site answers. slug is
// the hostname label the request named, used for the retired answer when the site closed
// between the lookup and the arm; empty when the request reached the site through the bare
// host, which then gets the no-folder answer instead.
func (a *app) dispatch(w http.ResponseWriter, r *http.Request, s *site, slug string) {
	if a.rt.afterLookup != nil {
		a.rt.afterLookup(s)
	}
	if !s.closed.Load() {
		s.armOnce.Do(s.arm)
	}
	if s.closed.Load() {
		// Closed between the lookup and the arm, or before it: the session manager refuses
		// the arm (ErrRevoked), and the retired answer is the only honest one.
		if slug == "" {
			writeNoFolder(w)
			return
		}
		a.serveRetired(w, r, slug)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	r = r.WithContext(ctx)
	r.Host = s.host
	s.srv.ServeHTTP(w, r)
}

// scriptAPIPath reports the home-relative file path of a data API request from a program
// rather than a browser: a /_/api/<path> request with neither Origin nor Sec-Fetch-Site,
// the classifier the wire uses. Only those reach a site through the bare host, so a
// script needs no folder hostname, and a browser page still cannot drive the data API of
// an origin it is not on.
func scriptAPIPath(r *http.Request) (string, bool) {
	if r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != "" {
		return "", false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		return "", false
	}
	rel, ok := strings.CutPrefix(r.URL.Path, "/_/api/")
	if !ok || strings.Trim(rel, "/") == "" {
		return "", false
	}
	return rel, true
}

// siteServingPath picks the live site that serves the home-relative path rel: the site
// keyed by the file itself (a file loose in home), else the live site with the longest
// anchor containing the file. It is lexical and takes only hostMu, like the rest of the
// router, so a stalled disk cannot block it.
func (a *app) siteServingPath(rel string) *site {
	abs := filepath.Join(a.rt.home, filepath.FromSlash(rel))
	if !session.EqualOrUnder(abs, a.rt.home) {
		return nil
	}
	a.hostMu.RLock()
	defer a.hostMu.RUnlock()
	var best *site
	for _, s := range a.bySlug {
		if s.closed.Load() {
			continue
		}
		if s.key == abs {
			return s
		}
		if s.anchor == a.rt.home || !session.EqualOrUnder(abs, s.anchor) {
			continue
		}
		if best == nil || len(s.anchor) > len(best.anchor) {
			best = s
		}
	}
	return best
}

// writeNoFolder answers a bare-host data API request no live site serves.
func writeNoFolder(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNotFound)
	w.Write([]byte(`{"error":"No open folder","message":"No folder HTML Clay is serving right now contains this file. Open the file in HTML Clay first, or send the request to its folder's address, which htmlclay wire where <file> prints."}` + "\n"))
}

func recoveryHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
}

// serveRetired answers a well-formed slug with no live site. It serves no file and
// registers nothing; it either sends an eligible navigation to the site that owns the
// file now, or explains itself with a fixed page that never echoes the request. A slug
// on the retired list has no key and always gets the recovery page.
func (a *app) serveRetired(w http.ResponseWriter, r *http.Request, slug string) {
	recoveryHeaders(w)
	page := recoveryPage
	if key, known := a.rt.cfg.SiteKeyForSlug(slug); known {
		dead := a.retiredIsDead(key)
		if target, ok := a.recoveryTargetFor(key, r, dead); ok {
			w.Header().Set("Referrer-Policy", "strict-origin")
			w.Header().Set("Location", target)
			w.WriteHeader(http.StatusFound)
			return
		}
		if dead {
			page = deadFolderPage
		}
	}
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	w.Write(page)
}

// retiredIsDead reports whether key is a trusted folder that is missing or whose identity
// pin no longer matches, which is startSites's dead-folder test applied at request time.
func (a *app) retiredIsDead(key string) bool {
	for _, tf := range a.rt.cfg.TrustedFolderList() {
		if tf.Path != key {
			continue
		}
		if info, err := os.Stat(tf.Path); err != nil || !info.IsDir() {
			return true
		}
		return !trust.IdentityOK(tf.Path, tf.Identity, a.rt.home)
	}
	return false
}

// publishLocked makes s live: appended to sites and visible to the router. Caller holds
// a.mu.
func (a *app) publishLocked(s *site) {
	a.sites = append(a.sites, s)
	a.hostMu.Lock()
	if a.bySlug == nil {
		a.bySlug = make(map[string]*site)
	}
	a.bySlug[s.slug] = s
	a.hostMu.Unlock()
}

var indexPage = []byte(`<!doctype html>
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
</style>
</head>
<body>
<main>
  <h1>HTML Clay is running</h1>
  <p>Each folder you open has its own address ending in <code>.localhost</code>. Nothing is served at this one.</p>
  <p>To open a file, double-click it in Finder or your file manager, or use the HTML Clay menu.</p>
</main>
</body>
</html>
`)
