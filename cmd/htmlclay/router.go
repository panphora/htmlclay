package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/panphora/htmlclay/internal/server"
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
	if s != nil && !s.closed.Load() {
		s.armOnce.Do(s.arm)
		if s.closed.Load() {
			// Closed between the lookup and the arm: the session manager already refused
			// the arm (ErrRevoked), and the retired answer is the only honest one.
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
		return
	}
	a.serveRetired(w, r, slug)
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
