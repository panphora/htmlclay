package browser

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

var macOSChromePaths = []string{
	"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
	"/Applications/Chromium.app/Contents/MacOS/Chromium",
	"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
}

var windowsChromePaths = []string{
	filepath.Join(os.Getenv("PROGRAMFILES"), "Google", "Chrome", "Application", "chrome.exe"),
	filepath.Join(os.Getenv("PROGRAMFILES(X86)"), "Google", "Chrome", "Application", "chrome.exe"),
	filepath.Join(os.Getenv("LOCALAPPDATA"), "Google", "Chrome", "Application", "chrome.exe"),
	filepath.Join(os.Getenv("PROGRAMFILES"), "Microsoft", "Edge", "Application", "msedge.exe"),
	filepath.Join(os.Getenv("PROGRAMFILES(X86)"), "Microsoft", "Edge", "Application", "msedge.exe"),
}

var linuxChromePaths = []string{
	"/snap/bin/chromium",
	"/snap/bin/google-chrome",
}

var chromeCLINames = []string{
	"google-chrome",
	"google-chrome-stable",
	"chromium",
	"chromium-browser",
	"microsoft-edge",
}

func FindChromium() string {
	finders := []func() string{
		findFromEnv,
		findFromBrowserEnv,
		findFromPath,
	}
	if runtime.GOOS == "darwin" {
		finders = append(finders, findFromMacOSPaths)
	}
	if runtime.GOOS == "windows" {
		finders = append(finders, findFromWindowsPaths)
	}
	if runtime.GOOS == "linux" {
		finders = append(finders, findFromLinuxPaths)
	}
	for _, fn := range finders {
		if path := fn(); path != "" {
			return path
		}
	}
	return ""
}

func findFromEnv() string {
	env := os.Getenv("HTMLCLAY_BROWSER")
	if env == "" {
		return ""
	}
	if _, err := os.Stat(env); err == nil {
		return env
	}
	return ""
}

func findFromBrowserEnv() string {
	env := os.Getenv("BROWSER")
	if env == "" {
		return ""
	}
	lower := strings.ToLower(env)
	if !strings.Contains(lower, "chrome") &&
		!strings.Contains(lower, "chromium") &&
		!strings.Contains(lower, "edge") {
		return ""
	}
	if path, err := exec.LookPath(env); err == nil {
		return path
	}
	return ""
}

func findFromPath() string {
	for _, name := range chromeCLINames {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	return ""
}

func findFromMacOSPaths() string {
	for _, path := range macOSChromePaths {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}

func findFromWindowsPaths() string {
	for _, path := range windowsChromePaths {
		if path == "" {
			continue
		}
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}

func findFromLinuxPaths() string {
	for _, path := range linuxChromePaths {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}

func LaunchAppMode(browserPath, url, profileDir string) (*exec.Cmd, error) {
	if err := os.MkdirAll(profileDir, 0755); err != nil {
		return nil, fmt.Errorf("cannot create profile dir: %w", err)
	}

	cmd := exec.Command(browserPath,
		"--app="+url,
		"--user-data-dir="+profileDir,
	)

	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	// Reap the child when its window closes so it does not linger as a zombie.
	go cmd.Wait()

	return cmd, nil
}
