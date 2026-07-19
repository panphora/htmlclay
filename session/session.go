package session

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
}

// Lock and Unlock serialize read-modify-write operations on this file (saves and
// on-serve htmlclayid injection) so concurrent handlers cannot clobber each other.
func (f *File) Lock()   { f.writeMu.Lock() }
func (f *File) Unlock() { f.writeMu.Unlock() }

type Manager struct {
	mu      sync.RWMutex
	byToken map[string]*File
	byPath  map[string]string
	roots   map[string]struct{}
	homeDir string
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
		byToken: make(map[string]*File),
		byPath:  make(map[string]string),
		roots:   make(map[string]struct{}),
		homeDir: normalizeHome(homeDir),
	}
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
	// The home directory itself never becomes an asset root: a file opened loose
	// in ~ must not expose the whole home tree to every page.
	if dir := filepath.Dir(cleaned); dir != m.homeDir {
		m.roots[dir] = struct{}{}
	}
	return f, nil
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

// AllowsAsset reports whether absPath sits under the directory of any opened
// file. Opening a file grants its page access to that folder tree, nothing more.
func (m *Manager) AllowsAsset(absPath string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for root := range m.roots {
		if _, ok := ContainWithinHome(root, absPath); ok {
			return true
		}
	}
	return false
}

func (m *Manager) RevokeAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byToken = make(map[string]*File)
	m.byPath = make(map[string]string)
	m.roots = make(map[string]struct{})
}
