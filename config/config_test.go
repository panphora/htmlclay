package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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

func TestTrustedFolderAddRemoveRoundTrip(t *testing.T) {
	baseDir := t.TempDir()
	cfg, _ := LoadFrom(baseDir)

	dirA := filepath.Join(baseDir, "sites")
	dirB := filepath.Join(baseDir, "projects")
	if err := os.MkdirAll(dirA, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dirB, 0755); err != nil {
		t.Fatal(err)
	}

	if !cfg.AddTrustedFolder(dirA) {
		t.Error("adding a new folder should report added")
	}
	if cfg.AddTrustedFolder(dirA) {
		t.Error("adding a duplicate should report not-added")
	}
	cfg.AddTrustedFolder(dirB)
	if err := cfg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := LoadFrom(baseDir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded.TrustedFolders) != 2 {
		t.Fatalf("expected 2 trusted folders after reload, got %v", loaded.TrustedFolders)
	}

	if !loaded.RemoveTrustedFolder(dirA) {
		t.Error("removing a present folder should report removed")
	}
	if loaded.RemoveTrustedFolder(dirA) {
		t.Error("removing an absent folder should report not-removed")
	}
	if err := loaded.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	reloaded, _ := LoadFrom(baseDir)
	if len(reloaded.TrustedFolders) != 1 || reloaded.TrustedFolders[0] != dirB {
		t.Errorf("expected only %q to remain, got %v", dirB, reloaded.TrustedFolders)
	}
}

func TestPruneTrustedFoldersDropsMissing(t *testing.T) {
	baseDir := t.TempDir()
	cfg, _ := LoadFrom(baseDir)

	real := filepath.Join(baseDir, "real")
	if err := os.MkdirAll(real, 0755); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(baseDir, "deleted")
	cfg.AddTrustedFolder(real)
	cfg.AddTrustedFolder(gone)
	if err := cfg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := LoadFrom(baseDir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded.TrustedFolders) != 1 || loaded.TrustedFolders[0] != real {
		t.Errorf("load should prune the missing folder, got %v", loaded.TrustedFolders)
	}
}

// The one Config is shared across the route, tray, and Trusted-Folders goroutines.
// Before the mutex, a SitePorts write concurrent with Save's marshal panicked with
// "concurrent map iteration and map write", and a TrustedFolders append tore under
// marshal. Run under -race; it must be clean and must not panic.
func TestConcurrentMutatorsAndSaveAreRaceFree(t *testing.T) {
	baseDir := t.TempDir()
	cfg, _ := LoadFrom(baseDir)

	const iters = 300
	var wg sync.WaitGroup
	run := func(f func(i int)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				f(i)
			}
		}()
	}

	run(func(i int) {
		cfg.RememberSitePort(fmt.Sprintf("/root/%d", i%8), i)
		_ = cfg.Save()
	})
	run(func(i int) {
		d := fmt.Sprintf("/trusted/%d", i%8)
		if !cfg.AddTrustedFolder(d) {
			cfg.RemoveTrustedFolder(d)
		}
		_ = cfg.Save()
	})
	run(func(i int) {
		cfg.SetMode([]string{"app", "browser"}[i%2])
		cfg.SetStartOnLogin(i%2 == 0)
		_ = cfg.Save()
	})
	run(func(i int) {
		_ = cfg.CurrentMode()
		_ = cfg.StartOnLoginEnabled()
		_ = cfg.SitePort("/root/1")
		_ = cfg.TrustedFolderList()
	})

	wg.Wait()
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
