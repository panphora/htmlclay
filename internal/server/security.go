package server

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/panphora/htmlclay/internal/session"
)

// LoopbackHosts are the two Host values a server bound to one loopback port accepts.
// Listener servers (New) and the legacy per-port recovery listeners use them.
func LoopbackHosts(port int) []string {
	return []string{fmt.Sprintf("127.0.0.1:%d", port), fmt.Sprintf("localhost:%d", port)}
}

// ValidateHost reports whether r names a loopback server on port.
func ValidateHost(r *http.Request, port int) bool {
	return ValidateHostName(r, LoopbackHosts(port)...)
}

// ValidateHostName reports whether r.Host is exactly one of allowed. Exact, case-sensitive
// string equality: the app lowercases a browser's hostname before dispatch, and anything
// that is not one of our own spellings is a DNS-rebinding attempt or a stranger.
func ValidateHostName(r *http.Request, allowed ...string) bool {
	for _, h := range allowed {
		if h != "" && r.Host == h {
			return true
		}
	}
	return false
}

// slugPattern is an RFC 1123 hostname label restricted to lowercase: what the app issues.
var slugPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidSlug reports whether label has the shape of a site label the app could have issued.
func ValidSlug(label string) bool {
	return slugPattern.MatchString(label)
}

// IsSiteHost reports whether host is <label>.localhost:<port> for a well-formed label.
// It says nothing about whether that label was issued; the router answers that.
func IsSiteHost(host string, port int) bool {
	name, p, err := net.SplitHostPort(host)
	if err != nil || p != fmt.Sprint(port) {
		return false
	}
	label, ok := strings.CutSuffix(strings.ToLower(name), ".localhost")
	return ok && ValidSlug(label)
}

func ValidatePath(relPath string, homeDir string) (string, error) {
	if strings.HasPrefix(relPath, "/") || strings.Contains(relPath, "\x00") {
		return "", fmt.Errorf("invalid path: %q", relPath)
	}

	joined := filepath.Join(homeDir, relPath)
	cleaned := filepath.Clean(joined)

	canonical, ok := session.ContainWithinHome(homeDir, cleaned)
	if !ok {
		return "", fmt.Errorf("path escapes home directory: %q", relPath)
	}

	return canonical, nil
}

// sameOrigin admits a request only when the browser attests it was made by this
// site's own page: Sec-Fetch-Site must be exactly "same-origin", and Origin,
// WHEN PRESENT, must name this exact origin. It wraps every mutating /_/ route
// and both live-sync routes.
//
// "same-site" is rejected because on loopback it means nothing: localhost:3000
// reaches localhost:<this port> as same-site, so any local web tool's page could
// drive these routes through the user's own browser. An absent Sec-Fetch-Site is
// rejected because a page in an old browser without Sec-Fetch-* support sends no
// Origin on its no-cors requests either, and failing open there re-opens the
// same hole.
//
// Origin cannot be required outright: Chrome omits it on SAME-ORIGIN GETs —
// including EventSource's stream GET, the one GET route this gate wraps — so
// requiring it 403'd every live-sync stream while letting every POST through
// (POSTs always carry Origin). Verified live against Chrome 147. Both headers
// are browser-controlled: a page cannot forge them. A non-browser local process
// can, but such a process runs as the user and needs no confused deputy.
//
// The Host header was already validated upstream (HostValidationMiddleware), so
// comparing Origin against it binds the request to this site's own origin,
// port included.
func sameOrigin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Sec-Fetch-Site") != "same-origin" {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

// OriginOwner reports whether the app owns the origin a Referer names. For the app
// that is any site hostname or bare loopback spelling on its port, plus a loopback
// spelling on a legacy per-folder port it still holds. A listener server owns only
// its own two loopback hosts.
type OriginOwner func(host string) bool

func ownsHosts(hosts []string) OriginOwner {
	return func(h string) bool {
		for _, a := range hosts {
			if h == a {
				return true
			}
		}
		return false
	}
}

// IsNavigation reports a top-level document navigation: GET, Sec-Fetch-Mode navigate,
// Sec-Fetch-Dest document. Iframes, fetches and subresources are not navigations.
func IsNavigation(r *http.Request) bool {
	return r.Method == http.MethodGet &&
		r.Header.Get("Sec-Fetch-Mode") == "navigate" &&
		r.Header.Get("Sec-Fetch-Dest") == "document"
}

// RefererHost is the host of an http Referer, or "" when there is none.
func RefererHost(r *http.Request) string {
	ref, err := url.Parse(r.Header.Get("Referer"))
	if err != nil || ref.Scheme != "http" {
		return ""
	}
	return strings.ToLower(ref.Host)
}

// EligibleNavigation is the one navigation policy. It decides which top-level
// navigations a recovery address may redirect and which cross-site navigations a
// site may serve:
//
//	Sec-Fetch-Site none         a typed address or a bookmark: always
//	same-origin                 only when the user asked for it (Sec-Fetch-User ?1),
//	                            so a page cannot bounce itself through a redirect loop
//	same-site, cross-site       only when the Referer names an origin we own
//	absent                      never: not a browser
//
// Two site hostnames are cross-site in Safari, Firefox and Chrome alike (localhost is
// not on the Public Suffix List, so a.localhost and b.localhost are two registrable
// domains), and under the default referrer policy a cross-site navigation carries the
// source origin as Referer (measured 2026-10-12). Only a page this process served can
// carry a Referer on this port, because the port is held on both loopback stacks and a
// browser page cannot forge the header. A page that sets its own no-referrer policy
// makes its cross-folder links fail closed.
func EligibleNavigation(r *http.Request, ours OriginOwner) bool {
	if !IsNavigation(r) {
		return false
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "none":
		return true
	case "same-origin":
		return r.Header.Get("Sec-Fetch-User") == "?1"
	case "same-site", "cross-site":
		return ours != nil && ours(RefererHost(r))
	}
	return false
}

// HostValidationMiddleware is HostGate for a server on one loopback port.
func HostValidationMiddleware(next http.Handler, port int) http.Handler {
	hosts := LoopbackHosts(port)
	return HostGate(next, ownsHosts(hosts), hosts...)
}

// HostGate refuses any request whose Host is not one of allowed, and any cross-site
// request that is not an eligible navigation from an origin we own. Browsers send
// Sec-Fetch-Site: same-origin for the page's own fetches, same-site for a sibling
// loopback port, and none for direct navigation; only cross-site is gated.
func HostGate(next http.Handler, ours OriginOwner, allowed ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ValidateHostName(r, allowed...) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		if r.Header.Get("Sec-Fetch-Site") == "cross-site" && !EligibleNavigation(r, ours) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
