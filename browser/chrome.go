package browser

import (
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

// chromeCommandsFromBrowserEnv parses a $BROWSER value into the ordered
// Chromium-family command tokens to try. On Unix it follows the freedesktop
// convention: a colon-separated list of commands, each of which may carry a %s
// URL placeholder and trailing whitespace-separated args. On Windows there is no
// such convention and paths contain ':' and spaces, so the whole value is one
// command. Only chrome/chromium/edge commands are kept, since LaunchAppMode
// relies on Chrome's --app flag.
func chromeCommandsFromBrowserEnv(env string) []string {
	var entries []string
	if runtime.GOOS == "windows" {
		entries = []string{env}
	} else {
		entries = strings.Split(env, ":")
	}

	var cmds []string
	for _, entry := range entries {
		cmd := strings.TrimSpace(entry)
		if runtime.GOOS != "windows" {
			fields := strings.Fields(strings.ReplaceAll(entry, "%s", " "))
			if len(fields) == 0 {
				continue
			}
			cmd = fields[0]
		}
		if cmd == "" {
			continue
		}
		lower := strings.ToLower(cmd)
		if strings.Contains(lower, "chrome") ||
			strings.Contains(lower, "chromium") ||
			strings.Contains(lower, "edge") {
			cmds = append(cmds, cmd)
		}
	}
	return cmds
}

func findFromBrowserEnv() string {
	env := os.Getenv("BROWSER")
	if env == "" {
		return ""
	}
	for _, cmd := range chromeCommandsFromBrowserEnv(env) {
		if path, err := exec.LookPath(cmd); err == nil {
			return path
		}
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
