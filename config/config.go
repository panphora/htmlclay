package config

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
)

type Config struct {
	// mu guards every field below. The one Config is shared across goroutines: the
	// route path writes SitePorts, the tray writes Mode/StartOnLogin, and the
	// Trusted Folders hooks write TrustedFolders, and Save marshals all of them at
	// once. Without one lock a concurrent SitePorts write + marshal panics
	// ("concurrent map iteration and write") and a slice append tears under marshal.
	// Every exported mutator and Save take it; callers must not touch the fields
	// directly. Lock order is a.mu -> cfg.mu (nothing takes cfg.mu then a.mu).
	mu           sync.Mutex
	Mode         string `json:"mode"`
	StartOnLogin bool   `json:"startOnLogin"`
	Port         int    `json:"port"`
	// SitePorts remembers the loopback port each opened tree was served on.
	// Browser storage (localStorage, IndexedDB, cookies) is scoped to an origin,
	// and the port is part of the origin, so handing a tree a fresh random port
	// on every launch silently orphans whatever the page stored last time.
	// Keyed by the tree's root directory.
	SitePorts map[string]int `json:"sitePorts,omitempty"`
	// TrustedFolders are folders the user marked as their own. A file opened from
	// inside one is silently allowed to read that folder's whole tree with no
	// permission prompt. Stored as canonical absolute paths; the caller canonicalizes
	// and validates (inside home, not the config tree, no hidden component) before
	// adding, so containment checks here can stay simple.
	TrustedFolders []string `json:"trustedFolders,omitempty"`
	// WorkspaceFolders are folders the user declared fully theirs: HTML Clay
	// files under one auto-register and self-save with no prompts, which makes
	// this the one WRITE-granting trust in the config. Same canonical-path
	// contract as TrustedFolders. Unlike trusted folders these are never pruned
	// on Load: a workspace is a standing write capability, and a dead or
	// identity-changed entry must surface in the tray as dead rather than
	// silently vanish from the record of what the user granted.
	WorkspaceFolders []WorkspaceFolder `json:"workspaceFolders,omitempty"`
	baseDir          string
}

// WorkspaceFolder is one declared workspace. Identity is the folder's
// device+inode fingerprint at declaration time ("" where the platform cannot
// provide one); installers compare it before seeding so a directory swapped for
// a symlink since declaration is refused under the old name instead of granting
// write over whatever tree the link now points at.
type WorkspaceFolder struct {
	Path     string `json:"path"`
	Identity string `json:"identity,omitempty"`
}

// AddWorkspaceFolder records dir as a workspace with its identity fingerprint,
// returning false if the path was already present. dir must already be
// canonical (resolved, home-contained), like AddTrustedFolder.
func (c *Config) AddWorkspaceFolder(dir, identity string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, w := range c.WorkspaceFolders {
		if w.Path == dir {
			return false
		}
	}
	c.WorkspaceFolders = append(c.WorkspaceFolders, WorkspaceFolder{Path: dir, Identity: identity})
	return true
}

// RemoveWorkspaceFolder drops dir from the workspace list, returning whether it
// was present.
func (c *Config) RemoveWorkspaceFolder(dir string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, w := range c.WorkspaceFolders {
		if w.Path == dir {
			c.WorkspaceFolders = append(c.WorkspaceFolders[:i], c.WorkspaceFolders[i+1:]...)
			return true
		}
	}
	return false
}

// WorkspaceFolderList returns a copy of the workspace folders so callers can
// read the list without touching the field under the lock.
func (c *Config) WorkspaceFolderList() []WorkspaceFolder {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]WorkspaceFolder(nil), c.WorkspaceFolders...)
}

// AddTrustedFolder records dir as trusted, returning false if it was already
// present. dir must already be canonical (resolved, home-contained).
func (c *Config) AddTrustedFolder(dir string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, d := range c.TrustedFolders {
		if d == dir {
			return false
		}
	}
	c.TrustedFolders = append(c.TrustedFolders, dir)
	return true
}

// RemoveTrustedFolder drops dir from the trusted list, returning whether it was
// present.
func (c *Config) RemoveTrustedFolder(dir string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, d := range c.TrustedFolders {
		if d == dir {
			c.TrustedFolders = append(c.TrustedFolders[:i], c.TrustedFolders[i+1:]...)
			return true
		}
	}
	return false
}

// TrustedFolderList returns a copy of the trusted folders. It exists so callers
// can read the list without touching the field under the lock.
func (c *Config) TrustedFolderList() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.TrustedFolders...)
}

// pruneTrustedFolders drops trusted entries whose directory no longer exists, so a
// deleted folder does not linger in config across the life of an install.
func (c *Config) pruneTrustedFolders() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.TrustedFolders) == 0 {
		return
	}
	kept := make([]string, 0, len(c.TrustedFolders))
	for _, d := range c.TrustedFolders {
		if info, err := os.Stat(d); err == nil && info.IsDir() {
			kept = append(kept, d)
		}
	}
	c.TrustedFolders = kept
}

// SitePort returns the port previously used for root, or 0 if there is none.
func (c *Config) SitePort(root string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.SitePorts[root]
}

// RememberSitePort records the port a tree was served on so the next launch can
// reuse it and keep the origin stable.
func (c *Config) RememberSitePort(root string, port int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.SitePorts == nil {
		c.SitePorts = make(map[string]int)
	}
	c.SitePorts[root] = port
}

// pruneSitePorts drops remembered ports for trees that no longer exist, so the
// map cannot grow without bound across the life of an install.
func (c *Config) pruneSitePorts() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for root := range c.SitePorts {
		if info, err := os.Stat(root); err != nil || !info.IsDir() {
			delete(c.SitePorts, root)
		}
	}
}

// CurrentMode returns the launch mode under the lock.
func (c *Config) CurrentMode() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Mode
}

// SetMode sets the launch mode and returns the previous value so a caller can roll
// back if a following Save fails.
func (c *Config) SetMode(mode string) (prev string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prev, c.Mode = c.Mode, mode
	return prev
}

// StartOnLoginEnabled reports the start-on-login preference under the lock.
func (c *Config) StartOnLoginEnabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.StartOnLogin
}

// SetStartOnLogin sets the start-on-login preference under the lock.
func (c *Config) SetStartOnLogin(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.StartOnLogin = v
}

func defaultConfigDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine config directory: %w", err)
	}
	return base, nil
}

func Dir() (string, error) {
	base, err := defaultConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "htmlclay"), nil
}

func DirFrom(baseDir string) string {
	return filepath.Join(baseDir, "htmlclay")
}

func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

func EnsureDir() error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	return os.MkdirAll(dir, 0755)
}

func Load() (*Config, error) {
	base, err := defaultConfigDir()
	if err != nil {
		return nil, err
	}
	return LoadFrom(base)
}

func LoadFrom(baseDir string) (*Config, error) {
	cfg := &Config{
		Mode:         "app",
		StartOnLogin: false,
		Port:         0,
		baseDir:      baseDir,
	}

	path := filepath.Join(DirFrom(baseDir), "config.json")

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal(data, cfg); err != nil {
		// A corrupt config must not brick startup: warn and fall back to defaults.
		fmt.Fprintf(os.Stderr, "[htmlclay] config.json is corrupt (%v), using defaults\n", err)
		return &Config{Mode: "app", StartOnLogin: false, Port: 0, baseDir: baseDir}, nil
	}
	cfg.pruneSitePorts()
	cfg.pruneTrustedFolders()
	return cfg, nil
}

func (c *Config) Save() error {
	// Hold the lock across the marshal so a concurrent SitePorts/TrustedFolders
	// mutation cannot tear the snapshot or panic the map iteration. The disk write
	// stays under the lock too, which serialises Saves and prevents two writers'
	// temp-rename races from resurrecting a just-removed entry.
	c.mu.Lock()
	defer c.mu.Unlock()
	dir := DirFrom(c.baseDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	path := filepath.Join(dir, "config.json")
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0600); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func (c *Config) ResolvePort() (net.Listener, error) {
	if c.Port != 0 {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", c.Port))
		if err == nil {
			return ln, nil
		}
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}

	c.Port = ln.Addr().(*net.TCPAddr).Port
	if err := c.Save(); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}
