package server

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/panphora/htmlclay/session"
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
