# Malleable: Implementation Plan

Reference spec: `malleable-app-spec.md` in this same directory. Every implementation decision traces back to that document.

## Project Structure

```
malleable/
├── main.go                     # Entry point: CLI parsing, orchestration
├── go.mod
├── go.sum
├── Makefile                    # Build targets: build, test, clean, dist-macos
│
├── server/
│   ├── server.go               # HTTP server setup, router, middleware
│   ├── server_test.go
│   ├── handlers.go             # Handler functions: ServeFile, Read, Save, Meta
│   ├── handlers_test.go
│   ├── security.go             # Host header validation, path traversal checks
│   └── security_test.go
│
├── session/
│   ├── session.go              # Token generation, file registration, token→file lookup
│   └── session_test.go
│
├── htmlutil/
│   ├── htmlutil.go             # Inject appname attribute, strip appname attribute
│   └── htmlutil_test.go
│
├── browser/
│   ├── browser.go              # Interface + Browser Mode (open default browser)
│   ├── browser_darwin.go       # macOS: `open` command
│   ├── browser_linux.go        # Linux: `xdg-open`
│   ├── browser_windows.go      # Windows: `start`
│   ├── chrome.go               # Chromium detection + App Mode launch
│   └── chrome_test.go
│
├── config/
│   ├── config.go               # Load/save ~/.malleable/config.json, port management
│   └── config_test.go
│
├── logging/
│   ├── logging.go              # File-based logger with 10MB rotation
│   └── logging_test.go
│
├── platform/
│   ├── singleinstance.go       # Interface for single-instance enforcement
│   ├── singleinstance_darwin.go # macOS: Unix socket (dev) / Apple Events (app bundle)
│   ├── singleinstance_linux.go  # Linux: lock file + Unix socket
│   ├── singleinstance_windows.go# Windows: named mutex + named pipe
│   ├── loginitem.go            # Interface for Start on Login
│   ├── loginitem_darwin.go     # macOS: LaunchAgent plist
│   ├── loginitem_linux.go      # Linux: .desktop file in ~/.config/autostart/
│   └── loginitem_windows.go    # Windows: Registry key
│
├── tray/
│   ├── tray.go                 # System tray icon + menu (uses systray library)
│   └── tray_test.go
│
├── update/
│   ├── update.go               # Version check: fetch version.json, compare
│   └── update_test.go
│
├── dist/
│   └── macos/
│       ├── Info.plist           # macOS app bundle metadata + file association
│       ├── malleable.icns       # App icon (placeholder)
│       └── build.sh            # Script to assemble .app bundle
│
└── testdata/
    ├── minimal.malleable        # Minimal test file
    ├── with-appname.malleable   # File that already has appname attr (edge case)
    └── traversal.malleable      # File for path traversal tests
```

### External dependencies

| Dependency | Purpose |
|---|---|
| `github.com/getlantern/systray` (or `fyne.io/systray`) | System tray icon and menu (Phase 4+). |

Everything else uses Go's standard library.

---

## Phase 1: Core Server

**Goal:** A working HTTP server that can serve, read, save, and return metadata for `.malleable` files. Testable entirely with `curl`. No browser launch, no tray.

### Step 1.1: Project bootstrap

```bash
mkdir malleable && cd malleable
go mod init github.com/user/malleable
```

#### `config/config.go`

```go
package config

import (
    "encoding/json"
    "fmt"
    "net"
    "os"
    "path/filepath"
)

type Config struct {
    Mode         string `json:"mode"`
    StartOnLogin bool   `json:"startOnLogin"`
    Port         int    `json:"port"`
}

func Dir() string {
    home, _ := os.UserHomeDir()
    return filepath.Join(home, ".malleable")
}

func Path() string {
    return filepath.Join(Dir(), "config.json")
}

// EnsureDir creates ~/.malleable/ if it doesn't exist.
func EnsureDir() error {
    return os.MkdirAll(Dir(), 0755)
}

// Load reads config from disk. Returns defaults if the file doesn't exist.
func Load() (*Config, error) {
    cfg := &Config{
        Mode:         "app",
        StartOnLogin: false,
        Port:         0,
    }

    data, err := os.ReadFile(Path())
    if os.IsNotExist(err) {
        return cfg, nil
    }
    if err != nil {
        return nil, err
    }

    if err := json.Unmarshal(data, cfg); err != nil {
        return nil, err
    }
    return cfg, nil
}

// Save writes config to disk.
func (c *Config) Save() error {
    if err := EnsureDir(); err != nil {
        return err
    }
    data, err := json.MarshalIndent(c, "", "  ")
    if err != nil {
        return err
    }
    return os.WriteFile(Path(), data, 0644)
}

// ResolvePort returns the configured port if it's available, otherwise picks
// a random available port. Saves the chosen port to config.
func (c *Config) ResolvePort() (int, error) {
    if c.Port != 0 {
        // Try the saved port first
        ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", c.Port))
        if err == nil {
            ln.Close()
            return c.Port, nil
        }
        // Port is taken, fall through to pick a new one
    }

    // Listen on :0 to get a random available port
    ln, err := net.Listen("tcp", "127.0.0.1:0")
    if err != nil {
        return 0, err
    }
    port := ln.Addr().(*net.TCPAddr).Port
    ln.Close()

    c.Port = port
    if err := c.Save(); err != nil {
        return 0, err
    }
    return port, nil
}
```

#### `config/config_test.go`

Test scenarios:
- `Load()` returns defaults when no file exists
- `Save()` then `Load()` round-trips correctly
- `EnsureDir()` creates the directory
- `ResolvePort()` with `Port: 0` picks an available port and saves it
- `ResolvePort()` with a valid saved port reuses it
- `ResolvePort()` with a taken port picks a new one

---

### Step 1.2: Session manager

#### `session/session.go`

```go
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
    RelPath string // relative to home dir, e.g. "Documents/file.malleable"
    Name    string // just the filename, e.g. "file.malleable"
}

type Manager struct {
    mu      sync.RWMutex
    byToken map[string]*File   // token -> File
    byPath  map[string]string  // absolute path -> token
    homeDir string
}

func NewManager() (*Manager, error) {
    home, err := os.UserHomeDir()
    if err != nil {
        return nil, fmt.Errorf("cannot determine home directory: %w", err)
    }
    return &Manager{
        byToken: make(map[string]*File),
        byPath:  make(map[string]string),
        homeDir: home,
    }, nil
}

func (m *Manager) HomeDir() string {
    return m.homeDir
}

// Register adds a file to the session. If the file is already registered,
// returns the existing entry. absPath must be an absolute path.
func (m *Manager) Register(absPath string) (*File, error) {
    // Clean and resolve symlinks
    cleaned, err := filepath.EvalSymlinks(absPath)
    if err != nil {
        return nil, fmt.Errorf("cannot resolve path %q: %w", absPath, err)
    }
    cleaned = filepath.Clean(cleaned)

    // Must be inside home directory
    if !strings.HasPrefix(cleaned, m.homeDir+string(os.PathSeparator)) {
        return nil, fmt.Errorf("path %q is outside home directory", cleaned)
    }

    m.mu.Lock()
    defer m.mu.Unlock()

    // Already registered?
    if token, ok := m.byPath[cleaned]; ok {
        return m.byToken[token], nil
    }

    // Generate token: 32 bytes, base64url-encoded (43 chars, 256 bits)
    tokenBytes := make([]byte, 32)
    if _, err := rand.Read(tokenBytes); err != nil {
        return nil, fmt.Errorf("cannot generate token: %w", err)
    }
    token := base64.RawURLEncoding.EncodeToString(tokenBytes)

    // Compute relative path from home directory
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

// Lookup returns the file for a given token.
func (m *Manager) Lookup(token string) (*File, bool) {
    m.mu.RLock()
    defer m.mu.RUnlock()
    f, ok := m.byToken[token]
    return f, ok
}

// LookupByPath returns the file for a given absolute path.
func (m *Manager) LookupByPath(absPath string) (*File, bool) {
    m.mu.RLock()
    defer m.mu.RUnlock()
    token, ok := m.byPath[absPath]
    if !ok {
        return nil, false
    }
    return m.byToken[token], true
}

// RevokeAll removes all registered files and tokens.
func (m *Manager) RevokeAll() {
    m.mu.Lock()
    defer m.mu.Unlock()
    m.byToken = make(map[string]*File)
    m.byPath = make(map[string]string)
}
```

#### `session/session_test.go`

Test scenarios:
- `Register` returns a file with a 43-character token
- `Register` the same path twice returns the same `*File` (same token)
- `Register` two different paths returns different tokens
- `Register` a path outside home dir returns an error
- `Register` a path with `../` that escapes home dir returns an error
- `Lookup` with valid token returns the file
- `Lookup` with invalid token returns false
- `LookupByPath` with registered path returns the file
- `LookupByPath` with unregistered path returns false
- `RevokeAll` clears everything
- Concurrent `Register` and `Lookup` calls don't race (run with `-race`)

---

### Step 1.3: HTML utilities

#### `htmlutil/htmlutil.go`

```go
package htmlutil

import "regexp"

// Matches the opening <html tag (case-insensitive).
// Captures everything up to and including "<html" as group 1,
// and the next character (space, >, or newline) as group 2.
var htmlTagOpen = regexp.MustCompile(`(?i)(<html)([\s>])`)

// Matches an existing appname="..." attribute (case-insensitive for the tag context).
// Handles both double and single quotes.
var appnameAttr = regexp.MustCompile(`(?i)\s+appname=("[^"]*"|'[^']*')`)

// InjectAppName adds or replaces the appname attribute on the <html> element.
// If appname already exists, its value is replaced. If not, it's inserted
// after <html. This is idempotent — calling it multiple times with the
// same value produces the same result.
func InjectAppName(html []byte, value string) []byte {
    attr := ` appname="` + value + `"`

    // If appname already exists, replace it
    if appnameAttr.Match(html) {
        return appnameAttr.ReplaceAll(html, []byte(attr))
    }

    // Otherwise, insert after <html
    // <html> becomes <html appname="value">
    // <html lang="en"> becomes <html appname="value" lang="en">
    return htmlTagOpen.ReplaceAll(html, []byte(`${1}`+attr+`${2}`))
}

// StripAppName removes the appname attribute from the <html> element.
// Used by Malleable on save to keep the file on disk clean (the token
// is a session value, not meaningful on disk). Hyperclay does NOT call
// this — the site name in appname is harmless to persist.
func StripAppName(html []byte) []byte {
    return appnameAttr.ReplaceAll(html, nil)
}
```

**Important regex notes for the implementer:**

- `htmlTagOpen` uses `(?i)` for case-insensitive matching. This handles `<html>`, `<HTML>`, `<Html>`, etc.
- The `${1}` and `${2}` in the replacement string are Go regex backreferences (using `$` not `\`).
- `appnameAttr` matches ` appname="anything"` with the leading space, so stripping it doesn't leave a double space.
- The regex only operates on the first match. Since there's only one `<html>` tag in a valid document, this is correct. If the HTML has no `<html>` tag at all (fragment), `InjectAppName` returns the input unchanged — this is acceptable since the file is malformed anyway.

#### `htmlutil/htmlutil_test.go`

Test scenarios with exact input/output:

```go
// Inject into bare <html>
// Input:  <html>
// Output: <html appname="tok123">

// Inject into <html> with existing attributes
// Input:  <html lang="en">
// Output: <html appname="tok123" lang="en">

// Inject into <HTML> (uppercase)
// Input:  <HTML>
// Output: <HTML appname="tok123">

// Replace existing appname
// Input:  <html appname="old-value" lang="en">
// Output: <html appname="new-value" lang="en">

// Replace existing appname (single quotes)
// Input:  <html appname='old-value'>
// Output: <html appname="new-value">

// Strip appname
// Input:  <html appname="tok123" lang="en">
// Output: <html lang="en">

// Strip appname (only attribute)
// Input:  <html appname="tok123">
// Output: <html>

// Round-trip: inject then strip returns original
// Input:  <html lang="en">
// After inject: <html appname="tok123" lang="en">
// After strip:  <html lang="en">

// No <html> tag — inject returns unchanged
// Input:  <div>hello</div>
// Output: <div>hello</div>

// Full document round-trip (byte-for-byte)
// Input:  <!DOCTYPE html>\n<html lang="en">\n<head>...
// Inject, then strip, compare bytes — must be identical

// appname inside a comment or script should NOT be touched
// Input:  <html><!-- appname="fake" --><script>let appname="x"</script>
// After strip: same (the regex targets the <html> tag only, not content)
```

---

### Step 1.4: Security utilities

#### `server/security.go`

```go
package server

import (
    "fmt"
    "net/http"
    "os"
    "path/filepath"
    "strings"
)

// ValidateHost checks that the Host header is 127.0.0.1:{port} or localhost:{port}.
// This prevents DNS rebinding attacks.
func ValidateHost(r *http.Request, port int) bool {
    host := r.Host
    allowed1 := fmt.Sprintf("127.0.0.1:%d", port)
    allowed2 := fmt.Sprintf("localhost:%d", port)
    return host == allowed1 || host == allowed2
}

// ValidatePath resolves a relative path (from URL) against the home directory
// and verifies the result doesn't escape it. Returns the cleaned absolute path.
//
// Examples:
//   ValidatePath("Documents/file.malleable", "/Users/david")
//     → "/Users/david/Documents/file.malleable", nil
//
//   ValidatePath("../../../etc/passwd", "/Users/david")
//     → "", error
//
//   ValidatePath("Documents/../../etc/passwd", "/Users/david")
//     → "", error
func ValidatePath(relPath string, homeDir string) (string, error) {
    // Reject paths that start with / (absolute) or contain null bytes
    if strings.HasPrefix(relPath, "/") || strings.Contains(relPath, "\x00") {
        return "", fmt.Errorf("invalid path: %q", relPath)
    }

    joined := filepath.Join(homeDir, relPath)
    cleaned := filepath.Clean(joined)

    // After cleaning, the path must still be inside the home directory.
    // Use HasPrefix with the separator to prevent matching /Users/david2/...
    if !strings.HasPrefix(cleaned, homeDir+string(os.PathSeparator)) {
        return "", fmt.Errorf("path escapes home directory: %q", relPath)
    }

    return cleaned, nil
}

// HostValidationMiddleware wraps an http.Handler and rejects requests
// with invalid Host headers.
func HostValidationMiddleware(next http.Handler, port int) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if !ValidateHost(r, port) {
            http.Error(w, "Forbidden", http.StatusForbidden)
            return
        }
        next.ServeHTTP(w, r)
    })
}
```

#### `server/security_test.go`

```go
// ValidateHost tests:
// - "127.0.0.1:49821" with port 49821 → true
// - "localhost:49821" with port 49821 → true
// - "127.0.0.1:9999" with port 49821 → false (wrong port)
// - "evil.com:49821" → false
// - "127.0.0.1" (no port) → false
// - "" (empty) → false

// ValidatePath tests:
// - "Documents/file.malleable" → /Users/david/Documents/file.malleable (success)
// - "file.malleable" → /Users/david/file.malleable (success)
// - "a/b/c/file.malleable" → /Users/david/a/b/c/file.malleable (success)
// - "../../../etc/passwd" → error
// - "Documents/../../etc/passwd" → error
// - "/etc/passwd" (absolute) → error
// - "Documents/file\x00.malleable" (null byte) → error
// - "" (empty) → depends on implementation, probably error or homeDir itself
// - "Documents/../Documents/file.malleable" → /Users/david/Documents/file.malleable (success, normalized)
```

---

### Step 1.5: HTTP handlers

#### `server/server.go`

Go 1.22+ has pattern matching in `http.ServeMux` with `{name}` and `{name...}` wildcards. We use this. If you need Go < 1.22, you'd need a third-party router — but Go 1.22+ is the right baseline.

```go
package server

import (
    "fmt"
    "log"
    "net/http"

    "github.com/user/malleable/session"
)

type Server struct {
    httpServer *http.Server
    sessions   *session.Manager
    port       int
    logger     *log.Logger
}

func New(port int, sessions *session.Manager, logger *log.Logger) *Server {
    s := &Server{
        sessions: sessions,
        port:     port,
        logger:   logger,
    }

    mux := http.NewServeMux()

    // GET /f/{path...} — path... captures the rest of the URL including slashes
    mux.HandleFunc("GET /f/{path...}", s.handleServeFile)

    // GET /read/{token}
    mux.HandleFunc("GET /read/{token}", s.handleRead)

    // POST /save/{token}
    mux.HandleFunc("POST /save/{token}", s.handleSave)

    // GET /meta/{token}
    mux.HandleFunc("GET /meta/{token}", s.handleMeta)

    // Wrap everything with Host header validation
    handler := HostValidationMiddleware(mux, port)

    s.httpServer = &http.Server{
        Addr:    fmt.Sprintf("127.0.0.1:%d", port),
        Handler: handler,
    }

    return s
}

// Start begins listening. Call in a goroutine — this blocks.
func (s *Server) Start() error {
    s.logger.Printf("Server listening on 127.0.0.1:%d", s.port)
    return s.httpServer.ListenAndServe()
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
    return s.httpServer.Shutdown(ctx)
}
```

**Note on Go 1.22+ routing:** The pattern `"GET /f/{path...}"` means:
- Only matches GET requests
- `{path...}` captures everything after `/f/`, including slashes (e.g., `Documents/subfolder/file.malleable`)
- Access it with `r.PathValue("path")`

If using an older Go version, you'd need to register `/f/` as a prefix handler and manually parse the path. Stick with Go 1.22+.

#### `server/handlers.go`

```go
package server

import (
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "os"
    "path/filepath"
    "time"

    "github.com/user/malleable/htmlutil"
)

const maxSaveSize = 50 * 1024 * 1024 // 50 MB

// handleServeFile handles GET /f/{path...}
// Serves a registered .malleable file with appname injected.
func (s *Server) handleServeFile(w http.ResponseWriter, r *http.Request) {
    relPath := r.PathValue("path")

    absPath, err := ValidatePath(relPath, s.sessions.HomeDir())
    if err != nil {
        s.logger.Printf("Invalid path %q: %v", relPath, err)
        http.Error(w, "Not Found", http.StatusNotFound)
        return
    }

    // Must be a registered file (opened via double-click or CLI)
    f, ok := s.sessions.LookupByPath(absPath)
    if !ok {
        http.Error(w, "Not Found", http.StatusNotFound)
        return
    }

    // Read file from disk
    data, err := os.ReadFile(f.AbsPath)
    if err != nil {
        s.logger.Printf("Error reading %s: %v", f.AbsPath, err)
        http.Error(w, "Internal Server Error", http.StatusInternalServerError)
        return
    }

    // Inject appname attribute with the session token
    data = htmlutil.InjectAppName(data, f.Token)

    w.Header().Set("Content-Type", "text/html; charset=utf-8")
    w.Write(data)
}

// handleRead handles GET /read/{token}
// Returns the raw file contents from disk (no appname injection).
func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) {
    token := r.PathValue("token")

    f, ok := s.sessions.Lookup(token)
    if !ok {
        s.writeError(w, http.StatusUnauthorized, "invalid token")
        return
    }

    data, err := os.ReadFile(f.AbsPath)
    if err != nil {
        s.logger.Printf("Error reading %s: %v", f.AbsPath, err)
        s.writeError(w, http.StatusInternalServerError, "read error")
        return
    }

    w.Header().Set("Content-Type", "text/html; charset=utf-8")
    w.Write(data)
}

// handleSave handles POST /save/{token}
// Accepts full HTML body, strips appname, writes to disk atomically.
func (s *Server) handleSave(w http.ResponseWriter, r *http.Request) {
    token := r.PathValue("token")

    f, ok := s.sessions.Lookup(token)
    if !ok {
        s.writeError(w, http.StatusUnauthorized, "invalid token")
        return
    }

    // Limit request body to 50MB
    r.Body = http.MaxBytesReader(w, r.Body, maxSaveSize)

    body, err := io.ReadAll(r.Body)
    if err != nil {
        // MaxBytesReader returns a specific error when the limit is exceeded.
        // The error message contains "http: request body too large".
        if err.Error() == "http: request body too large" {
            s.writeError(w, http.StatusRequestEntityTooLarge, "body too large (max 50MB)")
            return
        }
        s.writeError(w, http.StatusInternalServerError, "read error")
        return
    }

    // Strip the appname attribute before writing to disk.
    // Malleable tokens are session values — meaningless on disk.
    body = htmlutil.StripAppName(body)

    // Atomic write: write to temp file in the same directory, then rename.
    // Same directory ensures same filesystem (os.Rename requirement).
    dir := filepath.Dir(f.AbsPath)
    tmp, err := os.CreateTemp(dir, ".malleable-save-*")
    if err != nil {
        s.logger.Printf("Error creating temp file in %s: %v", dir, err)
        s.writeError(w, http.StatusInternalServerError, "write error")
        return
    }
    tmpPath := tmp.Name()

    // If anything goes wrong, clean up the temp file
    defer func() {
        if tmpPath != "" {
            os.Remove(tmpPath)
        }
    }()

    if _, err := tmp.Write(body); err != nil {
        tmp.Close()
        s.logger.Printf("Error writing temp file %s: %v", tmpPath, err)
        s.writeError(w, http.StatusInternalServerError, "write error")
        return
    }
    if err := tmp.Close(); err != nil {
        s.logger.Printf("Error closing temp file %s: %v", tmpPath, err)
        s.writeError(w, http.StatusInternalServerError, "write error")
        return
    }

    // Preserve the original file's permissions
    if info, err := os.Stat(f.AbsPath); err == nil {
        os.Chmod(tmpPath, info.Mode())
    }

    // Atomic rename
    if err := os.Rename(tmpPath, f.AbsPath); err != nil {
        s.logger.Printf("Error renaming %s to %s: %v", tmpPath, f.AbsPath, err)
        s.writeError(w, http.StatusForbidden, "permission denied")
        return
    }

    tmpPath = "" // Prevent deferred cleanup since rename succeeded

    s.logger.Printf("Saved %s (%d bytes)", f.RelPath, len(body))
    w.Header().Set("Content-Type", "application/json")
    w.Write([]byte(`{"ok":true}`))
}

// handleMeta handles GET /meta/{token}
// Returns JSON metadata about the file.
func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
    token := r.PathValue("token")

    f, ok := s.sessions.Lookup(token)
    if !ok {
        s.writeError(w, http.StatusUnauthorized, "invalid token")
        return
    }

    info, err := os.Stat(f.AbsPath)
    if err != nil {
        s.logger.Printf("Error stat %s: %v", f.AbsPath, err)
        s.writeError(w, http.StatusInternalServerError, "stat error")
        return
    }

    meta := map[string]interface{}{
        "path":         f.RelPath,
        "absolutePath": f.AbsPath,
        "name":         f.Name,
        "size":         info.Size(),
        "lastModified": info.ModTime().UTC().Format(time.RFC3339),
    }

    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(meta)
}

// writeError sends a JSON error response.
func (s *Server) writeError(w http.ResponseWriter, status int, message string) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(status)
    json.NewEncoder(w).Encode(map[string]interface{}{
        "ok":    false,
        "error": message,
    })
}
```

#### `server/handlers_test.go`

Use `httptest` for all handler tests. Here's the pattern:

```go
package server

import (
    "io"
    "net/http"
    "net/http/httptest"
    "os"
    "path/filepath"
    "strings"
    "testing"

    "github.com/user/malleable/session"
)

// Helper: create a temp file inside the home dir and register it.
func setupTest(t *testing.T) (*Server, *session.File, string) {
    t.Helper()

    // Create a temp dir to act as "home"
    homeDir := t.TempDir()

    // Create a test .malleable file
    filePath := filepath.Join(homeDir, "test.malleable")
    content := `<!DOCTYPE html>
<html lang="en">
<head><title>Test</title></head>
<body><p>Hello</p></body>
</html>`
    os.WriteFile(filePath, []byte(content), 0644)

    // Create session manager with custom home dir
    mgr := &session.Manager{} // You'll need to allow setting homeDir in tests
    f, _ := mgr.Register(filePath)

    logger := log.New(io.Discard, "", 0)
    srv := New(49821, mgr, logger)

    return srv, f, content
}

// Test: GET /f/{path} serves the file with appname injected
// 1. Register a file
// 2. GET /f/test.malleable
// 3. Assert status 200
// 4. Assert response contains appname="<token>"
// 5. Assert response contains the file's original HTML content

// Test: GET /f/{path} returns 404 for unregistered file
// 1. GET /f/nonexistent.malleable
// 2. Assert status 404

// Test: GET /f/{path} returns 404 for path traversal
// 1. GET /f/../../../etc/passwd
// 2. Assert status 404

// Test: GET /read/{token} returns raw file contents
// 1. Register a file, GET /read/<token>
// 2. Assert response is the raw file content (no appname)

// Test: GET /read/{token} returns 401 for invalid token
// 1. GET /read/invalid-token
// 2. Assert status 401, body contains {"ok":false,"error":"invalid token"}

// Test: POST /save/{token} writes to disk
// 1. Register a file
// 2. POST /save/<token> with new HTML body
// 3. Assert status 200, body {"ok":true}
// 4. Read the file from disk, assert it matches the sent body (with appname stripped)

// Test: POST /save/{token} strips appname before writing
// 1. Register a file
// 2. POST /save/<token> with body containing appname="something"
// 3. Read file from disk, assert appname is gone

// Test: POST /save/{token} returns 401 for invalid token
// 1. POST /save/bad-token with body
// 2. Assert status 401

// Test: POST /save/{token} returns 413 for body over 50MB
// 1. Register a file
// 2. POST /save/<token> with 50MB + 1 byte body
// 3. Assert status 413

// Test: POST /save/{token} atomic write — original file untouched on failure
// (Hard to test directly, but verify the temp file pattern works)

// Test: GET /meta/{token} returns correct metadata
// 1. Register a file
// 2. GET /meta/<token>
// 3. Assert JSON contains path, absolutePath, name, size, lastModified
// 4. Assert size matches actual file size

// Test: Host header validation
// 1. Create a request with Host: evil.com
// 2. Assert status 403
```

To use `httptest`, the pattern is:

```go
func TestServeFile(t *testing.T) {
    srv, f, _ := setupTest(t)

    req := httptest.NewRequest("GET", "/f/test.malleable", nil)
    req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
    // For Go 1.22 routing, set the path value manually in tests:
    req.SetPathValue("path", "test.malleable")

    w := httptest.NewRecorder()
    srv.handleServeFile(w, req)

    if w.Code != 200 {
        t.Fatalf("expected 200, got %d", w.Code)
    }

    body := w.Body.String()
    if !strings.Contains(body, `appname="`+f.Token+`"`) {
        t.Fatal("response missing appname attribute")
    }
}
```

---

### Step 1.6: Wire it up in `main.go`

```go
package main

import (
    "context"
    "flag"
    "fmt"
    "log"
    "os"
    "os/signal"
    "path/filepath"
    "syscall"

    "github.com/user/malleable/config"
    "github.com/user/malleable/server"
    "github.com/user/malleable/session"
)

func main() {
    appMode := flag.Bool("app", false, "Open in App Mode (chromeless window)")
    browserMode := flag.Bool("browser", false, "Open in Browser Mode (default)")
    flag.Parse()

    args := flag.Args()
    if len(args) < 1 {
        fmt.Fprintln(os.Stderr, "Usage: malleable [--app|--browser] <file>")
        os.Exit(1)
    }

    // Resolve the file path to absolute
    filePath, err := filepath.Abs(args[0])
    if err != nil {
        fmt.Fprintf(os.Stderr, "Error resolving path: %v\n", err)
        os.Exit(1)
    }

    // Verify file exists
    if _, err := os.Stat(filePath); os.IsNotExist(err) {
        fmt.Fprintf(os.Stderr, "File not found: %s\n", filePath)
        os.Exit(1)
    }

    // Load config
    cfg, err := config.Load()
    if err != nil {
        fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
        os.Exit(1)
    }

    // Ensure ~/.malleable/ exists
    if err := config.EnsureDir(); err != nil {
        fmt.Fprintf(os.Stderr, "Error creating config dir: %v\n", err)
        os.Exit(1)
    }

    // Resolve port
    port, err := cfg.ResolvePort()
    if err != nil {
        fmt.Fprintf(os.Stderr, "Error resolving port: %v\n", err)
        os.Exit(1)
    }

    // Create session manager
    sessions, err := session.NewManager()
    if err != nil {
        fmt.Fprintf(os.Stderr, "Error creating session manager: %v\n", err)
        os.Exit(1)
    }

    // Register the file
    f, err := sessions.Register(filePath)
    if err != nil {
        fmt.Fprintf(os.Stderr, "Error registering file: %v\n", err)
        os.Exit(1)
    }

    // Create logger (just stdout for Phase 1, file-based in Phase 3)
    logger := log.New(os.Stdout, "[malleable] ", log.LstdFlags)

    // Start server in a goroutine
    srv := server.New(port, sessions, logger)
    go func() {
        if err := srv.Start(); err != nil && err.Error() != "http: Server closed" {
            logger.Fatalf("Server error: %v", err)
        }
    }()

    // Determine mode: --app flag, --browser flag, or default (browser for CLI)
    mode := "browser" // CLI default
    if *appMode {
        mode = "app"
    }
    _ = mode        // Used in Phase 2 for browser launch
    _ = browserMode // Explicit browser flag (same as default)

    url := fmt.Sprintf("http://127.0.0.1:%d/f/%s", port, f.RelPath)
    logger.Printf("Serving %s at %s", f.Name, url)
    fmt.Println(url)

    // Block until SIGINT or SIGTERM
    sigCh := make(chan os.Signal, 1)
    signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
    <-sigCh

    logger.Println("Shutting down...")
    sessions.RevokeAll()
    srv.Shutdown(context.Background())
}
```

### Manual smoke test for Phase 1

After building (`go build -o malleable .`), test with `curl`:

```bash
# Start the server
./malleable testdata/minimal.malleable
# Output: http://127.0.0.1:49821/f/Documents/testdata/minimal.malleable
# (port and path will vary)

# In another terminal, using the URL and port from the output:

# 1. Serve the file (should have appname injected)
curl -s http://127.0.0.1:49821/f/testdata/minimal.malleable
# Look for: <html appname="<43-char-token>">

# 2. Read the file (raw, no appname)
# Get the token from the served HTML's appname attribute
TOKEN="paste-the-token-here"
curl -s http://127.0.0.1:49821/read/$TOKEN
# Should return raw file contents without appname

# 3. Save new content
curl -s -X POST \
  -H "Content-Type: text/html" \
  -d '<!DOCTYPE html><html appname="tok123"><body>Updated!</body></html>' \
  http://127.0.0.1:49821/save/$TOKEN
# Should return: {"ok":true}
# Check the file on disk — appname should be stripped:
cat testdata/minimal.malleable
# Should show: <html><body>Updated!</body></html> (no appname)

# 4. Get metadata
curl -s http://127.0.0.1:49821/meta/$TOKEN | jq .
# Should return JSON with path, absolutePath, name, size, lastModified

# 5. Invalid token
curl -s http://127.0.0.1:49821/save/invalid-token -X POST -d "test"
# Should return: {"ok":false,"error":"invalid token"} with status 401

# 6. Path traversal
curl -s http://127.0.0.1:49821/f/../../../etc/passwd
# Should return 404

# 7. Host header attack
curl -s -H "Host: evil.com" http://127.0.0.1:49821/f/testdata/minimal.malleable
# Should return 403 Forbidden
```

---

## Phase 2: Browser Launch + CLI Polish

**Goal:** The binary opens files in Chrome App Mode or the default browser.

### Step 2.1: Chromium detection

#### `browser/chrome.go`

```go
package browser

import (
    "os"
    "os/exec"
    "path/filepath"
    "runtime"
    "strings"
)

// macOS application paths to check, in order of preference.
var macOSChromePaths = []string{
    "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
    "/Applications/Chromium.app/Contents/MacOS/Chromium",
    "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
}

// CLI names to search in $PATH (cross-platform).
var chromeCLINames = []string{
    "google-chrome",
    "google-chrome-stable",
    "chromium",
    "chromium-browser",
    "microsoft-edge",
}

// FindChromium searches for a Chromium-based browser.
// Returns the path to the browser executable, or empty string if not found.
func FindChromium() string {
    // 1. Explicit override via $MALLEABLE_BROWSER
    if env := os.Getenv("MALLEABLE_BROWSER"); env != "" {
        if _, err := os.Stat(env); err == nil {
            return env
        }
    }

    // 2. $BROWSER if it looks like Chrome/Chromium/Edge
    if env := os.Getenv("BROWSER"); env != "" {
        lower := strings.ToLower(env)
        if strings.Contains(lower, "chrome") ||
            strings.Contains(lower, "chromium") ||
            strings.Contains(lower, "edge") {
            if _, err := exec.LookPath(env); err == nil {
                return env
            }
        }
    }

    // 3. Common CLI names in $PATH
    for _, name := range chromeCLINames {
        if path, err := exec.LookPath(name); err == nil {
            return path
        }
    }

    // 4. Platform-specific application paths
    if runtime.GOOS == "darwin" {
        for _, path := range macOSChromePaths {
            if _, err := os.Stat(path); err == nil {
                return path
            }
        }
    }

    // Future: add Windows %ProgramFiles% checks here
    // Future: add WSL /mnt/c/... checks here

    return ""
}

// LaunchAppMode starts a Chromium browser in app mode (chromeless window).
// profileDir should be ~/.malleable/chrome-profile/.
// Returns the exec.Cmd so the caller can optionally wait on it.
func LaunchAppMode(browserPath, url, profileDir string) (*exec.Cmd, error) {
    // Ensure the profile directory exists
    os.MkdirAll(profileDir, 0755)

    cmd := exec.Command(browserPath,
        "--app="+url,
        "--user-data-dir="+profileDir,
    )

    // Detach from the parent process so it survives if malleable exits
    // (platform-specific, handled by exec defaults on most OS)
    cmd.Stdout = nil
    cmd.Stderr = nil

    if err := cmd.Start(); err != nil {
        return nil, err
    }

    return cmd, nil
}
```

### Step 2.2: Default browser launch

#### `browser/browser_darwin.go`

```go
//go:build darwin

package browser

import "os/exec"

func OpenURL(url string) error {
    return exec.Command("open", url).Run()
}
```

#### `browser/browser_linux.go`

```go
//go:build linux

package browser

import "os/exec"

func OpenURL(url string) error {
    return exec.Command("xdg-open", url).Run()
}
```

#### `browser/browser_windows.go`

```go
//go:build windows

package browser

import "os/exec"

func OpenURL(url string) error {
    // "start" needs an empty title argument when the URL contains special chars
    return exec.Command("cmd", "/c", "start", "", url).Run()
}
```

### Step 2.3: Update `main.go`

Add browser launch after printing the URL. Replace the `_ = mode` placeholder:

```go
    url := fmt.Sprintf("http://127.0.0.1:%d/f/%s", port, f.RelPath)
    logger.Printf("Serving %s at %s", f.Name, url)

    // Launch browser
    switch mode {
    case "app":
        chromePath := browser.FindChromium()
        if chromePath == "" {
            logger.Println("No Chromium browser found, falling back to Browser Mode")
            if err := browser.OpenURL(url); err != nil {
                logger.Printf("Error opening browser: %v", err)
            }
        } else {
            profileDir := filepath.Join(config.Dir(), "chrome-profile")
            if _, err := browser.LaunchAppMode(chromePath, url, profileDir); err != nil {
                logger.Printf("Error launching app mode: %v", err)
                // Fall back to browser mode
                browser.OpenURL(url)
            }
        }
    case "browser":
        if err := browser.OpenURL(url); err != nil {
            logger.Printf("Error opening browser: %v", err)
        }
    }
```

### Testing Phase 2

```bash
# Browser Mode (default for CLI)
./malleable testdata/minimal.malleable
# → should open in your default browser

# App Mode
./malleable --app testdata/minimal.malleable
# → should open in a chromeless Chrome window

# App Mode without Chrome
MALLEABLE_BROWSER=/nonexistent ./malleable --app testdata/minimal.malleable
# → should log "No Chromium browser found" and fall back to default browser

# Full save cycle: edit in the browser, click save, verify file changed on disk
```

---

## Phase 3: Logging

### `logging/logging.go`

```go
package logging

import (
    "fmt"
    "os"
    "sync"
    "time"
)

const maxLogSize = 10 * 1024 * 1024 // 10 MB

type Logger struct {
    mu       sync.Mutex
    file     *os.File
    path     string
    written  int64
}

func New(path string) (*Logger, error) {
    f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
    if err != nil {
        return nil, err
    }

    // Get current file size so we know when to rotate
    info, err := f.Stat()
    if err != nil {
        f.Close()
        return nil, err
    }

    return &Logger{
        file:    f,
        path:    path,
        written: info.Size(),
    }, nil
}

// Printf writes a formatted log line with an ISO 8601 timestamp.
func (l *Logger) Printf(format string, args ...interface{}) {
    msg := fmt.Sprintf(format, args...)
    line := fmt.Sprintf("%s %s\n", time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), msg)

    l.mu.Lock()
    defer l.mu.Unlock()

    l.file.WriteString(line)
    l.written += int64(len(line))

    if l.written >= maxLogSize {
        l.rotate()
    }
}

// rotate renames the current log to .log.1 and opens a fresh log file.
func (l *Logger) rotate() {
    l.file.Close()
    os.Rename(l.path, l.path+".1")
    l.file, _ = os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
    l.written = 0
}

// Close closes the log file.
func (l *Logger) Close() {
    l.mu.Lock()
    defer l.mu.Unlock()
    l.file.Close()
}
```

### Integration with `main.go`

Replace the stdout logger with the file-based logger:

```go
    // Create file-based logger
    logPath := filepath.Join(config.Dir(), "malleable.log")
    logger, err := logging.New(logPath)
    if err != nil {
        fmt.Fprintf(os.Stderr, "Error creating logger: %v\n", err)
        os.Exit(1)
    }
    defer logger.Close()
```

The `server.Server` needs to accept `*logging.Logger` instead of `*log.Logger`. Update the `Server` struct and `New()` function to use the custom logger. The interface is the same — `Printf(format, args...)`.

### HTTP request logging middleware

Add this to `server/server.go`, wrapping the handler chain:

```go
func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        start := time.Now()

        // Wrap ResponseWriter to capture status code
        rw := &responseWriter{ResponseWriter: w, status: 200}
        next.ServeHTTP(rw, r)

        s.logger.Printf("%s %s %d %s",
            r.Method, r.URL.Path, rw.status, time.Since(start))
    })
}

type responseWriter struct {
    http.ResponseWriter
    status int
}

func (rw *responseWriter) WriteHeader(code int) {
    rw.status = code
    rw.ResponseWriter.WriteHeader(code)
}
```

Then in `New()`, wrap the handler chain: `HostValidation → Logging → Mux`.

### `logging/logging_test.go`

```go
// Test: log rotation
// 1. Create a logger with a temp file path
// 2. Write > 10MB of log lines in a loop
// 3. Assert malleable.log exists and is < 10MB
// 4. Assert malleable.log.1 exists

// Test: log format
// 1. Write a log line
// 2. Read the file, assert it matches "2006-01-02T15:04:05.000Z message\n"

// Test: concurrent writes
// 1. Launch 100 goroutines each writing 100 lines
// 2. Run with -race flag, assert no races
// 3. Read the file, assert all lines are intact (no interleaving)
```

---

**MVP is complete after Phase 3.** The `malleable` binary can:
- Serve `.malleable` files with token-based authentication
- Open them in Chrome App Mode or the default browser
- Handle save/read/meta with all security protections
- Log everything to `~/.malleable/malleable.log`

---

## Phase 4: System Tray

### `tray/tray.go`

```go
package tray

import (
    _ "embed"

    "github.com/getlantern/systray"

    "github.com/user/malleable/config"
)

//go:embed icon.png
var iconBytes []byte

type Tray struct {
    cfg         *config.Config
    onQuit      func()
    updateItem  *systray.MenuItem
}

func Run(cfg *config.Config, onQuit func()) {
    t := &Tray{cfg: cfg, onQuit: onQuit}
    systray.Run(t.onReady, t.onExit)
}

func (t *Tray) onReady() {
    systray.SetIcon(iconBytes)
    systray.SetTooltip("Malleable")

    // Update item (hidden by default)
    t.updateItem = systray.AddMenuItem("", "")
    t.updateItem.Hide()
    systray.AddSeparator()

    // Mode toggle
    appItem := systray.AddMenuItemCheckbox("App Mode", "", t.cfg.Mode == "app")
    browserItem := systray.AddMenuItemCheckbox("Browser Mode", "", t.cfg.Mode == "browser")
    systray.AddSeparator()

    // Start on Login
    loginItem := systray.AddMenuItemCheckbox("Start on Login", "", t.cfg.StartOnLogin)
    systray.AddSeparator()

    // Quit
    quitItem := systray.AddMenuItem("Quit", "")

    // Handle clicks in goroutines
    go func() {
        for {
            select {
            case <-appItem.ClickedCh:
                t.cfg.Mode = "app"
                appItem.Check()
                browserItem.Uncheck()
                t.cfg.Save()

            case <-browserItem.ClickedCh:
                t.cfg.Mode = "browser"
                browserItem.Check()
                appItem.Uncheck()
                t.cfg.Save()

            case <-loginItem.ClickedCh:
                t.cfg.StartOnLogin = !t.cfg.StartOnLogin
                if t.cfg.StartOnLogin {
                    loginItem.Check()
                } else {
                    loginItem.Uncheck()
                }
                // platform.SetLoginItem(t.cfg.StartOnLogin) — Phase 6
                t.cfg.Save()

            case <-quitItem.ClickedCh:
                systray.Quit()
            }
        }
    }()
}

func (t *Tray) onExit() {
    t.onQuit()
}

// ShowUpdate shows the update menu item with the given version.
func (t *Tray) ShowUpdate(version, url string) {
    t.updateItem.SetTitle("Update to v" + version + " ↓")
    t.updateItem.Show()
    go func() {
        <-t.updateItem.ClickedCh
        // browser.OpenURL(url) — import and call
    }()
}
```

### Integrate with `main.go`

`systray.Run` blocks the calling goroutine. Restructure `main.go`:

```go
func main() {
    // ... parse flags, resolve file, load config, resolve port ...
    // ... create session manager, register file ...
    // ... create logger ...

    // Start server in background
    srv := server.New(port, sessions, logger)
    go func() {
        if err := srv.Start(); err != nil && err.Error() != "http: Server closed" {
            logger.Printf("Server error: %v", err)
        }
    }()

    // Launch browser
    // ... same as Phase 2 ...

    // Run tray (this blocks the main goroutine)
    tray.Run(cfg, func() {
        // onQuit callback
        logger.Printf("Shutting down...")
        sessions.RevokeAll()
        srv.Shutdown(context.Background())
        logger.Close()
    })
}
```

If no file argument is provided (app started from login item), skip the Register + browser launch but still start the server and tray.

### Tray icon

Create a simple 22x22 PNG icon. On macOS, template images (black on transparent) work best for the menu bar. Name it `tray/icon.png` so the `//go:embed` directive finds it.

---

## Phase 5: Single-Instance + File Handling

### `platform/singleinstance.go`

```go
package platform

// SingleInstance manages ensuring only one app instance runs.
type SingleInstance interface {
    // TryLock attempts to become the primary instance.
    // Returns true if this is the first instance, false if another exists.
    TryLock() (bool, error)

    // SendFilePath sends a file path to the running instance.
    // Only valid when TryLock returned false.
    SendFilePath(path string) error

    // OnFileReceived registers a callback for when another instance
    // sends a file path. Called in a new goroutine for each received path.
    OnFileReceived(callback func(path string))

    // Unlock releases the lock and cleans up. Call on shutdown.
    Unlock() error
}
```

### `platform/singleinstance_darwin.go`

```go
//go:build darwin || linux

package platform

import (
    "bufio"
    "fmt"
    "net"
    "os"
    "path/filepath"
    "strconv"
    "strings"
    "syscall"
)

type socketSingleInstance struct {
    sockPath string
    lockPath string
    listener net.Listener
    callback func(string)
}

func NewSingleInstance(configDir string) SingleInstance {
    return &socketSingleInstance{
        sockPath: filepath.Join(configDir, "sock"),
        lockPath: filepath.Join(configDir, "lock"),
    }
}

func (s *socketSingleInstance) TryLock() (bool, error) {
    // Try to connect to existing socket
    conn, err := net.Dial("unix", s.sockPath)
    if err == nil {
        // Another instance is running
        conn.Close()
        return false, nil
    }

    // Check for stale lock file
    if data, err := os.ReadFile(s.lockPath); err == nil {
        pidStr := strings.TrimSpace(strings.Split(string(data), "\n")[0])
        if pid, err := strconv.Atoi(pidStr); err == nil {
            // Check if process is alive
            process, err := os.FindProcess(pid)
            if err == nil {
                // On Unix, sending signal 0 checks if the process exists
                if process.Signal(syscall.Signal(0)) == nil {
                    // Process is alive but socket is gone — unusual state.
                    // Try to connect one more time in case of race.
                    conn, err := net.Dial("unix", s.sockPath)
                    if err == nil {
                        conn.Close()
                        return false, nil
                    }
                }
            }
        }
        // Stale lock — clean up
        os.Remove(s.sockPath)
        os.Remove(s.lockPath)
    }

    // Remove any leftover socket file
    os.Remove(s.sockPath)

    // Create our socket
    listener, err := net.Listen("unix", s.sockPath)
    if err != nil {
        return false, fmt.Errorf("cannot create socket: %w", err)
    }
    s.listener = listener

    // Write lock file with our PID
    lockData := fmt.Sprintf("%d\n", os.Getpid())
    if err := os.WriteFile(s.lockPath, []byte(lockData), 0644); err != nil {
        listener.Close()
        return false, fmt.Errorf("cannot write lock file: %w", err)
    }

    // Start accepting connections in background
    go s.acceptLoop()

    return true, nil
}

func (s *socketSingleInstance) acceptLoop() {
    for {
        conn, err := s.listener.Accept()
        if err != nil {
            return // Listener was closed
        }
        go func(c net.Conn) {
            defer c.Close()
            scanner := bufio.NewScanner(c)
            if scanner.Scan() {
                path := scanner.Text()
                if s.callback != nil {
                    s.callback(path)
                }
            }
        }(conn)
    }
}

func (s *socketSingleInstance) SendFilePath(path string) error {
    conn, err := net.Dial("unix", s.sockPath)
    if err != nil {
        return err
    }
    defer conn.Close()
    _, err = fmt.Fprintln(conn, path)
    return err
}

func (s *socketSingleInstance) OnFileReceived(callback func(string)) {
    s.callback = callback
}

func (s *socketSingleInstance) Unlock() error {
    if s.listener != nil {
        s.listener.Close()
    }
    os.Remove(s.sockPath)
    os.Remove(s.lockPath)
    return nil
}
```

### Integrate with `main.go`

```go
func main() {
    // ... parse flags, resolve file path ...

    if err := config.EnsureDir(); err != nil { /* ... */ }

    si := platform.NewSingleInstance(config.Dir())
    isPrimary, err := si.TryLock()
    if err != nil { /* ... */ }

    if !isPrimary {
        // Another instance is running — send the file path and exit
        if filePath != "" {
            if err := si.SendFilePath(filePath); err != nil {
                fmt.Fprintf(os.Stderr, "Error sending file to running instance: %v\n", err)
                os.Exit(1)
            }
        }
        os.Exit(0)
    }
    defer si.Unlock()

    // We are the primary instance.
    // ... load config, start server, etc. ...

    // Handle files received from other instances
    si.OnFileReceived(func(path string) {
        absPath, err := filepath.Abs(path)
        if err != nil {
            logger.Printf("Error resolving received path: %v", err)
            return
        }

        // Check if already open
        if f, ok := sessions.LookupByPath(absPath); ok {
            // Focus existing window
            url := fmt.Sprintf("http://127.0.0.1:%d/f/%s", port, f.RelPath)
            browser.OpenURL(url) // or LaunchAppMode depending on config
            return
        }

        // Register and open
        f, err := sessions.Register(absPath)
        if err != nil {
            logger.Printf("Error registering %s: %v", absPath, err)
            return
        }
        url := fmt.Sprintf("http://127.0.0.1:%d/f/%s", port, f.RelPath)
        // Launch browser based on current config mode
        // ... same logic as initial file open ...
    })

    // ... start tray, block ...
}
```

---

## Phase 6: Login Item + Update Check

### `platform/loginitem_darwin.go`

```go
//go:build darwin

package platform

import (
    "fmt"
    "os"
    "path/filepath"
)

const launchAgentLabel = "app.malleable"

func launchAgentPath() string {
    home, _ := os.UserHomeDir()
    return filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist")
}

// SetLoginItem registers or unregisters the app as a macOS login item
// using a LaunchAgent plist.
func SetLoginItem(enabled bool, executablePath string) error {
    path := launchAgentPath()

    if !enabled {
        if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
            return err
        }
        return nil
    }

    plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
</dict>
</plist>`, launchAgentLabel, executablePath)

    // Ensure LaunchAgents directory exists
    os.MkdirAll(filepath.Dir(path), 0755)

    return os.WriteFile(path, []byte(plist), 0644)
}

func IsLoginItem() bool {
    _, err := os.Stat(launchAgentPath())
    return err == nil
}
```

### `update/update.go`

```go
package update

import (
    "encoding/json"
    "net/http"
    "time"
)

const defaultVersionURL = "https://malleable.app/version.json"

type Info struct {
    Version string `json:"latest"`
    URL     string `json:"url"`
}

// Check fetches version info and returns non-nil Info if an update is available.
// Returns (nil, nil) if current version is up to date or if the check fails.
// This is fire-and-forget — errors are silently ignored.
func Check(currentVersion string) *Info {
    client := &http.Client{Timeout: 5 * time.Second}

    resp, err := client.Get(defaultVersionURL)
    if err != nil {
        return nil
    }
    defer resp.Body.Close()

    if resp.StatusCode != 200 {
        return nil
    }

    var info Info
    if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
        return nil
    }

    if info.Version == "" || info.Version == currentVersion {
        return nil
    }

    // Simple string comparison works for "1.0" < "1.1" < "1.2" etc.
    // For proper semver, use a library — but this is fine for v1.
    if info.Version > currentVersion {
        return &info
    }

    return nil
}
```

Wire into tray startup:

```go
    // In main.go, after tray starts, check for updates
    go func() {
        if info := update.Check("1.0"); info != nil {
            trayInstance.ShowUpdate(info.Version, info.URL)
        }
    }()
```

---

## Phase 7: macOS App Bundle

### `dist/macos/Info.plist`

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>CFBundleIdentifier</key>
    <string>app.malleable.Malleable</string>
    <key>CFBundleName</key>
    <string>Malleable</string>
    <key>CFBundleDisplayName</key>
    <string>Malleable</string>
    <key>CFBundleExecutable</key>
    <string>malleable</string>
    <key>CFBundleVersion</key>
    <string>1.0</string>
    <key>CFBundleShortVersionString</key>
    <string>1.0</string>
    <key>CFBundleIconFile</key>
    <string>malleable</string>
    <key>CFBundlePackageType</key>
    <string>APPL</string>
    <key>LSMinimumSystemVersion</key>
    <string>11.0</string>
    <key>LSUIElement</key>
    <true/>
    <key>CFBundleDocumentTypes</key>
    <array>
        <dict>
            <key>CFBundleTypeName</key>
            <string>Malleable HTML File</string>
            <key>CFBundleTypeExtensions</key>
            <array>
                <string>malleable</string>
            </array>
            <key>CFBundleTypeRole</key>
            <string>Editor</string>
            <key>LSHandlerRank</key>
            <string>Owner</string>
            <key>CFBundleTypeIconFile</key>
            <string>doc</string>
        </dict>
    </array>
</dict>
</plist>
```

Key fields:
- `LSUIElement: true` — hides from Dock (tray-only app)
- `CFBundleDocumentTypes` — registers `.malleable` file association
- `LSHandlerRank: Owner` — this app is the preferred handler

### `dist/macos/build.sh`

```bash
#!/bin/bash
set -euo pipefail

APP="Malleable.app"
CONTENTS="$APP/Contents"
MACOS="$CONTENTS/MacOS"
RESOURCES="$CONTENTS/Resources"

# Clean
rm -rf "$APP"

# Build the Go binary
CGO_ENABLED=1 go build -o "$MACOS/malleable" .

# Assemble bundle
mkdir -p "$RESOURCES"
cp dist/macos/Info.plist "$CONTENTS/"
echo -n "APPL????" > "$CONTENTS/PkgInfo"

# Copy icons (if they exist)
[ -f dist/macos/malleable.icns ] && cp dist/macos/malleable.icns "$RESOURCES/"
[ -f dist/macos/doc.icns ] && cp dist/macos/doc.icns "$RESOURCES/"

# Sign (optional, requires Apple Developer identity)
# codesign --sign "Developer ID Application: Your Name" --deep "$APP"

echo "Built $APP"
```

### `Makefile`

```makefile
.PHONY: build test clean dist-macos

build:
	go build -o malleable .

test:
	go test ./... -race -count=1

clean:
	rm -f malleable
	rm -rf Malleable.app

dist-macos:
	bash dist/macos/build.sh
```

---

## Testing Strategy

| Layer | Tool | What |
|---|---|---|
| Unit | `go test ./...` | Token generation, HTML inject/strip, path validation, host validation, config, log rotation, update check |
| Integration | `httptest` | Full HTTP cycle for all 4 endpoints, error cases |
| Manual | `curl` + browser | End-to-end serve → edit → save → verify on disk |
| Platform | Manual on macOS | Tray, menu, file association, single-instance, login item, Chrome app mode |

### Key test scenarios

1. **Round-trip integrity**: Serve → save without changes → file is byte-for-byte identical (appname stripped).
2. **Concurrent saves**: Two rapid POSTs to same token → no corruption (atomic write).
3. **Path traversal**: `../../../etc/passwd`, URL-encoded variants.
4. **Host header attacks**: Wrong host, wrong port, missing host.
5. **50MB limit**: Exactly 50MB succeeds, 50MB+1 returns 413.
6. **Token isolation**: Token A cannot access Token B's file.
7. **Unregistered file**: `GET /f/not-registered.malleable` returns 404.
8. **Existing appname**: File already has `appname="something"` → replaced, not duplicated.

### Test data files

#### `testdata/minimal.malleable`

```html
<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>Test File</title>
</head>
<body>
  <h1 contenteditable>Edit me</h1>
  <button id="save">Save</button>
  <script>
    const name = document.documentElement.getAttribute('appname');
    document.getElementById('save').addEventListener('click', async () => {
      const html = '<!DOCTYPE html>\n' + document.documentElement.outerHTML;
      const res = await fetch('/save/' + name, {
        method: 'POST',
        headers: { 'Content-Type': 'text/html' },
        body: html
      });
      if (res.ok) document.title = 'Saved!';
    });
  </script>
</body>
</html>
```

#### `testdata/with-appname.malleable`

```html
<!DOCTYPE html>
<html appname="old-value" lang="en">
<head><title>Has Appname</title></head>
<body><p>This file already has appname on the html tag.</p></body>
</html>
```

---

## Cross-Platform Notes

| Feature | macOS (now) | Linux (future) | Windows (future) |
|---|---|---|---|
| Default browser | `open URL` | `xdg-open URL` | `cmd /c start URL` |
| Chromium detection | `/Applications/*.app` paths | PATH search | `%ProgramFiles%` paths |
| Single-instance | Unix socket (dev) / Apple Events (bundle) | Unix socket + lock file | Named mutex + named pipe |
| Login item | LaunchAgent plist | `.desktop` in `~/.config/autostart/` | Registry key |
| System tray | `systray` (CGO, native NSStatusItem) | `systray` (libayatana) | `systray` (Win32) |
| File association | `Info.plist` in app bundle | `.desktop` + mime database | Registry keys |
| Config dir | `~/.malleable/` | `~/.malleable/` | `%APPDATA%\Malleable\` |

**Key rules:**
- All platform-specific code in `browser/` and `platform/` with build-tagged filenames.
- Use `os.UserHomeDir()`, never hardcode `$HOME`.
- Use `filepath.Join` and `filepath.Separator` everywhere.
- `server/`, `session/`, `htmlutil/`, `config/`, `logging/`, `update/` are 100% cross-platform.

---

## Risk Areas

1. **CGO for systray on macOS**: Requires macOS build machine or CI runner. Can't cross-compile from Linux.
2. **Chrome profile conflicts**: If Malleable crashes, Chrome may show "profile in use" dialog. Mitigation: check for and remove stale Chrome lock files in `~/.malleable/chrome-profile/SingletonLock` on startup.
3. **HTML regex fragility**: Could break on unusual HTML (`<html` split across lines, inside comments). Mitigation: extensive test cases targeting only the first `<html` tag.
4. **Atomic writes on Windows** (future): `os.Rename` fails across drives. Mitigation: write temp file in same directory as target (already implemented this way).
5. **Port taken on restart**: Saved port may be occupied. Mitigation: try saved port first, pick new one if `net.Listen` fails (already implemented this way).
