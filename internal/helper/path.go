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
