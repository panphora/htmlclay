package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type Config struct {
	// mu guards every field below. The one Config is shared across goroutines: the
	// route path writes SitePorts, the tray writes StartOnLogin, and the Trusted
	// Folders hooks write TrustedFolders, and Save marshals all of them at once.
	// Without one lock a concurrent SitePorts write + marshal panics
	// ("concurrent map iteration and write") and a slice append tears under marshal.
	// Every exported mutator and Save take it; callers must not touch the fields
	// directly. Lock order is a.mu -> cfg.mu (nothing takes cfg.mu then a.mu).
	mu           sync.Mutex
	StartOnLogin bool `json:"startOnLogin"`
	// SitePorts remembers the loopback port each origin was served on.
	// Browser storage (localStorage, IndexedDB, cookies) is scoped to an origin,
	// and the port is part of the origin, so handing a tree a fresh random port
	// on every launch silently orphans whatever the page stored last time.
	// Keyed by the origin's anchor: a trusted folder, or an ordinary directory
	// for a file opened outside every trusted folder.
	SitePorts map[string]int `json:"sitePorts,omitempty"`
	// TrustedFolders are folders the user declared theirs. HTML Clay files under
	// one open editable with no prompts, including files added later, and any
	// file in it can change any other. This is the app's one durable permission
	// fact: it grants read and write, it anchors exactly one origin, and it is
	// the key that origin's port is remembered under.
	//
	// These are never pruned on Load: a trusted folder is a standing write
	// capability, and a dead or identity-changed entry must surface in the tray
	// as dead rather than silently vanish from the record of what the user
	// granted.
	//
	// The on-disk key stays "workspaceFolders" even though the concept is now
	// called a trusted folder. Reusing the "trustedFolders" key would make
	// json.Unmarshal fail against 1.2.0 configs, where it held a []string, and
	// the corrupt-config path would then reset every setting the user has.
	TrustedFolders []TrustedFolder `json:"workspaceFolders,omitempty"`
	// LegacyTrusted is 1.2.0's read-only trusted folder list. It is promoted to
	// TrustedFolders once on Load and cleared on the next Save, which is the
	// completion marker. Distinct Go field, distinct JSON key, distinct type, so
	// the decoder never sees a shape it does not expect.
	LegacyTrusted []string `json:"trustedFolders,omitempty"`
	baseDir       string
}

// TrustedFolder is one declared folder. Identity is the folder's device+inode
// fingerprint at declaration time ("" where the platform cannot provide one);
// callers compare it before installing so a directory swapped for a symlink
// since declaration is refused under the old name instead of granting write
// over whatever tree the link now points at.
type TrustedFolder struct {
	Path     string `json:"path"`
	Identity string `json:"identity,omitempty"`
}

// sitePortCap bounds the remembered-port map. Every entry becomes a bound
// listener at startup, so an unbounded map is an unbounded number of sockets
// held from login. A trusted folder's entry is never evicted: it is the
// bookmark contract. Ad-hoc entries are evicted only when the map is over the
// cap, stat-failures first, because a stat failure cannot distinguish a
// deleted folder from an unmounted volume and dropping a merely unmounted
// drive's port would move a URL the user still has open in a tab.
const sitePortCap = 32

// AddTrustedFolder records dir with its identity fingerprint, returning false
// if the path was already present. dir must already be canonical (resolved,
// home-contained); the caller validates before adding, so containment checks
// here can stay simple.
func (c *Config) AddTrustedFolder(dir, identity string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, w := range c.TrustedFolders {
		if w.Path == dir {
			return false
		}
	}
	c.TrustedFolders = append(c.TrustedFolders, TrustedFolder{Path: dir, Identity: identity})
	return true
}

// RemoveTrustedFolder drops dir from the list, returning the entry it removed
// along with whether it was there at all.
//
// The entry comes back so a caller whose removal fails to reach disk can restore
// exactly what it took out. Re-adding a freshly built entry instead re-pins the
// folder to whatever is at that path now, which turns a dead entry (folder
// deleted and replaced, granting nothing) into a live grant over the newcomer —
// the one thing the identity pin exists to prevent, arrived at through an error
// path.
func (c *Config) RemoveTrustedFolder(dir string) (TrustedFolder, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, w := range c.TrustedFolders {
		if w.Path == dir {
			c.TrustedFolders = append(c.TrustedFolders[:i], c.TrustedFolders[i+1:]...)
			return w, true
		}
	}
	return TrustedFolder{}, false
}

// SetTrustedIdentity re-pins an existing entry and returns the pin it replaced,
// so a caller whose save fails can put it back. ok reports whether there was an
// entry to re-pin. The pin is what makes a replaced folder stop granting;
// moving it is how an explicit re-approval of the folder now on disk takes
// effect.
func (c *Config) SetTrustedIdentity(dir, identity string) (previous string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, w := range c.TrustedFolders {
		if w.Path == dir {
			previous = w.Identity
			c.TrustedFolders[i].Identity = identity
			return previous, true
		}
	}
	return "", false
}

// TrustedFolderList returns a copy of the trusted folders so callers can read
// the list without touching the field under the lock.
func (c *Config) TrustedFolderList() []TrustedFolder {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]TrustedFolder(nil), c.TrustedFolders...)
}

// promoteLegacyTrusted turns 1.2.0's read-only trusted folders into ordinary
// trusted folders, which now grant write. This is a deliberate widening: the
// concept the user agreed to is gone, and the closest surviving one is the
// full trust. Each promoted entry is pinned to the directory that is there now,
// since no pin was ever stored for it. Returns whether anything changed.
func (c *Config) promoteLegacyTrusted(identity func(string) string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.LegacyTrusted) == 0 {
		return false
	}
	existing := make(map[string]bool, len(c.TrustedFolders))
	for _, w := range c.TrustedFolders {
		existing[w.Path] = true
	}
	for _, dir := range c.LegacyTrusted {
		if existing[dir] {
			continue
		}
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			continue
		}
		c.TrustedFolders = append(c.TrustedFolders, TrustedFolder{Path: dir, Identity: identity(dir)})
		existing[dir] = true
	}
	c.LegacyTrusted = nil
	return true
}

// dedupeTrustedFolders drops entries that name a directory an earlier entry
// already names, keeping the first and its pin.
//
// Until 1.4.0 the list compared paths byte for byte while trust.Canonical stored
// whatever capitalization the caller supplied, so a page could add a second entry
// for one folder by asking for an asset under a different casing, and untrusting
// the spelling the user recognized left the other one granting write. Canonical
// now stores the filesystem's own spelling, which stops new duplicates; this
// clears out the ones already written to disk.
//
// Sameness is os.SameFile, never a case-folded string compare. session.EqualOrUnder
// follows the home volume's rule, and its own comment warns that a volume with
// different folding mounted inside home is misjudged: folding here would merge two
// genuinely distinct directories and hand one folder's pin to the other. A dead
// entry stats nothing, so it matches only its own byte-exact path and survives,
// which is what keeps it visible in the tray as dead.
func (c *Config) dedupeTrustedFolders() {
	c.mu.Lock()
	defer c.mu.Unlock()
	type seen struct {
		path string
		info os.FileInfo
	}
	var keptInfo []seen
	kept := make([]TrustedFolder, 0, len(c.TrustedFolders))
	for _, w := range c.TrustedFolders {
		info, err := os.Stat(w.Path)
		if err != nil {
			info = nil
		}
		duplicate := false
		for _, s := range keptInfo {
			if s.path == w.Path || (info != nil && s.info != nil && os.SameFile(info, s.info)) {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		kept = append(kept, w)
		keptInfo = append(keptInfo, seen{path: w.Path, info: info})
	}
	c.TrustedFolders = kept
}

// SitePort returns the port previously used for an anchor, or 0 if there is none.
func (c *Config) SitePort(anchor string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.SitePorts[anchor]
}

// RememberSitePort records the port an origin was served on so the next launch
// can reuse it and keep the origin stable.
func (c *Config) RememberSitePort(anchor string, port int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.SitePorts == nil {
		c.SitePorts = make(map[string]int)
	}
	c.SitePorts[anchor] = port
}

// ForgetSitePort drops the remembered port for anchor and returns it, so a
// caller whose change fails to reach disk can put it back.
//
// Untrusting a folder forgets its port. Keeping it would hand the folder's exact
// origin straight back to the first file re-homed out of it: that file's own
// folder IS the untrusted folder, so it anchors there and binds the same
// remembered port, leaving the untrusted folder's still-open pages same-origin
// with a file that has a live save token. "Files you had opened yourself
// survive, on a new address of their own" is the promise, and the new address is
// the whole of it.
func (c *Config) ForgetSitePort(anchor string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	port := c.SitePorts[anchor]
	delete(c.SitePorts, anchor)
	return port
}

// SitePortList returns a copy of the remembered ports for startup planning.
func (c *Config) SitePortList() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int, len(c.SitePorts))
	for k, v := range c.SitePorts {
		out[k] = v
	}
	return out
}

// capSitePorts enforces sitePortCap. See the constant for why eviction is
// bounded to the over-cap case rather than run on every load.
func (c *Config) capSitePorts() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.SitePorts) <= sitePortCap {
		return
	}
	protected := make(map[string]bool, len(c.TrustedFolders))
	for _, w := range c.TrustedFolders {
		protected[w.Path] = true
	}
	var missing, present []string
	for root := range c.SitePorts {
		if protected[root] {
			continue
		}
		if info, err := os.Stat(root); err != nil || !info.IsDir() {
			missing = append(missing, root)
		} else {
			present = append(present, root)
		}
	}
	sort.Strings(missing)
	sort.Strings(present)
	for _, root := range append(missing, present...) {
		if len(c.SitePorts) <= sitePortCap {
			return
		}
		delete(c.SitePorts, root)
	}
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

// Result reports what Load had to do to the file, so startup can act on a
// one-time migration without a live field for a setting that no longer exists.
type Result struct {
	// HadAppMode is true when the loaded file still carried App Mode. Startup
	// uses it to delete the Chromium profile directory that mode created.
	HadAppMode bool
	// PromotedLegacy is true when 1.2.0 read-only trusted folders were widened
	// into trusted folders.
	PromotedLegacy bool
}

func Load(identity func(string) string) (*Config, Result, error) {
	base, err := defaultConfigDir()
	if err != nil {
		return nil, Result{}, err
	}
	return LoadFrom(base, identity)
}

func LoadFrom(baseDir string, identity func(string) string) (*Config, Result, error) {
	cfg := &Config{StartOnLogin: false, baseDir: baseDir}
	var res Result

	path := filepath.Join(DirFrom(baseDir), "config.json")

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return cfg, res, nil
	}
	if err != nil {
		return nil, res, err
	}

	if uErr := json.Unmarshal(data, cfg); uErr != nil {
		// A corrupt config must not brick startup, and it must not silently
		// erase what the user granted either. Falling back to defaults and
		// letting the next Save overwrite the file is how a single mis-shaped
		// field used to take the whole trusted-folder list with it. Move the bad
		// file aside first, so it is recoverable and so the user can be told.
		quarantine := fmt.Sprintf("%s.corrupt-%d", path, time.Now().Unix())
		if rErr := os.Rename(path, quarantine); rErr != nil {
			fmt.Fprintf(os.Stderr, "[htmlclay] config.json is corrupt (%v) and could not be set aside (%v), using defaults\n", uErr, rErr)
		} else {
			fmt.Fprintf(os.Stderr, "[htmlclay] config.json is corrupt (%v); saved it as %s and starting from defaults\n", uErr, quarantine)
		}
		return &Config{StartOnLogin: false, baseDir: baseDir}, res, nil
	}

	// "mode" is App Mode's only trace once the field is gone. Read it straight
	// from the bytes rather than keeping a live setting for a feature that no
	// longer exists; the next Save drops the key, which completes the migration.
	var legacy struct {
		Mode string `json:"mode"`
	}
	if json.Unmarshal(data, &legacy) == nil && legacy.Mode == "app" {
		res.HadAppMode = true
	}

	res.PromotedLegacy = cfg.promoteLegacyTrusted(identity)
	// After the promotion, which appends straight to the list with a byte-exact
	// dedupe of its own and so can add an alias of an entry already there.
	cfg.dedupeTrustedFolders()
	cfg.capSitePorts()
	return cfg, res, nil
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
