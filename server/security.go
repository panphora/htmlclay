package server

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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

	if !strings.HasPrefix(cleaned, homeDir+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes home directory: %q", relPath)
	}

	return cleaned, nil
}

func HostValidationMiddleware(next http.Handler, port int) http.Handler {
	allowed1 := fmt.Sprintf("127.0.0.1:%d", port)
	allowed2 := fmt.Sprintf("localhost:%d", port)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != allowed1 && r.Host != allowed2 {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
