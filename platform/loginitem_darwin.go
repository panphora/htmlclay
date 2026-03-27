//go:build darwin

package platform

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"html/template"
)

const launchAgentLabel = "com.htmlclay"

func launchAgentPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist"), nil
}

var plistTmpl = template.Must(template.New("plist").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>{{.Label}}</string>
    <key>ProgramArguments</key>
    <array>
        <string>{{.ExecPath}}</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
</dict>
</plist>`))

func SetLoginItem(enabled bool, executablePath string) error {
	path, err := launchAgentPath()
	if err != nil {
		return err
	}

	if !enabled {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}

	var buf bytes.Buffer
	err = plistTmpl.Execute(&buf, struct {
		Label    string
		ExecPath string
	}{launchAgentLabel, executablePath})
	if err != nil {
		return fmt.Errorf("cannot render plist: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("cannot create LaunchAgents dir: %w", err)
	}

	return os.WriteFile(path, buf.Bytes(), 0644)
}

func IsLoginItem() bool {
	path, err := launchAgentPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}
