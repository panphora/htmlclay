package server

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/panphora/htmlclay/internal/session"
)

func ValidateHost(r *http.Request, port int) bool {
	host := r.Host
	allowed1 := fmt.Sprintf("127.0.0.1:%d", port)
	allowed2 := fmt.Sprintf("localhost:%d", port)
	return host == allowed1 || host == allowed2
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

func HostValidationMiddleware(next http.Handler, port int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ValidateHost(r, port) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		// Defense in depth: reject cross-site requests so another website in the
		// user's browser cannot drive the save endpoint even if a token leaks.
		// Browsers send Sec-Fetch-Site: same-origin for the page's own fetches
		// and none for direct navigation; only cross-site is rejected.
		if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
