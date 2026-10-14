package config

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
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
	// ListenPort is the one loopback port every site is served on, held on 127.0.0.1 and
	// ::1 for as long as the app runs. 0 until the first launch chooses one. The port is
	// part of the browser origin, so a change here resets every page's browser storage,
	// which is why the app asks before moving it. Read through Port().
	ListenPort int `json:"port,omitempty"`
	// SiteSlugs maps an origin key to the label it is served under, <label>.localhost:<port>.
	// The key is the site's anchor (a trusted folder, or the folder a loose file was opened
	// from), or the file's own path for a file loose in the home directory, which can
	// install no read root and so shares its anchor with every other such file. A slug is
	// chosen once and never changes, so a bookmark survives restarts. Trusted folders'
	// entries are never evicted; the rest are capped like SitePorts.
	SiteSlugs map[string]string `json:"siteSlugs,omitempty"`
	// RetiredSlugs are labels that were once issued and then forgotten (the folder was
	// untrusted, or the entry was evicted). They are never issued again, so no folder can
	// inherit another's browser storage, and their addresses answer the recovery page.
	RetiredSlugs []string `json:"retiredSlugs,omitempty"`
	// NextSiteNumber is the number the next new slug gets: 1 is htmlclay, 2 is htmlclay1, and so
	// on. It only grows, so a label is never issued twice and no folder inherits another's
	// browser storage.
	NextSiteNumber int `json:"nextSiteNumber,omitempty"`
	// SitePorts is the pre-hostname record of the loopback port each origin was served on.
	// Kept so an old bookmark can be redirected: startup binds each remembered port that is
	// still free with a redirect to the folder's hostname, and drops the entry the first
	// time the port is found taken. Nothing new is ever written here.
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
	LegacyTrusted   []string         `json:"trustedFolders,omitempty"`
	HelperPrograms  []HelperProgram  `json:"helperPrograms,omitempty"`
	HelperDecisions []HelperDecision `json:"helperDecisions,omitempty"`
	// AIEdit is the built-in AI editing helper's setting. Nil, or a nil Enabled,
	// reads as on: AI editing is on unless the person turned it off.
	AIEdit *AIEditSettings `json:"aiEdit,omitempty"`
	// Profile is the person HTML Clay names in files it serves, when Enabled. Nil
	// means never set up, which reads as off. The id survives renames and toggles.
	Profile *Profile `json:"profile,omitempty"`
	baseDir string
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

// AIEditSettings mirrors Hyperclay Local's settings.aiEdit. Default names the engine
// used when a request names none ("" means claude). Engines are user-defined agents:
// name -> argv, where an argument containing "{prompt}" receives the prompt.
type AIEditSettings struct {
	Enabled *bool               `json:"enabled,omitempty"`
	Default string              `json:"default,omitempty"`
	Engines map[string][]string `json:"engines,omitempty"`
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

// backfillIdentities pins entries that were stored without a fingerprint, and
// reports whether it pinned any.
//
// Windows had no directory fingerprint until DirIdentity learned to read the
// file id, so every trusted folder a Windows build recorded before that carries
// an empty pin. An empty pin means the path is the entry's whole identity
// (trust.IdentityOK), which keeps those folders working, and must: they are
// standing write grants the user made, and turning them dead on upgrade would
// silently revoke them. Pinning them to the directory that is there now is the
// same trade promoteLegacyTrusted makes for the same reason, and it is not a
// weakening: a folder already swapped before the upgrade was granting to the
// newcomer anyway, and after this any later swap is caught.
//
// A folder that is gone, or that stats as something other than a directory, is
// left alone. Its entry is the record of a grant and has to stay visible in the
// tray as dead rather than gain a pin for whatever now sits at its path.
//
// Nothing is written to disk here. Load reports the change through its Result and
// the caller saves, exactly as the legacy promotion's does; until that Save lands
// the pin is re-derived on every Load.
func (c *Config) backfillIdentities(identity func(string) string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	pinned := false
	for i, w := range c.TrustedFolders {
		if w.Identity != "" {
			continue
		}
		if info, err := os.Stat(w.Path); err != nil || !info.IsDir() {
			continue
		}
		id := identity(w.Path)
		if id == "" {
			continue
		}
		c.TrustedFolders[i].Identity = id
		pinned = true
	}
	return pinned
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

// siteSlugCap bounds the slug map the way sitePortCap bounds the legacy one; a slug
// costs no socket, so the cap is only about config size.
const siteSlugCap = 64

// Port returns the remembered port, 0 when none has been chosen.
func (c *Config) Port() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ListenPort
}

// SetPort records the port the app is serving on.
func (c *Config) SetPort(port int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ListenPort = port
}

// SiteSlug returns the slug remembered for key, or "" if there is none.
func (c *Config) SiteSlug(key string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.SiteSlugs[key]
}

// SiteKeyForSlug is the reverse lookup the router needs for a hostname with no live site.
// A retired slug has no key.
func (c *Config) SiteKeyForSlug(slug string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, s := range c.SiteSlugs {
		if s == slug {
			return key, true
		}
	}
	return "", false
}

// SlugInUse reports whether slug is issued or retired, so an allocator never reissues it.
func (c *Config) SlugInUse(slug string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.slugInUseLocked(slug)
}

func (c *Config) slugInUseLocked(slug string) bool {
	for _, s := range c.SiteSlugs {
		if s == slug {
			return true
		}
	}
	for _, s := range c.RetiredSlugs {
		if s == slug {
			return true
		}
	}
	return false
}

// AllocateSiteSlug records and returns key's slug: the next label in the htmlclay, htmlclay1,
// htmlclay2, ... sequence, skipping any label already issued or retired. The counter only grows.
func (c *Config) AllocateSiteSlug(key string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := max(c.NextSiteNumber, 1)
	for {
		slug := numberedSlugFor(n)
		n++
		if c.slugInUseLocked(slug) {
			continue
		}
		if c.SiteSlugs == nil {
			c.SiteSlugs = make(map[string]string)
		}
		c.SiteSlugs[key] = slug
		c.NextSiteNumber = n
		return slug
	}
}

// RememberSiteSlug records key's slug. The caller has checked SlugInUse.
func (c *Config) RememberSiteSlug(key, slug string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.SiteSlugs == nil {
		c.SiteSlugs = make(map[string]string)
	}
	c.SiteSlugs[key] = slug
}

// ForgetSiteSlug drops key's slug, retires it, and returns it so a caller whose change
// fails to reach disk can RestoreSiteSlug. Untrusting a folder forgets its slug for the
// reason ForgetSitePort gives: the re-homed survivors must land on a new origin.
func (c *Config) ForgetSiteSlug(key string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	slug := c.SiteSlugs[key]
	delete(c.SiteSlugs, key)
	if slug != "" {
		c.retireLocked(slug)
	}
	return slug
}

// DropSiteSlug removes key's slug without retiring it. It is for a label that was
// allocated and never served: no page ever ran on it, so it has no storage to protect.
func (c *Config) DropSiteSlug(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.SiteSlugs, key)
}

// RestoreSiteSlug undoes ForgetSiteSlug after a failed save.
func (c *Config) RestoreSiteSlug(key, slug string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.SiteSlugs == nil {
		c.SiteSlugs = make(map[string]string)
	}
	c.SiteSlugs[key] = slug
	kept := c.RetiredSlugs[:0]
	for _, s := range c.RetiredSlugs {
		if s != slug {
			kept = append(kept, s)
		}
	}
	c.RetiredSlugs = kept
}

// SiteSlugList returns a copy of the slug map.
func (c *Config) SiteSlugList() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]string, len(c.SiteSlugs))
	for k, v := range c.SiteSlugs {
		out[k] = v
	}
	return out
}

// retireLocked appends slug to RetiredSlugs once. The list is never trimmed: a label that
// left it could be issued again, and the folder that got it would inherit the old origin's
// browser storage.
func (c *Config) retireLocked(slug string) {
	for _, s := range c.RetiredSlugs {
		if s == slug {
			return
		}
	}
	c.RetiredSlugs = append(c.RetiredSlugs, slug)
}

var numberedSlug = regexp.MustCompile(`^htmlclay([1-9][0-9]*)?$`)

// numberedSlugFor maps counter value n (from 1) to its label: htmlclay, htmlclay1, htmlclay2, ...
func numberedSlugFor(n int) string {
	if n == 1 {
		return "htmlclay"
	}
	return "htmlclay" + strconv.Itoa(n-1)
}

// retireNamedSlugs moves a config from the earlier <folder>-<4 hex> labels to numbered
// ones: each label of the old shape is retired, and its key gets the next number when its
// site is next built.
func (c *Config) retireNamedSlugs() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, slug := range c.SiteSlugs {
		if !numberedSlug.MatchString(slug) {
			c.retireLocked(slug)
			delete(c.SiteSlugs, key)
		}
	}
}

// dedupeSiteSlugs keeps one key per slug value. A trusted folder's entry wins, then the
// lexically first key; the others are deleted, not retired, because the label is still in
// use, and they get a fresh slug the next time they are opened. Two keys on one label would
// put two sites on one hostname.
func (c *Config) dedupeSiteSlugs() {
	c.mu.Lock()
	defer c.mu.Unlock()
	trusted := make(map[string]bool, len(c.TrustedFolders))
	for _, w := range c.TrustedFolders {
		trusted[w.Path] = true
	}
	keys := make([]string, 0, len(c.SiteSlugs))
	for key := range c.SiteSlugs {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if trusted[keys[i]] != trusted[keys[j]] {
			return trusted[keys[i]]
		}
		return keys[i] < keys[j]
	})
	owner := make(map[string]bool, len(keys))
	for _, key := range keys {
		slug := c.SiteSlugs[key]
		if owner[slug] {
			delete(c.SiteSlugs, key)
			continue
		}
		owner[slug] = true
	}
}

// capSiteSlugs enforces siteSlugCap with capSitePorts's rules: trusted folders are never
// evicted, and among the rest, keys that no longer stat go first. A key may be a file
// (a loose file in home), so the stat is for existence, not for a directory. Evicted
// slugs are retired.
func (c *Config) capSiteSlugs() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.SiteSlugs) <= siteSlugCap {
		return
	}
	protected := make(map[string]bool, len(c.TrustedFolders))
	for _, w := range c.TrustedFolders {
		protected[w.Path] = true
	}
	var missing, present []string
	for key := range c.SiteSlugs {
		if protected[key] {
			continue
		}
		if _, err := os.Stat(key); err != nil {
			missing = append(missing, key)
		} else {
			present = append(present, key)
		}
	}
	sort.Strings(missing)
	sort.Strings(present)
	for _, key := range append(missing, present...) {
		if len(c.SiteSlugs) <= siteSlugCap {
			return
		}
		c.retireLocked(c.SiteSlugs[key])
		delete(c.SiteSlugs, key)
	}
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

// AIEditEnabled reports whether AI editing is on, under the lock. A missing setting
// reads as on; only an explicit false turns it off.
func (c *Config) AIEditEnabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.AIEdit == nil || c.AIEdit.Enabled == nil || *c.AIEdit.Enabled
}

// SetAIEditEnabled records the AI editing switch under the lock, keeping the
// default engine and user engines.
func (c *Config) SetAIEditEnabled(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.AIEdit == nil {
		c.AIEdit = &AIEditSettings{}
	}
	c.AIEdit.Enabled = &v
}

// AIEditEngines returns the default engine name and a copy of the user engines,
// under the lock. The copy is the caller's to keep.
func (c *Config) AIEditEngines() (string, map[string][]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.AIEdit == nil {
		return "", nil
	}
	engines := make(map[string][]string, len(c.AIEdit.Engines))
	for name, argv := range c.AIEdit.Engines {
		engines[name] = append([]string(nil), argv...)
	}
	return c.AIEdit.Default, engines
}

// Profile is the app-wide person for files opened with HTML Clay: a random id and a
// display name, shared with documents only while Enabled.
type Profile struct {
	Enabled bool   `json:"enabled"`
	ID      string `json:"id,omitempty"`
	Name    string `json:"name,omitempty"`
}

var (
	profileIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)
	emailPattern     = regexp.MustCompile(`\S+@\S+\.\S+`)
)

// The same rules ClayJS applies to a host's person, so a name the app accepts is
// never silently replaced by the browser's own prompt.
const maxProfileName = 120

var (
	ErrProfileNameEmpty   = errors.New("Enter a name.")
	ErrProfileNameLong    = errors.New("Use a name of 120 characters or fewer.")
	ErrProfileNameEmail   = errors.New("Use a name, not an email address.")
	ErrProfileIncomplete  = errors.New("A profile needs an id and a name before it can be used.")
	ErrProfileIDMalformed = errors.New("The profile id is damaged.")
)

// isJSSpace is JavaScript's whitespace set (String.prototype.trim and \s), so a name
// HTML Clay accepts is one ClayJS accepts too.
func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ',
		0x00a0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// CleanProfileName trims and collapses whitespace and checks the result.
func CleanProfileName(name string) (string, error) {
	clean := strings.Join(strings.FieldsFunc(name, isJSSpace), " ")
	switch {
	case clean == "":
		return "", ErrProfileNameEmpty
	case len(utf16.Encode([]rune(clean))) > maxProfileName:
		return "", ErrProfileNameLong
	case emailPattern.MatchString(clean):
		return "", ErrProfileNameEmail
	}
	return clean, nil
}

// ValidProfileID reports whether id has the shape ClayJS accepts for a person.
func ValidProfileID(id string) bool {
	return profileIDPattern.MatchString(id)
}

// NewProfileID mints a person id: 16 random bytes as unpadded base64url.
func NewProfileID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Shareable reports whether this profile may be given to documents: on, with an id
// and a name that pass the same checks the setter applies.
func (p Profile) Shareable() bool {
	if !p.Enabled || !profileIDPattern.MatchString(p.ID) {
		return false
	}
	name, err := CleanProfileName(p.Name)
	return err == nil && name == p.Name
}

// ProfileState returns a copy of the profile under the lock; the zero value when
// none was ever set up.
func (c *Config) ProfileState() Profile {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Profile == nil {
		return Profile{}
	}
	return *c.Profile
}

// SharedPerson is the person documents see: ok only while sharing is on and the
// stored record passes the same checks the setter applies.
func (c *Config) SharedPerson() (id, name string, ok bool) {
	p := c.ProfileState()
	if !p.Shareable() {
		return "", "", false
	}
	return p.ID, p.Name, true
}

// UpdateProfile validates next, writes it to disk, and only then lets readers see it. A
// field the change leaves as it is stored is not re-checked, so a damaged record can
// always be turned off. Turning sharing on needs a sound id and name. A failed write
// leaves the previous profile in memory and on disk.
func (c *Config) UpdateProfile(next Profile) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var current Profile
	if c.Profile != nil {
		current = *c.Profile
	}
	if next.Name != current.Name || (next.Enabled && !current.Enabled) {
		if next.Name != "" || next.Enabled {
			name, err := CleanProfileName(next.Name)
			if err != nil {
				return err
			}
			next.Name = name
		}
	}
	if (next.ID != current.ID || (next.Enabled && !current.Enabled)) && next.ID != "" && !ValidProfileID(next.ID) {
		return ErrProfileIDMalformed
	}
	if next.Enabled && (next.ID == "" || next.Name == "") {
		return ErrProfileIncomplete
	}
	previous := c.Profile
	c.Profile = &next
	if err := c.saveLocked(); err != nil {
		c.Profile = previous
		return err
	}
	return nil
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
	// PinnedIdentities is true when trusted folders that carried no fingerprint
	// gained one, which is what a config written by a Windows build from before
	// DirIdentity answered there looks like.
	PinnedIdentities bool
	// DroppedHelperPrograms and DroppedHelperDecisions report malformed,
	// duplicate, dangling, or over-cap helper records removed during Load.
	DroppedHelperPrograms  int
	DroppedHelperDecisions int
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
	// After the dedupe, so a pin is never derived for an entry that is about to
	// be dropped as an alias of one already kept.
	res.PinnedIdentities = cfg.backfillIdentities(identity)
	res.DroppedHelperPrograms, res.DroppedHelperDecisions = cfg.normalizeHelpers()
	cfg.capSitePorts()
	cfg.retireNamedSlugs()
	cfg.dedupeSiteSlugs()
	cfg.capSiteSlugs()
	return cfg, res, nil
}

func (c *Config) Save() error {
	// Hold the lock across the marshal so a concurrent SitePorts/TrustedFolders
	// mutation cannot tear the snapshot or panic the map iteration. The disk write
	// stays under the lock too, which serialises Saves and prevents two writers'
	// temp-rename races from resurrecting a just-removed entry.
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.saveLocked()
}

// saveLocked writes the config. The caller holds c.mu.
func (c *Config) saveLocked() error {
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
