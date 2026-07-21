// Package versions stores plain, uncompressed backups of malleable HTML files.
//
// Layout:
//
//	<UserConfigDir>/htmlclay/versions/            0700
//	    notes-a3f19c2b/                           <basename>-<8 hex display suffix>
//	        2026-07-19-14-22-08-431-0400.html     0600, uncompressed, local + offset
//	        meta.json                             {name, absPath, key, updatedAt}
//
// A version filename carries LOCAL wall time so it reads correctly in a file
// browser, followed by the signed UTC offset that was in force at that moment.
// The offset is what makes the name a single instant: local wall time repeats
// for one hour on every DST fall-back, and this is a delete path, so a name that
// cannot be ordered is a name that can cost the user the version they wanted.
//
// The 8 hex characters in the folder name are a display affordance only. The
// logical key is the full UUID or the full path hash, recorded in meta.json, and
// two distinct keys sharing a display prefix never share a history.
//
// Containment is structural, not lexical. Every mutation runs through an *os.Root
// opened for the duration of one locked operation: the versions directory is
// acquired without trusting its final component, each history is reopened with an
// Lstat + OpenRoot + os.SameFile check, and deletes, links, renames and reads all
// go through the opened root. A history directory swapped for a symlink between a
// path check and a syscall can therefore no longer redirect a delete outside the
// store.
package versions

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/panphora/htmlclay/htmlutil"
)

const (
	// MaxAge is the age past which a version is eligible for deletion.
	MaxAge = 60 * 24 * time.Hour
	// MinKeep is the number of newest versions always retained per history,
	// regardless of age. Pruning retains the union of the two rules.
	MinKeep = 20
	// pruneInterval throttles opportunistic pruning to at most once per hour
	// per history.
	pruneInterval = time.Hour
	// maxEntrySize bounds a version read back off disk during a restore.
	maxEntrySize = 50 * 1024 * 1024
)

var (
	// ErrNoHistory is returned when a key has no history folder on disk.
	ErrNoHistory = errors.New("no version history for this file")
	// ErrBadName is returned when a version name is not exactly one generated
	// filename.
	ErrBadName = errors.New("invalid version name")
	// ErrNotRegular is returned when a version path is not a regular file.
	ErrNotRegular = errors.New("version is not a regular file")
)

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// entryRe matches exactly one generated version filename: a timestamp with
// millisecond precision, its zone, and an optional zero-padded numeric collision
// suffix. The zone is either a signed four-digit UTC offset (current form) or a
// bare `Z` (the earlier all-UTC form, still on disk and still readable). Both are
// exact instants. The offset group is fixed width and positional, so `431-0400`
// splits into millis `431` and offset `-0400` with nothing left to guess at.
var entryRe = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})-(\d{2})-(\d{2})-(\d{2})-(\d{3})(Z|[+-]\d{4})(?:-(\d{2,}))?\.html$`)

var unsafeDisplayRe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// IsCanonicalUUID reports whether id is a canonical 8-4-4-4-12 hex UUID.
// ReadHTMLClayID returns whatever is in the attribute unvalidated, so a
// hand-edited file can carry `..` or a short string; only a canonical id is
// trusted as a history key.
func IsCanonicalUUID(id string) bool {
	return uuidRe.MatchString(id)
}

// Key returns the logical history key for a file.
//
// A .htmlclay file keys by its htmlclayid, but only after that id validates as a
// canonical UUID. A plain .html file never receives an injected id, so it keys by
// a hash of its absolute path. That key does not survive a move; the same
// fallback applies to a .htmlclay file carrying an invalid id.
func Key(absPath string, data []byte) string {
	if strings.EqualFold(filepath.Ext(absPath), ".htmlclay") {
		if id := htmlutil.ReadHTMLClayID(data); IsCanonicalUUID(id) {
			return "id:" + strings.ToLower(id)
		}
	}
	sum := sha256.Sum256([]byte(absPath))
	return "path:" + hex.EncodeToString(sum[:])
}

// IDFromKey returns the canonical UUID an id: history key carries, or false for a
// path: key. It is the inverse of the id: half of Key, so restore can honor the
// stored identity without re-parsing the key grammar inline.
func IDFromKey(key string) (string, bool) {
	id, ok := strings.CutPrefix(key, "id:")
	if !ok || !IsCanonicalUUID(id) {
		return "", false
	}
	return strings.ToLower(id), true
}

// Hash returns the hex sha256 of data, the form used for both per-file records.
func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Entry is one stored version.
type Entry struct {
	Name string    `json:"name"`
	Time time.Time `json:"time"`
	Seq  int       `json:"seq"`
	Size int64     `json:"size"`
}

func (e Entry) before(other Entry) bool {
	if e.Time.Equal(other.Time) {
		return e.Seq < other.Seq
	}
	return e.Time.Before(other.Time)
}

type meta struct {
	Name      string `json:"name"`
	AbsPath   string `json:"absPath"`
	Key       string `json:"key"`
	UpdatedAt string `json:"updatedAt"`
}

type history struct {
	folder  string
	absPath string
}

// Store owns the versions directory. Every exported method takes the store lock.
// The lock hierarchy is one rule: callers acquire session.File's lock BEFORE the
// store lock, never the reverse.
type Store struct {
	mu sync.Mutex
	// parentDir is the canonical (symlink-resolved) parent of the versions
	// directory; leaf is its literal final component. They are kept separate so
	// the final component is never trusted as already-resolved: a planted
	// `versions` symlink must be rejected, not adopted as the root.
	parentDir string
	leaf      string
	// baseDir is parentDir joined with leaf, used only for the HTTP asset-denial
	// classification in Contains. It is not store containment.
	baseDir   string
	index     map[string]history
	loaded    bool
	lastPrune map[string]time.Time
}

// New returns a store rooted at baseDir, which is created lazily on first write.
func New(baseDir string) *Store {
	parent, leaf := resolveBase(baseDir)
	return &Store{
		parentDir: parent,
		leaf:      leaf,
		baseDir:   filepath.Join(parent, leaf),
		index:     make(map[string]history),
		lastPrune: make(map[string]time.Time),
	}
}

// resolveBase canonicalizes the versions directory's existing parent while
// retaining the lexical final component. The internal-directory denial compares
// symlink-resolved request paths, so the parent is resolved for that match; the
// leaf is deliberately NOT resolved, because EvalSymlinks over the full base
// would turn a planted `versions` symlink into the trusted root and erase the
// evidence this store is meant to reject.
func resolveBase(baseDir string) (parent, leaf string) {
	cleaned := filepath.Clean(baseDir)
	parent, leaf = filepath.Dir(cleaned), filepath.Base(cleaned)
	if resolved, err := filepath.EvalSymlinks(parent); err == nil {
		parent = filepath.Clean(resolved)
	}
	return parent, leaf
}

// BaseDir returns the resolved versions directory without creating it.
func (s *Store) BaseDir() string {
	return s.baseDir
}

// Dir returns the versions directory, creating it if needed. Used by the tray
// item so "Backups" opens something that exists.
func (s *Store) Dir() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	root, err := s.openVersionsRoot(true)
	if err != nil {
		return "", err
	}
	root.Close()
	return s.baseDir, nil
}

// Contains reports whether absPath sits inside the versions directory. The server
// uses it to deny requests for internal backup state on the app's own origin: the
// config directory lives under the user's home on every platform, so the static
// path would otherwise be reachable.
func (s *Store) Contains(absPath string) bool {
	return s.contain(absPath) == nil
}

// contain rechecks that child sits strictly inside the versions directory. It
// answers the HTTP asset-denial question only, where an absolute request path
// must be classified. It is NOT store containment; that is now structural, via
// os.Root.
//
// The prefix test folds case on the platforms whose default filesystem ignores
// it, so a request differing only in the casing of an interior segment cannot
// slip past the internal-directory denial.
func (s *Store) contain(child string) error {
	cleaned := filepath.Clean(child)
	prefix := s.baseDir + string(os.PathSeparator)
	if len(cleaned) <= len(prefix) {
		return fmt.Errorf("path %q escapes versions directory", cleaned)
	}
	head := cleaned[:len(prefix)]
	if caseInsensitiveFS() {
		if !strings.EqualFold(head, prefix) {
			return fmt.Errorf("path %q escapes versions directory", cleaned)
		}
		return nil
	}
	if head != prefix {
		return fmt.Errorf("path %q escapes versions directory", cleaned)
	}
	return nil
}

// caseInsensitiveFS reports whether the host platform's default filesystem
// ignores case (Windows and macOS).
func caseInsensitiveFS() bool {
	return runtime.GOOS == "windows" || runtime.GOOS == "darwin"
}

// samePath compares two absolute paths, folding case on the platforms whose
// default filesystem ignores it.
func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if caseInsensitiveFS() {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// randSuffix returns a random hex string for a hidden same-directory temp name.
func randSuffix() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}

// readDirNames lists name's entries beneath root without following symlinks.
func readDirNames(root *os.Root, name string) ([]os.DirEntry, error) {
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.ReadDir(-1)
}

// openVerifiedChild opens name beneath root as a new *os.Root, rejecting a
// symlink, a non-directory, or an ABA swap between the pre-open Lstat and the
// post-open fstat. This Lstat/open/fstat sequence is what makes containment
// structural: os.Root refuses to traverse the symlink, and os.SameFile catches a
// directory swapped after the check. The caller closes the returned root.
func openVerifiedChild(root *os.Root, name string, create bool) (*os.Root, error) {
	info, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		if !create {
			return nil, err
		}
		if mErr := root.Mkdir(name, 0700); mErr != nil && !errors.Is(mErr, fs.ErrExist) {
			return nil, mErr
		}
		info, err = root.Lstat(name)
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("%q is a symlink, not a directory", name)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%q is not a directory", name)
	}
	child, err := root.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	after, err := child.Stat(".")
	if err != nil {
		child.Close()
		return nil, err
	}
	if !os.SameFile(info, after) {
		child.Close()
		return nil, fmt.Errorf("%q changed identity during open", name)
	}
	return child, nil
}

// openVersionsRoot opens the versions directory as an *os.Root for one locked
// operation. The parent is already canonical; only the final component is checked
// at use time, so a symlink planted at `versions` cannot redirect a destructive
// syscall. The caller closes the returned root.
func (s *Store) openVersionsRoot(create bool) (*os.Root, error) {
	parentRoot, err := os.OpenRoot(s.parentDir)
	if err != nil {
		if create && errors.Is(err, fs.ErrNotExist) {
			if mErr := os.MkdirAll(s.parentDir, 0700); mErr != nil {
				return nil, mErr
			}
			parentRoot, err = os.OpenRoot(s.parentDir)
		}
		if err != nil {
			return nil, err
		}
	}
	defer parentRoot.Close()
	return openVerifiedChild(parentRoot, s.leaf, create)
}

// historyRef is an opened history directory. The root is the only mutation
// authority; folder is the relative name, kept for meta writes and rename logic.
type historyRef struct {
	folder string
	root   *os.Root
}

// Close releases the opened history root.
func (h *historyRef) Close() {
	if h != nil && h.root != nil {
		h.root.Close()
	}
}

func (s *Store) load(vroot *os.Root) error {
	if s.loaded {
		return nil
	}
	dirents, err := readDirNames(vroot, ".")
	if err != nil {
		return err
	}
	for _, d := range dirents {
		if !d.IsDir() {
			continue
		}
		data, err := vroot.ReadFile(filepath.Join(d.Name(), "meta.json"))
		if err != nil {
			continue
		}
		var m meta
		if json.Unmarshal(data, &m) != nil || m.Key == "" {
			continue
		}
		s.index[m.Key] = history{folder: d.Name(), absPath: m.AbsPath}
	}
	s.loaded = true
	return nil
}

func readMeta(root *os.Root) (meta, error) {
	var m meta
	data, err := root.ReadFile("meta.json")
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, err
	}
	return m, nil
}

// displayBase builds the human-readable folder stem: a sanitized basename plus
// eight hex characters derived from the logical key.
func displayBase(absPath, key string) string {
	name := filepath.Base(absPath)
	name = strings.TrimSuffix(name, filepath.Ext(name))
	name = unsafeDisplayRe.ReplaceAllString(name, "_")
	name = strings.Trim(name, "._-")
	if len(name) > 40 {
		name = name[:40]
	}
	if name == "" {
		name = "file"
	}
	sum := sha256.Sum256([]byte(key))
	return name + "-" + hex.EncodeToString(sum[:4])
}

// folderMatchesBase reports whether folder is already the right home for base:
// either base itself, or base with a numeric collision suffix.
//
// Matching on base rather than stripping a trailing -<digits> matters, because
// the eight hex display characters can themselves be all digits, and stripping
// those would make every poll look like a rename.
func folderMatchesBase(folder, base string) bool {
	if folder == base {
		return true
	}
	rest, ok := strings.CutPrefix(folder, base+"-")
	if !ok || rest == "" {
		return false
	}
	for _, c := range rest {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// freeFolder picks a folder name based on base that no other key already owns and
// that does not exist within the versions root.
func (s *Store) freeFolder(vroot *os.Root, base string) (string, error) {
	taken := make(map[string]struct{}, len(s.index))
	for _, h := range s.index {
		taken[h.folder] = struct{}{}
	}
	for i := 0; i < 10000; i++ {
		candidate := base
		if i > 0 {
			candidate = fmt.Sprintf("%s-%d", base, i+1)
		}
		if _, ok := taken[candidate]; ok {
			continue
		}
		if _, err := vroot.Lstat(candidate); errors.Is(err, fs.ErrNotExist) {
			return candidate, nil
		}
	}
	return "", errors.New("cannot find a free history folder name")
}

// openHistory locates the history for key within vroot, creating it or renaming
// it after a file rename, and returns an opened, verified history root. Lookup is
// by key first, so a renamed file keeps its single history instead of growing a
// second folder. It never returns an absolute path as mutation authority.
func (s *Store) openHistory(vroot *os.Root, key, absPath string, create bool) (*historyRef, error) {
	if err := s.load(vroot); err != nil {
		return nil, err
	}

	base := displayBase(absPath, key)

	if h, ok := s.index[key]; ok && h.folder != "" {
		folder := h.folder
		if !folderMatchesBase(folder, base) {
			if renamed, err := s.freeFolder(vroot, base); err == nil {
				if rErr := vroot.Rename(folder, renamed); rErr == nil {
					folder = renamed
				}
			}
		}
		h.folder = folder
		h.absPath = absPath
		s.index[key] = h

		root, err := openVerifiedChild(vroot, folder, create)
		if err != nil {
			if !create && errors.Is(err, fs.ErrNotExist) {
				return nil, ErrNoHistory
			}
			return nil, err
		}
		if create {
			if err := writeMeta(root, key, absPath); err != nil {
				root.Close()
				return nil, err
			}
		}
		return &historyRef{folder: folder, root: root}, nil
	}

	// Either the key is unknown, or Claim reserved it without materializing a
	// folder. Both become a real history only when something is written.
	if !create {
		return nil, ErrNoHistory
	}

	folder, err := s.freeFolder(vroot, base)
	if err != nil {
		return nil, err
	}
	root, err := openVerifiedChild(vroot, folder, true)
	if err != nil {
		return nil, err
	}
	if err := writeMeta(root, key, absPath); err != nil {
		root.Close()
		return nil, err
	}
	s.index[key] = history{folder: folder, absPath: absPath}
	return &historyRef{folder: folder, root: root}, nil
}

func writeMeta(root *os.Root, key, absPath string) error {
	m := meta{
		Name:      filepath.Base(absPath),
		AbsPath:   absPath,
		Key:       key,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return atomicPublishBytes(root, "meta.json", data)
}

// atomicPublishBytes writes data to a hidden same-directory temp beneath root and
// renames it over name. Opening the final name directly would leave a truncated
// but visible file after a crash.
func atomicPublishBytes(root *os.Root, name string, data []byte) error {
	tmpName := ".htmlclay-ver-" + randSuffix()
	tmp, err := root.OpenFile(tmpName, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			root.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := root.Rename(tmpName, name); err != nil {
		return err
	}
	committed = true
	// The bytes are already fsynced; the directory entry that names them is not
	// until the containing directory is synced too.
	return syncDirRoot(root)
}

// formatStamp renders local wall time followed by the signed UTC offset in force
// at that instant. The offset is always present and always signed, so UTC itself
// is written as +0000 rather than omitted.
func formatStamp(t time.Time) string {
	_, offset := t.Zone()
	sign := "+"
	if offset < 0 {
		sign = "-"
		offset = -offset
	}
	return fmt.Sprintf("%04d-%02d-%02d-%02d-%02d-%02d-%03d%s%02d%02d",
		t.Year(), int(t.Month()), t.Day(), t.Hour(), t.Minute(), t.Second(),
		t.Nanosecond()/int(time.Millisecond), sign, offset/3600, (offset%3600)/60)
}

// entryName renders a version filename. The collision suffix is zero-padded so
// -10 does not sort before -02.
func entryName(t time.Time, seq int) string {
	if seq == 0 {
		return formatStamp(t) + ".html"
	}
	return fmt.Sprintf("%s-%02d.html", formatStamp(t), seq)
}

// ParseEntryName validates a version filename and returns its ordering key. It
// accepts exactly one generated filename and nothing else.
func ParseEntryName(name string) (time.Time, int, error) {
	m := entryRe.FindStringSubmatch(name)
	if m == nil {
		return time.Time{}, 0, ErrBadName
	}
	n := func(s string) int { v, _ := strconv.Atoi(s); return v }
	loc := time.UTC
	if zone := m[8]; zone != "Z" {
		offset := n(zone[1:3])*3600 + n(zone[3:5])*60
		if zone[0] == '-' {
			offset = -offset
		}
		if offset != 0 {
			loc = time.FixedZone(zone, offset)
		}
	}
	t := time.Date(n(m[1]), time.Month(n(m[2])), n(m[3]), n(m[4]), n(m[5]), n(m[6]),
		n(m[7])*int(time.Millisecond), loc)
	seq := 0
	if m[9] != "" {
		seq = n(m[9])
	}
	return t, seq, nil
}

// listEntries returns the versions in the opened history root sorted oldest
// first, by parsed instant rather than by filename. Names carry a zone, so two
// versions written either side of a DST fall-back order correctly even though
// their wall times repeat.
func listEntries(root *os.Root) ([]Entry, error) {
	dirents, err := readDirNames(root, ".")
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(dirents))
	for _, d := range dirents {
		if d.IsDir() {
			continue
		}
		t, seq, err := ParseEntryName(d.Name())
		if err != nil {
			continue
		}
		var size int64
		if info, iErr := d.Info(); iErr == nil {
			if !info.Mode().IsRegular() {
				continue
			}
			size = info.Size()
		}
		entries = append(entries, Entry{Name: d.Name(), Time: t, Seq: seq, Size: size})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].before(entries[j]) })
	return entries, nil
}

// ClaimStatus is the outcome of Claim.
type ClaimStatus int

const (
	// ClaimOwned means absPath now owns key: it was unowned, or already bound
	// here. Nothing further is required of the caller.
	ClaimOwned ClaimStatus = iota
	// ClaimRenamed means key was bound to a different path that is definitively
	// gone, so this is a rename. The history has been rebound to absPath.
	ClaimRenamed
	// ClaimClone means key is still owned by a different path, so absPath is a
	// copy and the caller must fork it a fresh identity.
	ClaimClone
)

// Claim checks and claims ownership of key for absPath in one store transaction.
//
// Checking ownership and claiming it in two transactions let two copies of one
// file, first-opened concurrently, both see no owner: neither got a fresh id and
// both landed in a single logical history, whose folder then rebound to whichever
// ran last. Reserving the key here, before any folder exists, makes the second
// caller see an owner.
//
// Only a definitive not-exists means the old path is gone. Any other Lstat error,
// EACCES above all, means "still there", so a transient permission failure cannot
// rebind a history onto a clone.
func (s *Store) Claim(key, absPath string) (ClaimStatus, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	vroot, err := s.openVersionsRoot(true)
	if err != nil {
		return ClaimOwned, "", err
	}
	defer vroot.Close()
	if err := s.load(vroot); err != nil {
		return ClaimOwned, "", err
	}

	h, ok := s.index[key]
	if !ok {
		s.index[key] = history{absPath: absPath}
		return ClaimOwned, "", nil
	}
	if h.absPath == "" || samePath(h.absPath, absPath) {
		h.absPath = absPath
		s.index[key] = h
		return ClaimOwned, "", nil
	}

	bound := h.absPath
	if _, err := os.Lstat(bound); err != nil && errors.Is(err, fs.ErrNotExist) {
		h.absPath = absPath
		s.index[key] = h
		if h.folder == "" {
			return ClaimRenamed, bound, nil
		}
		hroot, err := openVerifiedChild(vroot, h.folder, false)
		if err != nil {
			return ClaimRenamed, bound, err
		}
		defer hroot.Close()
		return ClaimRenamed, bound, writeMeta(hroot, key, absPath)
	}
	return ClaimClone, bound, nil
}

// BoundPath returns the absolute path a key's history is currently bound to.
func (s *Store) BoundPath(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	vroot, err := s.openVersionsRoot(false)
	if err != nil {
		return "", false
	}
	defer vroot.Close()
	if err := s.load(vroot); err != nil {
		return "", false
	}
	h, ok := s.index[key]
	if !ok || h.absPath == "" {
		return "", false
	}
	return h.absPath, true
}

// HasHistory reports whether key already has at least one stored version. Used
// to decide whether a save is the first one for a file, which is when the
// existing on-disk content is versioned so the pre-Hyperclay state survives.
func (s *Store) HasHistory(key, absPath string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	vroot, err := s.openVersionsRoot(false)
	if err != nil {
		return false
	}
	defer vroot.Close()
	ref, err := s.openHistory(vroot, key, absPath, false)
	if err != nil {
		return false
	}
	defer ref.Close()
	entries, err := listEntries(ref.root)
	return err == nil && len(entries) > 0
}

// Rebind points an existing history at a new absolute path. Used when a file with
// a known id turns out to be a rename rather than a clone.
func (s *Store) Rebind(key, absPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	vroot, err := s.openVersionsRoot(false)
	if err != nil {
		return err
	}
	defer vroot.Close()
	if err := s.load(vroot); err != nil {
		return err
	}
	h, ok := s.index[key]
	if !ok {
		return ErrNoHistory
	}
	h.absPath = absPath
	s.index[key] = h
	hroot, err := openVerifiedChild(vroot, h.folder, false)
	if err != nil {
		return err
	}
	defer hroot.Close()
	return writeMeta(hroot, key, absPath)
}

// Backup publishes content as a new version of key's history, atomically.
// It reports whether a new version was actually written: identical content is
// deduped and reports false with a nil error.
func (s *Store) Backup(key, absPath string, content []byte) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	vroot, err := s.openVersionsRoot(true)
	if err != nil {
		return false, err
	}
	defer vroot.Close()
	ref, err := s.openHistory(vroot, key, absPath, true)
	if err != nil {
		return false, err
	}
	defer ref.Close()
	hroot := ref.root

	entries, err := listEntries(hroot)
	if err != nil {
		return false, err
	}

	want := Hash(content)
	if len(entries) > 0 {
		newest := entries[len(entries)-1]
		if existing, rErr := hroot.ReadFile(newest.Name); rErr == nil && Hash(existing) == want {
			return false, nil
		}
	}

	t := time.Now()
	seq := 0
	if len(entries) > 0 {
		newest := entries[len(entries)-1]
		candidate := Entry{Time: t, Seq: 0}
		// The recorded offset removes DST repetition but not a system clock
		// rollback. When the clock would produce a name sorting at or before the
		// newest entry, append after that entry instead of trusting the clock.
		if !newest.before(candidate) {
			t = newest.Time
			seq = newest.Seq + 1
		}
	}

	tmpName, err := writeTemp(hroot, content)
	if err != nil {
		return false, err
	}
	defer hroot.Remove(tmpName)

	for i := 0; i < 1000; i++ {
		final := entryName(t, seq)
		// Root.Link is both exclusive (EEXIST on collision) and atomic, so a crash
		// never leaves a half-written version under a real name.
		err = hroot.Link(tmpName, final)
		if err == nil {
			if sErr := syncDirRoot(hroot); sErr != nil {
				return true, sErr
			}
			if err := writeMeta(hroot, key, absPath); err != nil {
				return true, err
			}
			return true, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			break
		}
		seq++
	}

	// Filesystems without hard links fall back to reserve-then-rename.
	for i := 0; i < 1000; i++ {
		final := entryName(t, seq)
		if _, sErr := hroot.Lstat(final); sErr == nil {
			seq++
			continue
		}
		if rErr := hroot.Rename(tmpName, final); rErr != nil {
			return false, rErr
		}
		if sErr := syncDirRoot(hroot); sErr != nil {
			return true, sErr
		}
		if err := writeMeta(hroot, key, absPath); err != nil {
			return true, err
		}
		return true, nil
	}
	return false, fmt.Errorf("cannot publish version: %w", err)
}

// writeTemp writes content to a hidden same-directory temp beneath root and
// returns its relative name. The file is fsynced and closed before returning.
func writeTemp(root *os.Root, content []byte) (string, error) {
	name := ".htmlclay-ver-" + randSuffix()
	f, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(content); err != nil {
		f.Close()
		root.Remove(name)
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		root.Remove(name)
		return "", err
	}
	if err := f.Close(); err != nil {
		root.Remove(name)
		return "", err
	}
	return name, nil
}

// List returns key's versions, newest first.
func (s *Store) List(key, absPath string) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	vroot, err := s.openVersionsRoot(false)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer vroot.Close()

	ref, err := s.openHistory(vroot, key, absPath, false)
	if err != nil {
		if errors.Is(err, ErrNoHistory) || errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer ref.Close()

	entries, err := listEntries(ref.root)
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	return entries, nil
}

// Read returns the bytes of one version. name must be exactly one generated
// filename; the file is opened beneath the resolved history root through os.Root
// so a swapped symlink cannot escape, and must be a regular file.
func (s *Store) Read(key, absPath, name string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, _, err := ParseEntryName(name); err != nil {
		return nil, err
	}

	vroot, err := s.openVersionsRoot(false)
	if err != nil {
		return nil, err
	}
	defer vroot.Close()

	ref, err := s.openHistory(vroot, key, absPath, false)
	if err != nil {
		return nil, err
	}
	defer ref.Close()

	return readEntry(ref.root, name)
}

func readEntry(hroot *os.Root, name string) ([]byte, error) {
	if info, lErr := hroot.Lstat(name); lErr != nil {
		return nil, lErr
	} else if !info.Mode().IsRegular() {
		return nil, ErrNotRegular
	}

	f, err := hroot.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, ErrNotRegular
	}
	if info.Size() > maxEntrySize {
		return nil, fmt.Errorf("version is larger than %d bytes", int64(maxEntrySize))
	}

	return io.ReadAll(io.LimitReader(f, maxEntrySize))
}

// MaybePrune prunes key's history at most once per hour. Called opportunistically
// after a successful backup, on the store lock only, never inside the file lock.
func (s *Store) MaybePrune(key, absPath string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if last, ok := s.lastPrune[key]; ok && time.Since(last) < pruneInterval {
		return
	}
	vroot, err := s.openVersionsRoot(false)
	if err != nil {
		return
	}
	defer vroot.Close()
	ref, err := s.openHistory(vroot, key, absPath, false)
	if err != nil {
		return
	}
	defer ref.Close()
	s.lastPrune[key] = time.Now()
	pruneDir(ref.root)
}

// PruneAll prunes every history, including folders left behind by a failed
// rename. Called once at startup. A history whose folder is a symlink or is
// otherwise not a real directory is refused, deleting nothing.
func (s *Store) PruneAll() {
	s.mu.Lock()
	defer s.mu.Unlock()

	vroot, err := s.openVersionsRoot(false)
	if err != nil {
		return
	}
	defer vroot.Close()
	if err := s.load(vroot); err != nil {
		return
	}
	dirents, err := readDirNames(vroot, ".")
	if err != nil {
		return
	}
	now := time.Now()
	for _, d := range dirents {
		if !d.IsDir() {
			continue
		}
		hroot, err := openVerifiedChild(vroot, d.Name(), false)
		if err != nil {
			continue
		}
		pruneDir(hroot)
		if m, mErr := readMeta(hroot); mErr == nil && m.Key != "" {
			s.lastPrune[m.Key] = now
		}
		hroot.Close()
	}
}

// pruneDir deletes versions older than MaxAge while always retaining the newest
// MinKeep, keeping the union of the two sets. Deletion is oldest first and goes
// through the opened history root, so it can never resolve outside the store.
func pruneDir(hroot *os.Root) {
	entries, err := listEntries(hroot)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-MaxAge)
	n := len(entries)
	for i, e := range entries {
		if i >= n-MinKeep {
			continue
		}
		if e.Time.After(cutoff) {
			continue
		}
		hroot.Remove(e.Name)
	}
}
