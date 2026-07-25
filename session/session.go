package session

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
)

// ErrOutsideHome is returned by Register when a file resolves to a path outside
// the user's home directory. Callers use it to surface a clear user-facing error.
var ErrOutsideHome = errors.New("path is outside home directory")

type File struct {
	Token   string
	AbsPath string
	RelPath string
	Name    string

	writeMu sync.Mutex

	// There are exactly two per-file records, and nothing may introduce a third.
	// Both are hex sha256 of on-disk bytes, and both are read and written only
	// under Lock().
	//
	//   lastServerWrite        set by save, restore, htmlclayid injection, and the
	//                          first observation of a file. Never set by serving a
	//                          page or by the watcher. Used for stale-write
	//                          detection.
	//   lastStableObservation  set by the watcher confirming a stable read, and by
	//                          every server write. Never set by serving a page.
	//                          Used for change broadcast and for suppressing our
	//                          own writes.
	//
	// lastServerWrite ignores serves on purpose: if serving advanced it, tab A
	// could load H0, an editor write H1, tab B load H1 and advance the record, and
	// tab A's later save would compare H1 against H1, find no mismatch, and
	// silently destroy H1.
	lastServerWrite       string
	lastStableObservation string

	// historyKey is not a third record. It is this file's immutable backup
	// identity, resolved exactly once and never re-derived from mutable disk
	// bytes. Deriving it per request meant that an external process deleting the
	// file or stripping its htmlclayid silently moved the key to a path hash, so
	// the versions API listed and restored nothing while the id-keyed backups sat
	// on disk.
	historyKey string
}

// Lock and Unlock serialize read-modify-write operations on this file (saves,
// restores, and on-serve htmlclayid injection) so concurrent handlers cannot
// clobber each other. They also guard the two per-file records.
func (f *File) Lock()   { f.writeMu.Lock() }
func (f *File) Unlock() { f.writeMu.Unlock() }

// LastServerWrite returns the hash of the bytes this server last wrote.
// Caller must hold Lock().
func (f *File) LastServerWrite() string { return f.lastServerWrite }

// LastStableObservation returns the hash of the content last confirmed stable on
// disk. Caller must hold Lock().
func (f *File) LastStableObservation() string { return f.lastStableObservation }

// HistoryKey returns this file's resolved backup identity, or "" if it has not
// been resolved yet. Caller must hold Lock().
func (f *File) HistoryKey() string { return f.historyKey }

// SetHistoryKey records the backup identity the first time it is resolved and
// then refuses to move it. Every list, read, restore and save path reads the
// stored key rather than recomputing one from whatever is on disk right now.
// Caller must hold Lock().
func (f *File) SetHistoryKey(key string) {
	if f.historyKey == "" {
		f.historyKey = key
	}
}

// Observed reports whether this server has ever written or first-observed this
// file, which is what makes the very first save comparable.
//
// It is derived from lastServerWrite rather than stored, deliberately. As a
// stored flag it was a third per-file record, and a load-bearing one: it gates
// clone resolution and the first-open snapshot. The watcher could set it for a
// file that had never been served (an origin-trusted SSE subscription may name
// any registered path), and that file's first real GET then skipped both. Since
// the normative table says lastServerWrite is never set by the watcher and never
// set by serving a page, deriving from it makes the watcher structurally unable
// to mark a file observed. Caller must hold Lock().
func (f *File) Observed() bool { return f.lastServerWrite != "" }

// RecordServerWrite advances both records. Caller must hold Lock().
func (f *File) RecordServerWrite(hash string) {
	f.lastServerWrite = hash
	f.lastStableObservation = hash
}

// RecordStableObservation advances only the stable-observation record, which is
// what the watcher does after confirming a stable external read. Caller must hold
// Lock().
func (f *File) RecordStableObservation(hash string) {
	f.lastStableObservation = hash
}

// NoteFirstObservation seeds both records the first time a file is read, so the
// first save of a file the server has never written does not false-positive as a
// stale write. It reports whether this was the first observation. Caller must
// hold Lock().
func (f *File) NoteFirstObservation(hash string) bool {
	if f.Observed() {
		return false
	}
	f.RecordServerWrite(hash)
	return true
}

// rootKind distinguishes how a read root came to exist. It carries no
// authorization difference (a page may read through either) and exists so the
// tray can label roots and so persistence only ever writes granted ones.
type rootKind int

const (
	rootOpened  rootKind = iota // dir of a file the user explicitly opened
	rootGranted                 // dir the user approved via the permission dialog
	rootTrusted                 // dir the user marked trusted; seeded for anchors inside it
)

// readRoot is a directory a session's page is allowed to read. Reads are
// authorized by the presence of a readRoot, never by "a file happens to sit in
// this folder"; registration installs an opened root, grants install granted
// ones, and the two are otherwise identical.
//
// Provenance is independent flags rather than one kind, because a directory can
// be more than one thing at once: the folder of an explicitly opened file, the
// target of a grant, and inside a trusted folder. Collapsing them lost that:
// revoking a grant deleted the whole entry and took away the capability the open
// (or trust) had created, silently 404ing the opened page's own siblings.
type readRoot struct {
	path      string
	opened    bool
	granted   bool
	trusted   bool // seeded from a TrustedFolders entry this anchor lives inside
	persisted bool // AllowAlways: this grant is written to config and restored on next open
	// root is the held capability, opened when the root is installed and closed
	// when it is revoked. Reads go through it rather than re-opening the directory
	// per request, so a component swapped for a symlink between authorization and
	// open cannot escape. Always non-nil for an installed root.
	root *os.Root
}

type Manager struct {
	mu        sync.RWMutex
	byToken   map[string]*File
	byPath    map[string]string
	readRoots map[string]*readRoot
	homeDir   string
	// guard, when set, reports directories that must never become a granted
	// read root (the config dir and the versions/backup dir). It is injected by
	// the runtime, which knows those paths; the session manager does not.
	guard func(dir string) bool
}

func NewManager() (*Manager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("cannot determine home directory: %w", err)
	}
	return NewManagerWithHome(home), nil
}

// normalizeHome resolves symlinks in the home directory so the path-prefix
// check in resolveAndValidate matches symlink-resolved file paths. Without this,
// a home dir located under a symlinked path would reject every file.
func normalizeHome(homeDir string) string {
	if resolved, err := filepath.EvalSymlinks(homeDir); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(homeDir)
}

func NewManagerWithHome(homeDir string) *Manager {
	return &Manager{
		byToken:   make(map[string]*File),
		byPath:    make(map[string]string),
		readRoots: make(map[string]*readRoot),
		homeDir:   normalizeHome(homeDir),
	}
}

// SetGuard installs the predicate that vetoes granted read roots. Call it once,
// at construction, before the manager serves any request.
func (m *Manager) SetGuard(guard func(dir string) bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.guard = guard
}

func (m *Manager) HomeDir() string {
	return m.homeDir
}

// caseInsensitiveFS reports whether the host platform's default filesystem
// ignores case (Windows and macOS). On those, two paths that differ only in case
// name the same file, so home-containment checks must fold case.
func caseInsensitiveFS() bool {
	return runtime.GOOS == "windows" || runtime.GOOS == "darwin"
}

// ContainWithinHome reports whether child is strictly inside home. When it is, it
// returns child rebuilt with home's exact casing on the prefix, so derived
// relative paths and map keys stay stable no matter how child was cased (e.g. a
// lowercase Windows drive letter, or macOS symlink resolution that preserves
// input casing). The containment test folds case on case-insensitive platforms
// and is exact on case-sensitive ones (Linux).
func ContainWithinHome(home, child string) (string, bool) {
	prefix := home + string(os.PathSeparator)
	if len(child) < len(prefix) {
		return "", false
	}
	head, rest := child[:len(prefix)], child[len(prefix):]
	if caseInsensitiveFS() {
		if !strings.EqualFold(head, prefix) {
			return "", false
		}
	} else if head != prefix {
		return "", false
	}
	return prefix + rest, true
}

// EqualOrUnder reports whether child is parent or sits below it, folding case on
// the platforms whose filesystem ignores it.
//
// It exists so forbidden-root guards cannot disagree with ContainWithinHome
// about whether two spellings name the same directory. A plain byte-wise
// strings.HasPrefix is wrong here: filepath.EvalSymlinks preserves the caller's
// spelling of non-symlink components, so on macOS the same directory reaches a
// guard as both "Library/Application Support" and "library/application support"
// and a case-sensitive test waves the second one through.
func EqualOrUnder(child, parent string) bool {
	c, p := filepath.Clean(child), filepath.Clean(parent)
	if caseInsensitiveFS() {
		c, p = strings.ToLower(c), strings.ToLower(p)
	}
	if c == p {
		return true
	}
	return strings.HasPrefix(c, p+string(os.PathSeparator))
}

// SamePathComponent reports whether two single path components name the same
// thing, folding case on case-insensitive platforms. It exists so the broker's LCA
// computation agrees with the rest of the authorization code about casing: a
// byte-wise comparison split one real directory into two on macOS and could grant a
// broader ancestor than the assets needed.
func SamePathComponent(a, b string) bool {
	if caseInsensitiveFS() {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func resolveAndValidate(absPath, homeDir string) (string, error) {
	cleaned, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return "", fmt.Errorf("cannot resolve path %q: %w", absPath, err)
	}
	cleaned = filepath.Clean(cleaned)

	canonical, ok := ContainWithinHome(homeDir, cleaned)
	if !ok {
		return "", fmt.Errorf("path %q is outside home directory: %w", cleaned, ErrOutsideHome)
	}

	return canonical, nil
}

func generateToken() (string, error) {
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", fmt.Errorf("cannot generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(tokenBytes), nil
}

func (m *Manager) Register(absPath string) (*File, error) {
	cleaned, err := resolveAndValidate(absPath, m.homeDir)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if token, ok := m.byPath[cleaned]; ok {
		return m.byToken[token], nil
	}

	token, err := generateToken()
	if err != nil {
		return nil, err
	}

	relPath, err := filepath.Rel(m.homeDir, cleaned)
	if err != nil {
		return nil, fmt.Errorf("cannot compute relative path: %w", err)
	}

	f := &File{
		Token:   token,
		AbsPath: cleaned,
		RelPath: relPath,
		Name:    filepath.Base(cleaned),
	}

	m.byToken[token] = f
	m.byPath[cleaned] = token
	// Opening a file grants its page read access to that file's own folder,
	// nothing more. installReadRoot refuses the home directory itself, so a file
	// opened loose in ~ never exposes the whole home tree. A failure to open the
	// capability handle here is silently tolerated: the file still serves and
	// saves (that path reads f.AbsPath directly); only its sibling assets, which
	// go through the handle, would 404, and the directory we just resolved a file
	// in cannot realistically fail to open.
	_ = m.installReadRoot(filepath.Dir(cleaned), rootOpened, false)
	return f, nil
}

// installReadRoot records dir as a readable root with the given provenance and,
// for a new root, opens its capability handle. Caller must hold m.mu. The home
// directory is never installed: a page must not be able to read the entire home
// tree. Provenance accumulates rather than replaces, so a dir that is both opened
// and granted carries both, and AllowAlways is sticky once set. Returns an error
// only when a new root's handle cannot be opened; an existing root never fails.
func (m *Manager) installReadRoot(dir string, kind rootKind, persisted bool) error {
	if dir == m.homeDir {
		return nil
	}
	rr, ok := m.readRoots[dir]
	if !ok {
		root, err := os.OpenRoot(dir)
		if err != nil {
			return err
		}
		rr = &readRoot{path: dir, root: root}
		m.readRoots[dir] = rr
	}
	switch kind {
	case rootOpened:
		rr.opened = true
	case rootGranted:
		rr.granted = true
		if persisted {
			rr.persisted = true
		}
	case rootTrusted:
		rr.trusted = true
	}
	return nil
}

// GrantReadRoot widens a session's reads to dir (read-only), the operation a
// granted permission performs. persisted marks an AllowAlways grant for the
// config writer to pick up.
func (m *Manager) GrantReadRoot(dir string, persisted bool) error {
	return m.installValidatedRoot(dir, rootGranted, persisted)
}

// InstallTrustedRoot installs an ALREADY-canonical trusted folder (resolved and
// home-validated by the caller at trust time) as a silent, read-only capability
// over its whole tree, with its own provenance so untrusting never disturbs a root
// an open or a grant also created.
//
// It deliberately does NOT re-resolve symlinks. The caller's stored path is the
// folder's identity; re-running EvalSymlinks on it could, if a component were
// swapped for a symlink in between, open a different tree under a key that no
// longer matches the stored path and so could never be revoked. The path is still
// re-validated (inside home, no hidden component, not guard-vetoed) so a stale or
// tampered config entry can never install a forbidden root.
func (m *Manager) InstallTrustedRoot(canonical string) error {
	return m.installCanonicalRoot(canonical, rootTrusted, false)
}

// installValidatedRoot resolves symlinks in dir, then installs the resolved path.
// Used by GrantReadRoot, whose dir arrives fresh from a live request rather than
// from persisted state.
func (m *Manager) installValidatedRoot(dir string, kind rootKind, persisted bool) error {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("cannot resolve read root %q: %w", dir, err)
	}
	return m.installCanonicalRoot(filepath.Clean(resolved), kind, persisted)
}

// installCanonicalRoot installs an already symlink-resolved path without touching
// the filesystem to re-resolve it. It requires the path strictly inside home (so
// home itself is refused), with no hidden (dot-prefixed) component and not vetoed
// by the guard (config/versions), then opens the os.Root capability and records it
// under its ContainWithinHome-normalized path. That normalization is idempotent on
// an already-canonical input, so a caller that stored the same canonical string
// can always revoke the root by that string. Shared by GrantReadRoot (via
// installValidatedRoot) and InstallTrustedRoot so every gate stays identical.
func (m *Manager) installCanonicalRoot(canonical string, kind rootKind, persisted bool) error {
	c, ok := ContainWithinHome(m.homeDir, canonical)
	if !ok {
		return fmt.Errorf("read root %q is outside home directory: %w", canonical, ErrOutsideHome)
	}
	if m.hasHiddenComponent(c) {
		return fmt.Errorf("read root %q contains a hidden component", c)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.guard != nil && m.guard(c) {
		return fmt.Errorf("read root %q is a forbidden root", c)
	}
	if err := m.installReadRoot(c, kind, persisted); err != nil {
		return fmt.Errorf("cannot open read root %q: %w", c, err)
	}
	return nil
}

// RevokeReadRoot withdraws a grant (used by the tray's per-root revoke). A root
// that also exists because the user explicitly opened a file in it, or because it
// was seeded from a trusted folder, survives with that provenance intact:
// revoking a grant must never take away the capability an open or trust created.
func (m *Manager) RevokeReadRoot(dir string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rr, ok := m.readRoots[dir]
	if !ok {
		return
	}
	rr.granted = false
	rr.persisted = false
	if !rr.opened && !rr.trusted {
		if rr.root != nil {
			rr.root.Close()
		}
		delete(m.readRoots, dir)
	}
}

// RevokeTrustedRoot withdraws a seeded trusted root when the user untrusts its
// folder in the tray. dir must be the canonical trusted-folder path (the same
// value InstallTrustedRoot keyed on). A root the open or a grant also created
// survives with that provenance intact.
func (m *Manager) RevokeTrustedRoot(dir string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rr, ok := m.readRoots[dir]
	if !ok {
		return
	}
	rr.trusted = false
	if !rr.opened && !rr.granted {
		if rr.root != nil {
			rr.root.Close()
		}
		delete(m.readRoots, dir)
	}
}

// HasHiddenComponent reports whether any component of path below home starts with
// a dot. It keeps grants and asset reads away from ~/.ssh, ~/.git, ~/.config, and
// the like.
func HasHiddenComponent(home, path string) bool {
	rel, err := filepath.Rel(home, path)
	if err != nil {
		return true
	}
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}

func (m *Manager) hasHiddenComponent(path string) bool {
	return HasHiddenComponent(m.homeDir, path)
}

// CanGrant reports whether dir could be granted as a read root: strictly inside
// home, no hidden component, and not vetoed by the guard (config/versions tree). The
// broker calls this before prompting so it never raises a dialog for a folder that
// GrantReadRoot would then refuse (e.g. macOS ~/Library, which swallows the config
// dir). It does not resolve symlinks; the broker's dir is an already-resolved LCA,
// and GrantReadRoot re-validates on the actual grant.
func (m *Manager) CanGrant(dir string) bool {
	if _, ok := ContainWithinHome(m.homeDir, dir); !ok {
		return false
	}
	if m.hasHiddenComponent(dir) {
		return false
	}
	m.mu.RLock()
	guard := m.guard
	m.mu.RUnlock()
	return guard == nil || !guard(dir)
}

func (m *Manager) Lookup(token string) (*File, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	f, ok := m.byToken[token]
	return f, ok
}

func (m *Manager) LookupByPath(absPath string) (*File, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	token, ok := m.byPath[absPath]
	if !ok {
		return nil, false
	}
	return m.byToken[token], true
}

// AssetRoot returns the read root that authorizes absPath and absPath's path
// relative to that root, in the root's canonical casing. A read root is either
// the folder of an opened file or a folder the user granted. When roots nest,
// the MOST SPECIFIC (longest) containing root wins, so the result is
// deterministic regardless of map order.
func (m *Manager) AssetRoot(absPath string) (root, rel string, ok bool) {
	root, rel, _, ok = m.assetRoot(absPath)
	return root, rel, ok
}

// AssetRootOpened is AssetRoot plus whether the winning root exists because the
// user explicitly opened a file in it, rather than only because of a grant.
// Site routing prefers an opened root so a read-only grant never pulls a later
// explicit open into the granting origin, where an already-running page could
// lift the new file's save token.
func (m *Manager) AssetRootOpened(absPath string) (root, rel string, opened, ok bool) {
	return m.assetRoot(absPath)
}

func (m *Manager) assetRoot(absPath string) (root, rel string, opened, ok bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	best, bestRel := m.bestRootLocked(absPath)
	if best == nil {
		return "", "", false, false
	}
	return best.path, bestRel, best.opened, true
}

// bestRootLocked returns the most specific read root containing absPath and
// absPath relative to it. Caller holds m.mu (read or write).
func (m *Manager) bestRootLocked(absPath string) (*readRoot, string) {
	var best *readRoot
	var bestRel string
	for _, rr := range m.readRoots {
		if canonical, hit := ContainWithinHome(rr.path, absPath); hit {
			if best == nil || len(rr.path) > len(best.path) {
				best, bestRel = rr, canonical[len(rr.path)+1:]
			}
		}
	}
	return best, bestRel
}

// OpenAsset opens absPath for reading through the held capability of the most
// specific read root that authorizes it, and reports whether any root authorized
// it at all. The returned *os.File is an independent descriptor the caller closes;
// the root handle stays owned by the manager. The open happens under the read
// lock, so RevokeReadRoot / RevokeAll (write lock) cannot close the handle
// mid-open, and reading through the handle keeps a directory component swapped for
// a symlink after authorization from escaping the root.
func (m *Manager) OpenAsset(absPath string) (file *os.File, authorized bool, err error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	best, rel := m.bestRootLocked(absPath)
	if best == nil {
		return nil, false, nil
	}
	f, err := best.root.Open(rel)
	return f, true, err
}

// RootInfo is a snapshot of one installed read root, for the tray to display and
// manage. The provenance flags are independent (a dir can be more than one).
type RootInfo struct {
	Path    string
	Opened  bool
	Granted bool
	Trusted bool
}

// ReadRoots returns a snapshot of the installed read roots, sorted by path for
// stable display.
func (m *Manager) ReadRoots() []RootInfo {
	m.mu.RLock()
	out := make([]RootInfo, 0, len(m.readRoots))
	for _, rr := range m.readRoots {
		out = append(out, RootInfo{Path: rr.path, Opened: rr.opened, Granted: rr.granted, Trusted: rr.trusted})
	}
	m.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func (m *Manager) RevokeAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, rr := range m.readRoots {
		if rr.root != nil {
			rr.root.Close()
		}
	}
	m.byToken = make(map[string]*File)
	m.byPath = make(map[string]string)
	m.readRoots = make(map[string]*readRoot)
}
