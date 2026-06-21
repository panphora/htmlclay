//go:build !darwin

package browser

import (
	"fmt"
	"os"
	"os/exec"
)

// LaunchAppMode opens url in a chromeless Chrome "app" window by launching the
// browser binary directly. The macOS "responsible process" problem that the
// darwin build avoids via `open` does not exist on these platforms, so the
// direct launch is kept.
func LaunchAppMode(browserPath, url, profileDir string) (*exec.Cmd, error) {
	if err := os.MkdirAll(profileDir, 0755); err != nil {
		return nil, fmt.Errorf("cannot create profile dir: %w", err)
	}

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
