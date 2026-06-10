package session

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

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
		homeDir: normalizeHome(homeDir),
	}
}

func (m *Manager) HomeDir() string {
	return m.homeDir
}

func resolveAndValidate(absPath, homeDir string) (string, error) {
	cleaned, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return "", fmt.Errorf("cannot resolve path %q: %w", absPath, err)
	}
	cleaned = filepath.Clean(cleaned)

	if !strings.HasPrefix(cleaned, homeDir+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q is outside home directory", cleaned)
	}

	return cleaned, nil
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

func (m *Manager) RevokeAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byToken = make(map[string]*File)
	m.byPath = make(map[string]string)
}
