package browser

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// LaunchAppMode opens url in a chromeless Chrome "app" window.
//
// On macOS it launches Chrome through LaunchServices (`open`) instead of running
// the browser binary directly. Running the binary directly makes htmlclay the
// "responsible process" for everything Chrome does, so Chrome's launch-time
// permission requests (App Management, camera, mic, AppleEvents) get attributed
// to htmlclay and prompt the user about htmlclay. Going through `open` reparents
// Chrome to launchd, so Chrome is its own responsible process and htmlclay never
// appears in those prompts.
//
// `-n` forces a new instance so the flags are delivered even when a Chrome with
// this profile is already running; Chrome's per-profile singleton then routes
// the --app request into the existing window set.
func LaunchAppMode(browserPath, url, profileDir string) (*exec.Cmd, error) {
	if err := os.MkdirAll(profileDir, 0755); err != nil {
		return nil, fmt.Errorf("cannot create profile dir: %w", err)
	}

	appBundle := appBundlePath(browserPath)
	if appBundle == "" {
		// Not inside a .app bundle (e.g. a CLI chromium on $PATH); fall back to
		// launching the binary directly.
		return launchDirect(browserPath, url, profileDir)
	}

	cmd := exec.Command("open", "-n", "-a", appBundle, "--args",
		"--app="+url,
		"--user-data-dir="+profileDir,
		"--no-first-run",
		"--no-default-browser-check",
	)
	if err := cmd.Run(); err != nil {
		return nil, err
	}

	// `open` returns once Chrome has been handed off to LaunchServices; there is
	// no long-lived child process for htmlclay to track.
	return nil, nil
}

// appBundlePath returns the enclosing .app bundle for a path that points inside
// one, e.g. "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" ->
// "/Applications/Google Chrome.app". Returns "" when the path is not inside a
// .app bundle.
func appBundlePath(p string) string {
	const marker = ".app/"
	if i := strings.Index(p, marker); i >= 0 {
		return p[:i+len(marker)-1]
	}
	if strings.HasSuffix(p, ".app") {
		return p
	}
	return ""
}

func launchDirect(browserPath, url, profileDir string) (*exec.Cmd, error) {
	cmd := exec.Command(browserPath,
		"--app="+url,
		"--user-data-dir="+profileDir,
		"--no-first-run",
		"--no-default-browser-check",
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
