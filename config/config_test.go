package config

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	baseDir := t.TempDir()
	cfg, err := LoadFrom(baseDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Mode != "app" {
		t.Errorf("expected mode 'app', got %q", cfg.Mode)
	}
	if cfg.StartOnLogin != false {
		t.Error("expected StartOnLogin false")
	}
	if cfg.Port != 0 {
		t.Errorf("expected port 0, got %d", cfg.Port)
	}
}

func TestSaveAndLoad(t *testing.T) {
	baseDir := t.TempDir()
	cfg, _ := LoadFrom(baseDir)
	cfg.Mode = "browser"
	cfg.StartOnLogin = true
	cfg.Port = 12345
	if err := cfg.Save(); err != nil {
		t.Fatalf("save error: %v", err)
	}

	loaded, err := LoadFrom(baseDir)
	if err != nil {
		t.Fatalf("load error: %v", err)
	}
	if loaded.Mode != "browser" {
		t.Errorf("expected mode 'browser', got %q", loaded.Mode)
	}
	if loaded.StartOnLogin != true {
		t.Error("expected StartOnLogin true")
	}
	if loaded.Port != 12345 {
		t.Errorf("expected port 12345, got %d", loaded.Port)
	}
}

func TestLoadCorruptRecoversToDefaults(t *testing.T) {
	baseDir := t.TempDir()
	dir := DirFrom(baseDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{not valid json"), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFrom(baseDir)
	if err != nil {
		t.Fatalf("a corrupt config should not error, got: %v", err)
	}
	if cfg.Mode != "app" {
		t.Errorf("expected default mode 'app', got %q", cfg.Mode)
	}
}

func TestSaveIsAtomicNoTempLeft(t *testing.T) {
	baseDir := t.TempDir()
	cfg, _ := LoadFrom(baseDir)
	cfg.Port = 4321
	if err := cfg.Save(); err != nil {
		t.Fatalf("save error: %v", err)
	}

	entries, err := os.ReadDir(DirFrom(baseDir))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".config-") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}

	info, err := os.Stat(filepath.Join(DirFrom(baseDir), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Errorf("expected config.json mode 0600, got %v", info.Mode().Perm())
	}
}

func TestEnsureDir(t *testing.T) {
	baseDir := t.TempDir()
	dir := DirFrom(baseDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("MkdirAll error: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("dir not created: %v", err)
	}
	if !info.IsDir() {
		t.Error("expected directory")
	}
}

func TestResolvePortPicksAvailable(t *testing.T) {
	baseDir := t.TempDir()
	cfg, _ := LoadFrom(baseDir)
	ln, err := cfg.ResolvePort()
	if err != nil {
		t.Fatalf("ResolvePort error: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	if port == 0 {
		t.Error("expected non-zero port")
	}
}

func TestResolvePortReusesSaved(t *testing.T) {
	baseDir := t.TempDir()
	cfg, _ := LoadFrom(baseDir)
	ln1, err := cfg.ResolvePort()
	if err != nil {
		t.Fatal(err)
	}
	port1 := ln1.Addr().(*net.TCPAddr).Port
	ln1.Close()

	ln2, err := cfg.ResolvePort()
	if err != nil {
		t.Fatal(err)
	}
	port2 := ln2.Addr().(*net.TCPAddr).Port
	ln2.Close()

	if port1 != port2 {
		t.Errorf("expected same port %d, got %d", port1, port2)
	}
}
