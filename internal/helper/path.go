package helper

import (
	"os"
	"strings"
	"sync"
)

var loginPathCache struct {
	sync.Mutex
	resolved bool
	value    string
}

func loginPath() string {
	loginPathCache.Lock()
	defer loginPathCache.Unlock()
	if !loginPathCache.resolved {
		loginPathCache.value = resolveLoginPath()
		loginPathCache.resolved = true
	}
	return loginPathCache.value
}

func refreshLoginPath() string {
	loginPathCache.Lock()
	defer loginPathCache.Unlock()
	loginPathCache.value = resolveLoginPath()
	loginPathCache.resolved = true
	return loginPathCache.value
}

func withPath(env []string, path string) []string {
	if path == "" {
		return env
	}
	if env == nil {
		env = os.Environ()
	}
	out := append([]string(nil), env...)
	for i, entry := range out {
		key, _, found := strings.Cut(entry, "=")
		if found && strings.EqualFold(key, "PATH") {
			out[i] = "PATH=" + path
			return out
		}
	}
	return append(out, "PATH="+path)
}

// LoginEnv is this process's environment with PATH replaced by the one a login
// shell reports, resolved once and cached. Programs installed through a shell
// profile (Homebrew, npm -g, ~/.local/bin) are only on that PATH when HTML Clay
// was started from the Dock.
func LoginEnv() []string {
	return withPath(os.Environ(), loginPath())
}

// RefreshLoginEnv resolves the login PATH again, for a lookup that failed
// against the cached one because something was installed since.
func RefreshLoginEnv() []string {
	return withPath(os.Environ(), refreshLoginPath())
}
