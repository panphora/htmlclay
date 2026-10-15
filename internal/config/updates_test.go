package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUpdatesOffIsReadAndKeptAcrossSave(t *testing.T) {
	baseDir := t.TempDir()
	dir := DirFrom(baseDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"updates":"off"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := LoadFrom(baseDir, noIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.SelfUpdateOff() {
		t.Fatal(`"updates": "off" did not turn the self-updater off`)
	}
	cfg.SetStartOnLogin(true)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := LoadFrom(baseDir, noIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.SelfUpdateOff() {
		t.Fatal("saving the config dropped the updates setting")
	}

	fresh, _, _ := LoadFrom(t.TempDir(), noIdentity)
	if fresh.SelfUpdateOff() {
		t.Fatal("a config without the setting reads as off")
	}
}
