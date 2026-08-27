//go:build linux

package platform

import (
	"fmt"
	"os"
	"path/filepath"
)

func autostartDir() (string, error) {
	configDir := os.Getenv("XDG_CONFIG_HOME")
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot determine home directory: %w", err)
		}
		configDir = filepath.Join(home, ".config")
	}
	return filepath.Join(configDir, "autostart"), nil
}

func SetLoginItem(enabled bool, executablePath string) error {
	dir, err := autostartDir()
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "htmlclay.desktop")

	if !enabled {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("cannot create autostart dir: %w", err)
	}

	content := fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=HTML Clay
Exec="%s"
X-GNOME-Autostart-enabled=true
`, executablePath)

	return os.WriteFile(path, []byte(content), 0644)
}

func IsLoginItem() bool {
	dir, err := autostartDir()
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(dir, "htmlclay.desktop"))
	return err == nil
}
