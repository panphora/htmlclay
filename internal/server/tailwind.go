package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/panphora/htmlclay/internal/platform"
	"github.com/panphora/htmlclay/internal/session"
	"github.com/panphora/htmlclay/internal/tailwind"
)

// maxTailwindSource bounds the document bytes one compile reads. A document
// larger than this is not compiled: the stylesheet comes back empty rather than
// reading an unbounded file into memory on a subresource request.
const maxTailwindSource = 50 << 20

// tailwindCompileMu serializes compiles across every Server in the process: the
// app runs one Server per folder, and each compile is a ~100 MB CLI process.
var tailwindCompileMu sync.Mutex

// setTailwindCompiler replaces the compiler. Tests only; production injects
// compileTailwind in newServer.
func (s *Server) setTailwindCompiler(f func(context.Context, []byte) ([]byte, error)) {
	s.tailwindCompile = f
}

// compileTailwind builds a document's stylesheet with the binary this build
// carries, extracted under the internal directory on first use. It is a method
// so it sees an internal directory set after the server is built.
func (s *Server) compileTailwind(ctx context.Context, html []byte) ([]byte, error) {
	root := ""
	if s.internalDir != "" {
		root = filepath.Join(s.internalDir, "tailwind", "bin")
	}
	return tailwind.CompileDocument(ctx, root, html)
}

// handleTailwind serves the Tailwind stylesheet a document links as
// /tailwindcss/<name>.css. The document is the page that asked (Referer),
// else <name>.html, else <name>.htmlclay. Only a registered document, or one
// already inside an opened folder, is ever read: a stylesheet request never
// prompts for access. The document is never modified.
func (s *Server) handleTailwind(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("path")
	if !strings.HasSuffix(strings.ToLower(name), ".css") {
		r.SetPathValue("path", "tailwindcss/"+name)
		s.handleServeFile(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Vary", "Referer")

	absPath, html, ok := s.tailwindSource(r)
	if !ok {
		writeEmptyCSS(w)
		return
	}
	fp := tailwind.Fingerprint(html)
	etag := `"` + fp[:32] + `"`
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	css, _, err := s.tailwindStylesheet(context.WithoutCancel(r.Context()), absPath, html)
	if err != nil {
		writeEmptyCSS(w)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", etag)
	w.Write(css)
}

// writeEmptyCSS answers a stylesheet request with nothing at all, and says so
// uncacheably: a document that gains its document, or a compiler that starts
// working, must not be answered from a cached empty stylesheet.
func writeEmptyCSS(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}

// tailwindSource finds the document a stylesheet request is about, and reads it.
// The Referer is trusted only when it names this very host: a stylesheet request
// carries no token, so the address of the page that asked is the only thing that
// says which document's HTML the compile should see.
func (s *Server) tailwindSource(r *http.Request) (absPath string, html []byte, ok bool) {
	var candidates []string

	refHost := RefererHost(r)
	if refHost != "" && refHost == strings.ToLower(r.Host) {
		if u, err := url.Parse(r.Header.Get("Referer")); err == nil {
			// u.Path is already decoded once by url.Parse. Decoding again would let
			// %252e%252e walk out of the folder the path names.
			rel := strings.TrimPrefix(u.Path, "/")
			rel = extractFilePath(rel)
			lower := strings.ToLower(rel)
			if strings.HasSuffix(lower, ".html") || strings.HasSuffix(lower, ".htmlclay") {
				candidates = append(candidates, rel)
			}
		}
	}

	name := r.PathValue("path")
	if !strings.HasSuffix(strings.ToLower(name), ".css") {
		return "", nil, false
	}
	base := name[:len(name)-len(".css")]
	candidates = append(candidates, base+".html", base+".htmlclay")

	for _, rel := range candidates {
		if absPath, html, ok := s.readTailwindSource(rel); ok {
			return absPath, html, true
		}
	}
	return "", nil, false
}

// readTailwindSource reads rel, relative to home, only when it is already
// readable without asking: a registered document, or a file inside a read root
// the user already opened or granted. Everything serveAsset checks is checked
// here too, so a stylesheet request can never read internal state, a hidden
// file, or anything outside a root, and can never park on the broker.
func (s *Server) readTailwindSource(rel string) (absPath string, html []byte, ok bool) {
	home := s.sessions.HomeDir()
	absPath, err := ValidatePath(rel, home)
	if err != nil {
		return "", nil, false
	}
	if s.isInternal(absPath) || session.HasHiddenComponent(home, absPath) {
		return "", nil, false
	}
	lexical := absPath
	resolved, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return "", nil, false
	}
	absPath = filepath.Clean(resolved)
	if s.isInternal(absPath) || session.HasHiddenComponent(home, absPath) {
		return "", nil, false
	}

	// A registered document is read the way every other registered read is, so the
	// descriptor-bound internal and hidden checks apply here as well.
	if f, ok := s.sessions.LookupByPath(absPath); ok {
		f.Lock()
		data, err := s.readTailwindRegistered(f.AbsPath)
		f.Unlock()
		if err != nil {
			return "", nil, false
		}
		return f.AbsPath, data, true
	}

	if _, _, inScope := s.sessions.AssetRoot(lexical); !inScope {
		return "", nil, false
	}
	if _, _, inScope := s.sessions.AssetRoot(absPath); !inScope {
		return "", nil, false
	}
	file, authorized, err := s.sessions.OpenAsset(absPath)
	if err != nil || !authorized {
		return "", nil, false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", nil, false
	}
	real, err := platform.RealPath(file)
	if err != nil || s.isInternal(real) || session.HasHiddenComponent(home, real) {
		return "", nil, false
	}
	data, err := io.ReadAll(io.LimitReader(file, maxTailwindSource+1))
	if err != nil || len(data) > maxTailwindSource {
		return "", nil, false
	}
	return absPath, data, true
}

// readTailwindRegistered reads a registered document through the descriptor check
// every other registered read uses, bounded so a document larger than one compile
// accepts is refused instead of read into memory.
func (s *Server) readTailwindRegistered(absPath string) ([]byte, error) {
	file, err := s.openRegisteredFile(absPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxTailwindSource {
		return nil, errors.New("tailwind: not a regular document of acceptable size")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxTailwindSource+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxTailwindSource {
		return nil, errors.New("tailwind: document too large")
	}
	return data, nil
}

// tailwindStylesheet returns the stylesheet for html, compiling it only when the
// on-disk cache does not already hold this exact fingerprint. Compiles are
// serialized process-wide: each one is a process, and two documents loading at
// once must not start two.
func (s *Server) tailwindStylesheet(ctx context.Context, absPath string, html []byte) (css []byte, fp string, err error) {
	fp = tailwind.Fingerprint(html)
	cachePath := s.tailwindCachePath(absPath)
	if cachePath != "" {
		if css, ok := readTailwindCache(cachePath, fp); ok {
			return css, fp, nil
		}
	}
	tailwindCompileMu.Lock()
	defer tailwindCompileMu.Unlock()
	// A request that queued behind the compile of this same version finds it cached.
	if cachePath != "" {
		if css, ok := readTailwindCache(cachePath, fp); ok {
			return css, fp, nil
		}
	}
	css, err = s.tailwindCompile(ctx, html)
	if err != nil {
		s.logTailwindFailure(ctx, absPath, fp, err)
		return nil, fp, err
	}
	if cachePath != "" {
		if err := writeTailwindCache(cachePath, fp, css); err != nil {
			s.logger.Printf("Tailwind: cache write failed: %v", err)
		}
	}
	return css, fp, nil
}

// logTailwindFailure records a compile failure once: a compile whose context has
// ended is not a failure to report, a build with no compiler is one line however
// many documents ask, and every other failure is one line per document version.
func (s *Server) logTailwindFailure(ctx context.Context, absPath, fp string, err error) {
	if ctx.Err() != nil {
		return
	}
	key := absPath + "\x00" + fp
	if errors.Is(err, tailwind.ErrUnavailable) {
		key = "unavailable"
	}
	s.tailwindMu.Lock()
	defer s.tailwindMu.Unlock()
	if s.tailwindLogged[key] {
		return
	}
	s.tailwindLogged[key] = true
	s.logger.Printf("Tailwind: %s: %v", absPath, err)
}

// tailwindCachePath names the one file a document's stylesheet is cached in, or
// "" when this server has no internal directory to keep it in.
func (s *Server) tailwindCachePath(absPath string) string {
	if s.internalDir == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(absPath))
	return filepath.Join(s.internalDir, "tailwind", "cache", hex.EncodeToString(sum[:])+".css")
}

// readTailwindCache returns the cached stylesheet only when it was compiled from
// this exact fingerprint, so a document edit, a new Tailwind version or a
// changed plugin all miss rather than serve a stale sheet.
func readTailwindCache(path, fp string) ([]byte, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	nl := strings.IndexByte(string(data), '\n')
	if nl < 0 || string(data[:nl]) != fp {
		return nil, false
	}
	return data[nl+1:], true
}

// writeTailwindCache writes through a temp file in the same directory and renames
// it onto path, so a concurrent reader never sees a half-written stylesheet.
func writeTailwindCache(path, fp string, css []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tw-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(name)
	}
	if _, err := tmp.WriteString(fp + "\n"); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(css); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}
