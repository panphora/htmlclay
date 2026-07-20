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
package versions

import (
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
	mu        sync.Mutex
	baseDir   string
	index     map[string]history
	loaded    bool
	lastPrune map[string]time.Time
}

// New returns a store rooted at baseDir, which is created lazily on first write.
func New(baseDir string) *Store {
	return &Store{
		baseDir:   resolveBase(baseDir),
		index:     make(map[string]history),
		lastPrune: make(map[string]time.Time),
	}
}

// resolveBase resolves symlinks in the versions directory so containment checks
// compare like with like. session.Manager and asset serving both put request
// paths through EvalSymlinks; a merely cleaned base would then fail to match a
// resolved request path, and a symlinked config directory would let internal
// history be served as an ordinary asset. The directory is created lazily, so
// when it does not exist yet the nearest existing ancestor is resolved instead.
func resolveBase(baseDir string) string {
	cleaned := filepath.Clean(baseDir)
	if resolved, err := filepath.EvalSymlinks(cleaned); err == nil {
		return filepath.Clean(resolved)
	}
	parent, leaf := filepath.Dir(cleaned), filepath.Base(cleaned)
	if resolved, err := filepath.EvalSymlinks(parent); err == nil {
		return filepath.Join(filepath.Clean(resolved), leaf)
	}
	return cleaned
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
	if err := os.MkdirAll(s.baseDir, 0700); err != nil {
		return "", err
	}
	return s.baseDir, nil
}

// Contains reports whether absPath sits inside the versions directory. The server
// uses it to deny requests for internal backup state on the app's own origin: the
// config directory lives under the user's home on every platform, so the static
// path would otherwise be reachable.
func (s *Store) Contains(absPath string) bool {
	return s.contain(absPath) == nil
}

// contain rechecks that child sits strictly inside the versions directory. Run on
// every create, list, restore and prune.
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

func (s *Store) load() error {
	if s.loaded {
		return nil
	}
	if err := os.MkdirAll(s.baseDir, 0700); err != nil {
		return err
	}
	dirents, err := os.ReadDir(s.baseDir)
	if err != nil {
		return err
	}
	for _, d := range dirents {
		if !d.IsDir() {
			continue
		}
		m, err := readMeta(filepath.Join(s.baseDir, d.Name()))
		if err != nil || m.Key == "" {
			continue
		}
		s.index[m.Key] = history{folder: d.Name(), absPath: m.AbsPath}
	}
	s.loaded = true
	return nil
}

func readMeta(dir string) (meta, error) {
	var m meta
	data, err := os.ReadFile(filepath.Join(dir, "meta.json"))
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
// that does not exist on disk.
func (s *Store) freeFolder(base string) (string, error) {
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
		full := filepath.Join(s.baseDir, candidate)
		if err := s.contain(full); err != nil {
			return "", err
		}
		if _, err := os.Lstat(full); errors.Is(err, fs.ErrNotExist) {
			return candidate, nil
		}
	}
	return "", errors.New("cannot find a free history folder name")
}

// historyDir locates the history for key, creating it or renaming it after a file
// rename. Lookup is by key first, so a renamed file keeps its single history
// instead of growing a second folder.
func (s *Store) historyDir(key, absPath string, create bool) (string, error) {
	if err := s.load(); err != nil {
		return "", err
	}

	base := displayBase(absPath, key)

	if h, ok := s.index[key]; ok && h.folder != "" {
		dir := filepath.Join(s.baseDir, h.folder)
		if err := s.contain(dir); err != nil {
			return "", err
		}
		if !folderMatchesBase(h.folder, base) {
			renamed, err := s.freeFolder(base)
			if err == nil {
				newDir := filepath.Join(s.baseDir, renamed)
				if cErr := s.contain(newDir); cErr == nil {
					if rErr := os.Rename(dir, newDir); rErr == nil {
						h.folder = renamed
						dir = newDir
					}
				}
			}
		}
		h.absPath = absPath
		s.index[key] = h
		if create {
			if err := os.MkdirAll(dir, 0700); err != nil {
				return "", err
			}
			if err := s.writeMeta(dir, key, absPath); err != nil {
				return "", err
			}
		}
		return dir, nil
	}

	// Either the key is unknown, or Claim reserved it without materializing a
	// folder. Both become a real history only when something is written.
	if !create {
		return "", ErrNoHistory
	}

	folder, err := s.freeFolder(base)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(s.baseDir, folder)
	if err := s.contain(dir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	if err := s.writeMeta(dir, key, absPath); err != nil {
		return "", err
	}
	s.index[key] = history{folder: folder, absPath: absPath}
	return dir, nil
}

func (s *Store) writeMeta(dir, key, absPath string) error {
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
	return atomicPublishBytes(dir, "meta.json", data)
}

// atomicPublishBytes writes data to a temp file in dir and renames it over name.
// Opening the final name directly would leave a truncated but visible file after
// a crash.
func atomicPublishBytes(dir, name string, data []byte) error {
	tmp, err := os.CreateTemp(dir, ".htmlclay-ver-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

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
	if err := os.Chmod(tmpPath, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, filepath.Join(dir, name)); err != nil {
		return err
	}
	// The bytes are already fsynced; the directory entry that names them is not
	// until the containing directory is synced too.
	return SyncDir(dir)
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

// listEntries returns the versions in dir sorted oldest first, by parsed instant
// rather than by filename. Names carry a zone, so two versions written either
// side of a DST fall-back order correctly even though their wall times repeat.
func listEntries(dir string) ([]Entry, error) {
	dirents, err := os.ReadDir(dir)
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
	if err := s.load(); err != nil {
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
		dir := filepath.Join(s.baseDir, h.folder)
		if cErr := s.contain(dir); cErr != nil {
			return ClaimRenamed, bound, cErr
		}
		return ClaimRenamed, bound, s.writeMeta(dir, key, absPath)
	}
	return ClaimClone, bound, nil
}

// BoundPath returns the absolute path a key's history is currently bound to.
func (s *Store) BoundPath(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
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

	dir, err := s.historyDir(key, absPath, false)
	if err != nil {
		return false
	}
	if err := s.contain(dir); err != nil {
		return false
	}
	entries, err := listEntries(dir)
	return err == nil && len(entries) > 0
}

// Rebind points an existing history at a new absolute path. Used when a file with
// a known id turns out to be a rename rather than a clone.
func (s *Store) Rebind(key, absPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return err
	}
	h, ok := s.index[key]
	if !ok {
		return ErrNoHistory
	}
	h.absPath = absPath
	s.index[key] = h
	dir := filepath.Join(s.baseDir, h.folder)
	if err := s.contain(dir); err != nil {
		return err
	}
	return s.writeMeta(dir, key, absPath)
}

// Backup publishes content as a new version of key's history, atomically.
// It reports whether a new version was actually written: identical content is
// deduped and reports false with a nil error.
func (s *Store) Backup(key, absPath string, content []byte) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	dir, err := s.historyDir(key, absPath, true)
	if err != nil {
		return false, err
	}

	entries, err := listEntries(dir)
	if err != nil {
		return false, err
	}

	want := Hash(content)
	if len(entries) > 0 {
		newest := entries[len(entries)-1]
		if existing, rErr := os.ReadFile(filepath.Join(dir, newest.Name)); rErr == nil && Hash(existing) == want {
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

	tmpPath, err := writeTemp(dir, content)
	if err != nil {
		return false, err
	}
	defer os.Remove(tmpPath)

	for i := 0; i < 1000; i++ {
		final := filepath.Join(dir, entryName(t, seq))
		if cErr := s.contain(final); cErr != nil {
			return false, cErr
		}
		// os.Link is both exclusive (EEXIST on collision) and atomic, so a crash
		// never leaves a half-written version under a real name.
		err = os.Link(tmpPath, final)
		if err == nil {
			if sErr := SyncDir(dir); sErr != nil {
				return true, sErr
			}
			if err := s.writeMeta(dir, key, absPath); err != nil {
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
		name := entryName(t, seq)
		final := filepath.Join(dir, name)
		if cErr := s.contain(final); cErr != nil {
			return false, cErr
		}
		if _, sErr := os.Lstat(final); sErr == nil {
			seq++
			continue
		}
		if rErr := os.Rename(tmpPath, final); rErr != nil {
			return false, rErr
		}
		if sErr := SyncDir(dir); sErr != nil {
			return true, sErr
		}
		if err := s.writeMeta(dir, key, absPath); err != nil {
			return true, err
		}
		return true, nil
	}
	return false, fmt.Errorf("cannot publish version: %w", err)
}

func writeTemp(dir string, content []byte) (string, error) {
	tmp, err := os.CreateTemp(dir, ".htmlclay-ver-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return "", err
	}
	if err := os.Chmod(tmpPath, 0600); err != nil {
		os.Remove(tmpPath)
		return "", err
	}
	return tmpPath, nil
}

// List returns key's versions, newest first.
func (s *Store) List(key, absPath string) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	dir, err := s.historyDir(key, absPath, false)
	if err != nil {
		if errors.Is(err, ErrNoHistory) {
			return nil, nil
		}
		return nil, err
	}
	if err := s.contain(dir); err != nil {
		return nil, err
	}
	entries, err := listEntries(dir)
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	return entries, nil
}

// Read returns the bytes of one version. name must be exactly one generated
// filename; the file is opened beneath the resolved history directory through
// os.Root so a swapped symlink cannot escape, and must be a regular file.
func (s *Store) Read(key, absPath, name string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, _, err := ParseEntryName(name); err != nil {
		return nil, err
	}

	dir, err := s.historyDir(key, absPath, false)
	if err != nil {
		return nil, err
	}
	if err := s.contain(dir); err != nil {
		return nil, err
	}

	rt, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer rt.Close()

	if info, lErr := rt.Lstat(name); lErr != nil {
		return nil, lErr
	} else if !info.Mode().IsRegular() {
		return nil, ErrNotRegular
	}

	f, err := rt.Open(name)
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
	dir, err := s.historyDir(key, absPath, false)
	if err != nil {
		return
	}
	s.lastPrune[key] = time.Now()
	if err := s.contain(dir); err != nil {
		return
	}
	pruneDir(dir)
}

// PruneAll prunes every history, including folders left behind by a failed
// rename. Called once at startup.
func (s *Store) PruneAll() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.load(); err != nil {
		return
	}
	dirents, err := os.ReadDir(s.baseDir)
	if err != nil {
		return
	}
	now := time.Now()
	for _, d := range dirents {
		if !d.IsDir() {
			continue
		}
		dir := filepath.Join(s.baseDir, d.Name())
		if err := s.contain(dir); err != nil {
			continue
		}
		pruneDir(dir)
		if m, mErr := readMeta(dir); mErr == nil && m.Key != "" {
			s.lastPrune[m.Key] = now
		}
	}
}

// pruneDir deletes versions older than MaxAge while always retaining the newest
// MinKeep, keeping the union of the two sets.
func pruneDir(dir string) {
	entries, err := listEntries(dir)
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
		os.Remove(filepath.Join(dir, e.Name))
	}
}
