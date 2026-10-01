package aiedit

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// lookPath finds a program the way the child would: against the PATH inside its
// own environment, not this process's.
func lookPath(name string, env []string) (string, error) {
	if name == "" {
		return "", exec.ErrNotFound
	}
	if filepath.IsAbs(name) || strings.ContainsRune(name, filepath.Separator) || strings.Contains(name, "/") {
		if isExecutable(name) {
			return name, nil
		}
		return "", exec.ErrNotFound
	}
	extensions := []string{""}
	if runtime.GOOS == "windows" {
		extensions = pathExtensions(env)
	}
	for _, dir := range filepath.SplitList(envValue(env, "PATH")) {
		if dir == "" {
			continue
		}
		for _, extension := range extensions {
			candidate := filepath.Join(dir, name+extension)
			if isExecutable(candidate) {
				return candidate, nil
			}
		}
	}
	return "", exec.ErrNotFound
}

func pathExtensions(env []string) []string {
	raw := envValue(env, "PATHEXT")
	if raw == "" {
		raw = ".COM;.EXE;.BAT;.CMD"
	}
	extensions := []string{""}
	for _, ext := range strings.Split(raw, ";") {
		if ext != "" {
			extensions = append(extensions, ext)
		}
	}
	return extensions
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode().Perm()&0111 != 0
}

func envValue(env []string, key string) string {
	for _, entry := range env {
		name, value, found := strings.Cut(entry, "=")
		if found && strings.EqualFold(name, key) {
			return value
		}
	}
	return ""
}
