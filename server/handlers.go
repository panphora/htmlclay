package server

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/panphora/htmlclay/htmlutil"
	"github.com/panphora/htmlclay/session"
	"github.com/panphora/htmlclay/versions"
)

const maxSaveSize = 50 * 1024 * 1024

type fileMeta struct {
	Path         string `json:"path"`
	AbsolutePath string `json:"absolutePath"`
	Name         string `json:"name"`
	Size         int64  `json:"size"`
	LastModified string `json:"lastModified"`
	HTMLClayID   string `json:"htmlclayid,omitempty"`
}

func (s *Server) lookupSession(w http.ResponseWriter, r *http.Request) (*session.File, bool) {
	token := r.PathValue("token")
	f, ok := s.sessions.Lookup(token)
	if !ok {
		s.writeError(w, http.StatusUnauthorized, "invalid token")
		return nil, false
	}
	return f, true
}

func extractFilePath(rawPath string) string {
	lower := strings.ToLower(rawPath)
	for _, suffix := range []string{".htmlclay", ".html"} {
		if idx := strings.Index(lower, suffix); idx >= 0 {
			end := idx + len(suffix)
			if end == len(rawPath) || rawPath[end] == '/' {
				return rawPath[:end]
			}
		}
	}
	return rawPath
}

func (s *Server) handleServeFile(w http.ResponseWriter, r *http.Request) {
	relPath := extractFilePath(r.PathValue("path"))

	absPath, err := ValidatePath(relPath, s.sessions.HomeDir())
	if err != nil {
		s.logger.Printf("Invalid path %q: %v", relPath, err)
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}

	resolved, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	absPath = filepath.Clean(resolved)

	// Backups are internal state and are never served on the app's own origin.
	// The config directory sits under the user's home on every platform, so this
	// path would otherwise be reachable from a page opened next to it.
	if s.versions.Contains(absPath) {
		s.logger.Printf("Denying request for internal versions path %s", absPath)
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}

	f, ok := s.sessions.LookupByPath(absPath)
	if !ok {
		s.serveAsset(w, r, r.PathValue("path"))
		return
	}

	f.Lock()
	data, err := os.ReadFile(f.AbsPath)
	if err != nil {
		f.Unlock()
		s.logger.Printf("Error reading %s: %v", f.AbsPath, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// firstServe is captured before any record is touched, so both the clone check
	// and the first-open snapshot still see a genuinely fresh file even after the
	// htmlclayid injection below advances the records.
	firstServe := !f.Observed()
	serverWrote := false

	// Only .htmlclay files carry a persistent identity; a plain .html file
	// opened for viewing is never modified on disk.
	if strings.EqualFold(filepath.Ext(f.AbsPath), ".htmlclay") && htmlutil.ReadHTMLClayID(data) == "" {
		id, err := htmlutil.GenerateHTMLClayID()
		if err != nil {
			s.logger.Printf("Error generating htmlclayid: %v", err)
		} else {
			data = htmlutil.InjectHTMLClayID(data, id)
			if wErr := atomicWriteFile(f.AbsPath, data); wErr != nil {
				s.logger.Printf("Error persisting htmlclayid for %s: %v", f.AbsPath, wErr)
			} else {
				serverWrote = true
				s.logger.Printf("Assigned htmlclayid %s to %s", id, f.RelPath)
			}
		}
	}

	if firstServe {
		var forked bool
		data, forked = s.resolveIdentityOnFirstOpen(f, data)
		serverWrote = serverWrote || forked
	}

	// The file's backup identity is resolved exactly once, here, after injection
	// and clone resolution have settled what identity the file actually carries.
	// Every later list, read, restore and save reads this stored key instead of
	// re-deriving one from bytes an external process may have changed.
	f.SetHistoryKey(versions.Key(f.AbsPath, data))
	key := f.HistoryKey()

	// The injection is a server write, so it advances both records on every
	// injection and not only the first. Guarding the update by firstServe meant
	// that an external editor stripping the id, followed by a reload, had the
	// server inject a fresh id, write disk, leave the records untouched, and then
	// broadcast its own write as a foreign edit and warn stale against itself.
	// Serving on its own still never advances anything.
	if firstServe || serverWrote {
		f.RecordServerWrite(versions.Hash(data))
	}

	// B1a: capture a version when a file is first served, only if it differs from
	// the newest existing backup. Without this, a freshly opened file that has
	// never been saved has nothing to restore.
	//
	// Published inside f.Lock(), per B1. Publishing after the unlock let two
	// concurrent GETs interleave: GET1 captured H0 and was descheduled, GET2 saw
	// the file as observed and returned a token with no snapshot work, a save
	// published H0 then H1, and GET1 then published its stale H0, leaving history
	// ending at H0 after a successful H1 save. Two tabs opening one file at once
	// is ordinary.
	pruneKey := ""
	if firstServe {
		if _, bErr := s.versions.Backup(key, f.AbsPath, data); bErr != nil {
			s.logger.Printf("First-open snapshot failed for %s: %v", f.RelPath, bErr)
		}
		pruneKey = key
	}

	// A server-authorized write changes the file's inode via the atomic rename;
	// re-anchor the live-sync incarnation to it so the change is not later mistaken
	// for an external reincarnation. A no-op when nothing is streaming this file.
	if serverWrote {
		s.coord.acceptServerReplacement(f)
	}
	f.Unlock()

	// Bulk pruning runs on the store lock only, never inside f.Lock().
	if pruneKey != "" {
		s.versions.MaybePrune(pruneKey, f.AbsPath)
	}

	data = htmlutil.InjectToken(data, f.Token)

	// B0: edit mode via cookie, matching hyperclay-local. Both clients fall back
	// to exactly this cookie, read synchronously from document.cookie, and the
	// response cookie arrives before scripts execute. Host-only (no Domain).
	http.SetCookie(w, &http.Cookie{
		Name:     "isAdminOfCurrentResource",
		Value:    "true",
		Path:     "/",
		SameSite: http.SameSiteLaxMode,
		HttpOnly: false,
		Secure:   false,
	})

	// B6: tokens are per-process, so any cache validator means a 304 after a
	// restart hands back a dead token and every save 401s silently.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

// resolveIdentityOnFirstOpen makes clone identity self-healing, and reports
// whether it wrote to disk.
//
// Ownership is checked and claimed in one store transaction. Doing it in two let
// two copies of one file, first-opened concurrently, both see no owner, so
// neither got a fresh id and both landed in a single history.
//
// Caller must hold f.Lock().
func (s *Server) resolveIdentityOnFirstOpen(f *session.File, data []byte) ([]byte, bool) {
	if !strings.EqualFold(filepath.Ext(f.AbsPath), ".htmlclay") {
		return data, false
	}
	id := htmlutil.ReadHTMLClayID(data)
	if !versions.IsCanonicalUUID(id) {
		return data, false
	}

	status, bound, err := s.versions.Claim(versions.Key(f.AbsPath, data), f.AbsPath)
	if err != nil {
		s.logger.Printf("Could not claim history for %s: %v", f.RelPath, err)
		return data, false
	}
	switch status {
	case versions.ClaimOwned:
		return data, false
	case versions.ClaimRenamed:
		s.logger.Printf("Rebound history %s from %s to %s", id, bound, f.AbsPath)
		return data, false
	}

	newID, err := htmlutil.GenerateHTMLClayID()
	if err != nil {
		s.logger.Printf("Error generating htmlclayid for clone %s: %v", f.RelPath, err)
		return data, false
	}
	forked := htmlutil.SetHTMLClayID(data, newID)
	if wErr := atomicWriteFile(f.AbsPath, forked); wErr != nil {
		s.logger.Printf("Error forking htmlclayid for %s: %v", f.AbsPath, wErr)
		return data, false
	}
	// Claim the fresh identity in the same breath, so a second clone opened
	// concurrently sees this one as the owner rather than racing for the same id.
	if _, _, cErr := s.versions.Claim(versions.Key(f.AbsPath, forked), f.AbsPath); cErr != nil {
		s.logger.Printf("Could not claim forked history for %s: %v", f.RelPath, cErr)
	}
	s.logger.Printf("Detected clone of %s: assigned fresh htmlclayid %s to %s", bound, newID, f.RelPath)
	return forked, true
}

// serveAsset serves a file that was never opened directly: an asset (css, js,
// image) or a linked page referenced by an opened file. Allowed only under the
// folder tree of an opened file, and served without token injection, so linked
// pages cannot save. rawPath is the request path before extractFilePath
// truncation, so asset paths containing ".html" in a directory name stay intact.
func (s *Server) serveAsset(w http.ResponseWriter, r *http.Request, rawPath string) {
	absPath, err := ValidatePath(rawPath, s.sessions.HomeDir())
	if err != nil {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}

	resolved, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	absPath = filepath.Clean(resolved)

	// serveAsset resolves the untruncated request path, so the internal-directory
	// denial is rechecked here rather than inherited from the caller.
	if s.versions.Contains(absPath) {
		s.logger.Printf("Denying request for internal versions path %s", absPath)
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}

	root, rel, ok := s.sessions.AssetRoot(absPath)
	if !ok {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}

	// Re-walk the already-resolved path through os.Root so a component swapped
	// for a symlink between authorization and open cannot escape the root.
	rt, err := os.OpenRoot(root)
	if err != nil {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	defer rt.Close()

	file, err := rt.Open(rel)
	if err != nil {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}

	name := filepath.Base(absPath)

	// B8: assets always revalidate. Detailed failure causes go in the log; the
	// response bodies above stay coarse.
	etag, err := assetETag(file, info)
	if err != nil {
		s.logger.Printf("Error computing ETag for %s: %v", absPath, err)
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", etag)

	// B7: explicit .br/.gz sidecar metadata only, never generic negotiation.
	if encoding, inner, ok := sidecarEncoding(name); ok {
		s.serveEncodedSidecar(w, r, inner, encoding, etag, info, file)
		return
	}

	http.ServeContent(w, r, name, info.ModTime(), file)
}

// maxETagHashSize bounds the bytes hashed to build a content ETag. Above it the
// validator falls back to metadata, which is documented below.
const maxETagHashSize = 32 * 1024 * 1024

// assetETag derives the validator from the asset's content rather than from its
// mtime and size. A metadata-only ETag returned 304 for a same-size replacement
// with a preserved timestamp, so the browser kept stale bytes, while the watcher
// path explicitly accounts for exactly that replacement pattern: the two
// disagreed about whether the file had changed.
//
// Above maxETagHashSize the metadata form is kept deliberately. Hashing an
// arbitrarily large asset on every conditional request costs more than the stale
// window is worth, and assets that big are media, not the hand-edited HTML and
// CSS the replacement pattern applies to.
func assetETag(file *os.File, info os.FileInfo) (string, error) {
	if info.Size() > maxETagHashSize {
		return fmt.Sprintf(`"m%x-%x"`, info.ModTime().UnixNano(), info.Size()), nil
	}
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return "", err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	return fmt.Sprintf(`"c%x-%x"`, info.Size(), h.Sum(nil)[:16]), nil
}

// sidecarEncoding recognizes an explicitly requested pre-compressed sidecar. Only
// these two suffixes are recognized, and only when the URL names them directly.
func sidecarEncoding(name string) (encoding, inner string, ok bool) {
	switch {
	case strings.HasSuffix(name, ".br"):
		return "br", strings.TrimSuffix(name, ".br"), true
	case strings.HasSuffix(name, ".gz"):
		return "gzip", strings.TrimSuffix(name, ".gz"), true
	}
	return "", "", false
}

// serveEncodedSidecar serves a pre-compressed asset with the Content-Encoding it
// actually has, and a Content-Type derived from the inner extension. This is the
// bug that started the thread: htmlclay served a .br sidecar without
// Content-Encoding, and the client read compressed bytes as a mesh header.
//
// http.ServeContent is skipped deliberately. It sniffs Content-Type from the
// compressed bytes and negotiates Range against the encoded stream. Range is
// declined instead: Accept-Ranges is never advertised for an encoded sidecar, so
// a Range header here is unsolicited and the full representation is returned.
// Dropping Content-Encoding to satisfy a Range would reintroduce the original bug.
func (s *Server) serveEncodedSidecar(w http.ResponseWriter, r *http.Request, inner, encoding, etag string, info os.FileInfo, file io.Reader) {
	ctype := mime.TypeByExtension(filepath.Ext(inner))
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Encoding", encoding)
	w.Header().Set("Accept-Ranges", "none")

	if r.Header.Get("Range") != "" {
		s.logger.Printf("Declining Range on encoded sidecar %s; serving full representation", inner)
	}

	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	if _, err := io.Copy(w, file); err != nil {
		s.logger.Printf("Error writing sidecar %s: %v", inner, err)
	}
}

func etagMatches(header, etag string) bool {
	if header == "" {
		return false
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == etag || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}

func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) {
	f, ok := s.lookupSession(w, r)
	if !ok {
		return
	}

	data, err := os.ReadFile(f.AbsPath)
	if err != nil {
		s.logger.Printf("Error reading %s: %v", f.AbsPath, err)
		s.writeError(w, http.StatusInternalServerError, "read error")
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(data)
}

func (s *Server) handleSave(w http.ResponseWriter, r *http.Request) {
	f, ok := s.lookupSession(w, r)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxSaveSize)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			s.writeError(w, http.StatusRequestEntityTooLarge, "body too large (max 50MB)")
			return
		}
		s.writeError(w, http.StatusInternalServerError, "read error")
		return
	}

	if len(body) == 0 {
		s.writeError(w, http.StatusBadRequest, "empty body")
		return
	}

	// hyperclayjs sends a JSON {content, snapshotHtml} body when a live-sync
	// snapshot is present (it treats 127.0.0.1 as a local host). Persist only
	// content; snapshotHtml is for a future live-sync broadcast htmlclay does
	// not yet implement. Any non-JSON body is the raw HTML, written as-is.
	if isJSONContentType(r.Header.Get("Content-Type")) {
		var payload struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			s.writeError(w, http.StatusBadRequest, "invalid json body")
			return
		}
		if payload.Content == "" {
			s.writeError(w, http.StatusBadRequest, "empty content")
			return
		}
		body = []byte(payload.Content)
	}

	body = htmlutil.StripToken(body)

	// A valid save is always a full document (the browser serializes
	// documentElement.outerHTML). Reject anything without an <html> tag so a
	// stray request cannot overwrite the file with a fragment or junk.
	if !htmlutil.HasHTMLTag(body) {
		s.writeError(w, http.StatusBadRequest, "body is not an HTML document")
		return
	}

	f.Lock()
	current, readErr := os.ReadFile(f.AbsPath)

	// The backup identity comes from the stored key, never from the bytes that
	// happen to be on disk right now. Re-deriving it here meant that an external
	// process deleting the file or stripping its htmlclayid silently moved the key
	// to a path hash, so this save's backups went somewhere the versions API would
	// never list, while the warning told the user their previous version was in
	// Backups. A file saved before it was ever served resolves its key here.
	f.SetHistoryKey(versions.Key(f.AbsPath, current))
	key := f.HistoryKey()

	// B5: compare the on-disk hash against lastServerWrite. Hashing the on-disk
	// bytes on both sides sidesteps the token inject/strip round-trip entirely.
	// The notice cannot tell two tabs apart, because lastServerWrite advanced on
	// the first tab's write. Backups are the actual safety net.
	stale := false
	if readErr == nil {
		currentHash := versions.Hash(current)
		if !f.NoteFirstObservation(currentHash) && f.LastServerWrite() != currentHash {
			stale = true
		}
	}

	// B1: version the existing content on the first save of a file, so the
	// pre-Hyperclay state survives, then version the INCOMING bytes before writing
	// them. Versioning the outgoing pre-write bytes would mean the most recent
	// successful save is the one state never written to history, so an external
	// clobber would destroy exactly the version you would want back.
	//
	// A stale write is the other case where the on-disk bytes must be versioned
	// first: that content came from outside, so nothing else has ever recorded it,
	// and this save is about to clobber it. Backups are the actual safety net
	// behind the warning.
	if readErr == nil && (stale || !s.versions.HasHistory(key, f.AbsPath)) {
		if _, bErr := s.versions.Backup(key, f.AbsPath, current); bErr != nil {
			s.logger.Printf("Pre-write backup failed for %s: %v", f.RelPath, bErr)
		}
	}
	// Backup failure never fails a normal save.
	if _, bErr := s.versions.Backup(key, f.AbsPath, body); bErr != nil {
		s.logger.Printf("Backup failed for %s: %v", f.RelPath, bErr)
	}

	err = atomicWriteFile(f.AbsPath, body)
	if err == nil {
		f.RecordServerWrite(versions.Hash(body))
		s.coord.acceptServerReplacement(f)
		s.broadcastDiskHTML(f, body)
	}
	f.Unlock()

	if err != nil {
		s.logger.Printf("Error saving %s: %v", f.AbsPath, err)
		s.writeError(w, http.StatusInternalServerError, "write error")
		return
	}

	// Bulk pruning runs on the store lock only, never inside f.Lock().
	s.versions.MaybePrune(key, f.AbsPath)

	s.logger.Printf("Saved %s (%d bytes)", f.RelPath, len(body))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if stale {
		s.logger.Printf("Stale write: %s changed on disk since this server last wrote it", f.RelPath)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":      true,
			"msg":     f.Name + " had been changed outside this tab. Your version was saved; the previous one is in Backups.",
			"msgType": "warning",
		})
		return
	}
	w.Write([]byte(`{"ok":true,"msg":"Saved","msgType":"success"}`))
}

func isJSONContentType(ct string) bool {
	if ct == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	return mediaType == "application/json"
}

func atomicWriteFile(targetPath string, data []byte) error {
	dir := filepath.Dir(targetPath)
	tmp, err := os.CreateTemp(dir, ".htmlclay-save-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()

	defer func() {
		if tmpPath != "" {
			os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	if info, err := os.Stat(targetPath); err == nil {
		os.Chmod(tmpPath, info.Mode())
	}

	if err := os.Rename(tmpPath, targetPath); err != nil {
		return fmt.Errorf("rename to target: %w", err)
	}

	tmpPath = ""

	// Fsync the directory so the rename is durable: without this, a crash right
	// after a successful save can revert the file to its previous contents. The
	// error is returned rather than discarded, because a save that cannot be made
	// durable must not be acknowledged as one.
	if err := versions.SyncDir(dir); err != nil {
		return fmt.Errorf("sync directory: %w", err)
	}

	return nil
}

func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	f, ok := s.lookupSession(w, r)
	if !ok {
		return
	}

	info, err := os.Stat(f.AbsPath)
	if err != nil {
		s.logger.Printf("Error stat %s: %v", f.AbsPath, err)
		s.writeError(w, http.StatusInternalServerError, "stat error")
		return
	}

	var htmlclayID string
	if data, err := os.ReadFile(f.AbsPath); err == nil {
		htmlclayID = htmlutil.ReadHTMLClayID(data)
	}

	meta := fileMeta{
		Path:         f.RelPath,
		AbsolutePath: f.AbsPath,
		Name:         f.Name,
		Size:         info.Size(),
		LastModified: info.ModTime().UTC().Format(time.RFC3339),
		HTMLClayID:   htmlclayID,
	}

	noStoreJSON(w)
	json.NewEncoder(w).Encode(meta)
}

func (s *Server) writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":    false,
		"error": message,
	})
}
