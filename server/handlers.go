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
	"github.com/panphora/htmlclay/platform"
	"github.com/panphora/htmlclay/session"
	"github.com/panphora/htmlclay/versions"
)

// errRefusedInternal marks a registered-file read whose descriptor, once opened,
// resolves outside home or into htmlclay's own hidden/internal state (a symlink
// swapped in under the file's pathname). Callers turn it into a 404/403 rather than
// a 500 and never serve the bytes.
var errRefusedInternal = errors.New("registered file resolves to a refused location")

// openRegisteredFile opens a registered file for reading and enforces the
// "never serve internal state" invariant against the OPEN DESCRIPTOR rather than the
// pathname. os.Open follows any symlink swapped in under absPath, but platform.RealPath
// then reports where the held inode really lives, so a component swapped for a symlink
// into the config/versions tree (or out of home, or into a dotfile) is caught no
// matter how the pathname is raced. Caller closes the returned file.
func (s *Server) openRegisteredFile(absPath string) (*os.File, error) {
	f, err := os.Open(absPath)
	if err != nil {
		return nil, err
	}
	real, err := platform.RealPath(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	real = filepath.Clean(real)
	home := s.sessions.HomeDir()
	if _, ok := session.ContainWithinHome(home, real); !ok || s.isInternal(real) || session.HasHiddenComponent(home, real) {
		f.Close()
		return nil, errRefusedInternal
	}
	return f, nil
}

// readRegisteredFile reads a registered file through openRegisteredFile's descriptor
// check, so no read route can be tricked into returning internal state.
func (s *Server) readRegisteredFile(absPath string) ([]byte, error) {
	f, err := s.openRegisteredFile(absPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

const maxSaveSize = 50 * 1024 * 1024

type fileMeta struct {
	// Spec and Extensions describe the HOST, not this document, and are the same
	// for every file it serves. Added to the existing shape rather than replacing
	// it: the body carried no `spec` before, so a spec client already read
	// htmlclay as a bare core host, and every existing field still answers.
	Spec       int      `json:"spec"`
	Extensions []string `json:"extensions"`
	// Path is the document's location relative to home, in URL form: always
	// forward-slashed, so a client can build a URL from it. AbsolutePath is the
	// OS-native counterpart and keeps the platform's own separators, which is why
	// the two differ on Windows. Set through filepath.ToSlash, a no-op wherever
	// the separator is already "/".
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
	rawPath := r.PathValue("path")

	// Classified from the query string alone, before any path work, because a malformed data
	// parameter is a property of what the caller sent and reveals nothing about what is on disk.
	// Anything that is not a data request falls through and is served exactly as it is today.
	mode, answered := s.dataModeForQuery(w, r, extractFilePath(rawPath))
	if answered {
		return
	}
	s.serveFile(w, r, rawPath, mode)
}

// serveFile is the shared entry point for a plain GET and for both data faces. rawPath is the
// request path before extractFilePath truncation, because serveAsset needs it intact.
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, rawPath string, mode dataMode) {
	relPath := extractFilePath(rawPath)

	absPath, err := ValidatePath(relPath, s.sessions.HomeDir())
	if err != nil {
		s.logger.Printf("Invalid path %q: %v", relPath, err)
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}

	// Backups are internal state and are never served on the app's own origin, and
	// the config directory sits under the user's home on every platform. This is a
	// cheap pre-filter rather than the load-bearing check: the path it judges is an
	// ancestor of the one serveAsset judges and isInternal is downward-closed, so
	// everything caught here is caught again below, by serveAsset's own lexical
	// check for an unregistered path or by readRegisteredFile's descriptor guard
	// for a registered one. Keep it lexical: isInternal is pure string work, so the
	// refusal reads the same whether or not anything is at that path.
	if s.isInternal(absPath) {
		s.logger.Printf("Denying request for internal path %s", absPath)
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}

	resolved, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		// Unresolvable, which normally means it is not there. Answering 404 here
		// would answer BEFORE the scope check and make this an existence oracle: a
		// missing out-of-scope path would 404 instantly while a present one parked
		// and 403'd, which enumerates the home tree. Hand it to serveAsset, which
		// classifies scope on the lexical path and parks either way.
		s.serveAsset(w, r, rawPath, mode)
		return
	}
	absPath = filepath.Clean(resolved)

	// Internal-ness is deliberately NOT answered here. Deciding it after resolving
	// but before serveAsset's scope gate made a path that resolves into the config
	// or versions tree answer 404 instantly while a nonexistent path parked and
	// 403'd, which is the existence oracle again. A registered file is protected by
	// readRegisteredFile's descriptor guard below; anything else falls through to
	// serveAsset, which re-checks internal-ness AFTER the scope gate.

	f, ok := s.sessions.LookupByPath(absPath)
	if !ok {
		// An unregistered HTML Clay document inside a trusted folder registers
		// itself through the app seam before serving, so a link inside a project
		// lands on an editable page with no prompt. Everything about the
		// decision up to the seam call is lock- and string-work only (trusted
		// scope first, then hidden/internal), preserving the oracle-avoidance
		// ordering serveAsset relies on.
		//
		// A data request never registers. Registration is per-file state created on
		// a caller's say-so, and it can redirect to another site's origin, neither of
		// which belongs in a JSON read. The request falls through to serveAsset, which
		// is the stricter path, so a data face can only ever read less than a
		// navigation would.
		if !mode.active() && s.shouldAutoRegister(r, absPath) {
			if regFile, handled := s.autoRegister(w, r, absPath); handled {
				return
			} else if regFile != nil {
				s.serveRegistered(w, r, regFile, mode)
				return
			}
		}
		s.serveAsset(w, r, rawPath, mode)
		return
	}

	s.serveRegistered(w, r, f, mode)
}

// ensureHistoryKeyLocked resolves this file's durable backup identity exactly
// once and returns it, along with whether that identity was freshly minted and so
// is still provisional until something makes it durable. data is the file's
// current disk bytes: they are read for an existing htmlclayid, and hashed into a
// path key on every other route through here. Nothing is written to disk.
//
// This is the one seam where a key is allowed to come into existence, which is
// what keeps the watcher structurally unable to mint one. Serving, saving, and a
// wire handler's lease all resolve through here. Deriving a key anywhere else is
// how a first-save backup once ended up under a path hash while everything later
// keyed by id, orphaning it. Caller holds f.Lock.
func (s *Server) ensureHistoryKeyLocked(f *session.File, data []byte) (key string, provisional bool) {
	if k := f.HistoryKey(); k != "" {
		return k, false
	}
	if !strings.EqualFold(filepath.Ext(f.AbsPath), ".htmlclay") {
		f.SetHistoryKey(versions.Key(f.AbsPath, data))
		return f.HistoryKey(), false
	}
	id, prov, err := s.versions.ResolveIdentity(f.AbsPath, htmlutil.ReadHTMLClayID(data))
	if err != nil {
		s.logger.Printf("Could not resolve identity for %s: %v", f.RelPath, err)
		f.SetHistoryKey(versions.Key(f.AbsPath, data))
		return f.HistoryKey(), false
	}
	f.SetHistoryKey("id:" + id)
	return f.HistoryKey(), prov
}

// serveRegistered serves a registered file with its identity and, on a real
// document load, its save token.
func (s *Server) serveRegistered(w http.ResponseWriter, r *http.Request, f *session.File, mode dataMode) {
	f.Lock()
	data, err := s.readRegisteredFile(f.AbsPath)
	if err != nil {
		f.Unlock()
		if errors.Is(err, errRefusedInternal) {
			s.logger.Printf("Refusing to serve %s: resolves to a protected location", f.AbsPath)
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		s.logger.Printf("Error reading %s: %v", f.AbsPath, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// A data request touches no per-file state: no identity resolution, no history key, no
	// first-open snapshot, no NoteFirstObservation, no cookie and no token. A JSON read is not an
	// editing session. The lock is released before parsing, so a 17 ms extraction never sits on the
	// save path.
	//
	// What actually guarantees the bytes are a WHOLE pre-save or post-save document is that both
	// writers (save and restore) publish through atomicWriteFile's rename, not this mutex —
	// MEASURED, by replacing this branch with an unlocked re-read and finding the concurrency test
	// still green over three runs. The lock is kept anyway: it costs nothing on a loopback server,
	// it matches how every other reader here behaves, and it is what would still hold if a future
	// write path stopped being a rename.
	//
	// Observable consequence, tested: ?data={id:"html@htmlclayid"} is null before the first save,
	// while the page's own clay.extractData() sees the injected id.
	if mode.active() {
		f.Unlock()
		s.writeExtracted(w, data, mode)
		return
	}

	// firstServe is captured before any record is touched, so the first-open
	// snapshot still sees a genuinely fresh file.
	firstServe := !f.Observed()

	// Resolve the file's durable identity exactly once, without ever writing to
	// disk. The id rides only in the bytes served (below); it reaches the file
	// only when the client's own save carries it back. Every later list, read,
	// restore and save reads this stored key.
	key, provisional := s.ensureHistoryKeyLocked(f, data)

	// served carries the durable id; the bytes on disk never do. Injecting the
	// tracked id over whatever disk holds is also model B′'s self-heal: a file
	// whose id was stripped or replaced externally is re-anchored on the next serve.
	served := data
	if id, ok := versions.IDFromKey(key); ok {
		served = htmlutil.SetHTMLClayID(data, id)
	}

	// B1a: capture a version when a file is first served, so a freshly opened file
	// that is never saved still has something to restore.
	//
	// Published inside f.Lock(), per B1. Publishing after the unlock let two
	// concurrent GETs interleave: GET1 captured H0 and was descheduled, GET2 saw
	// the file as observed and returned a token with no snapshot work, a save
	// published H0 then H1, and GET1 then published its stale H0, leaving history
	// ending at H0 after a successful H1 save. Two tabs opening one file at once
	// is ordinary.
	pruneKey := ""
	if firstServe {
		// Seed both per-file records from the DISK bytes, so the first real save
		// compares like-for-like and does not false-positive as a stale write.
		f.NoteFirstObservation(versions.Hash(data))

		// A trusted-folder auto-registration skips the first-serve snapshot: a
		// page can pull a whole tree into registration, and snapshotting each
		// file on serve would copy the tree into the versions store on a page's
		// say-so. Nothing durable is lost — the first real save's pre-write
		// backup still captures the pre-existing state, because no history
		// exists yet.
		if s.sessions.Via(f.AbsPath) != session.ViaTrusted {
			if s.beforeFirstOpenSnapshot != nil {
				s.beforeFirstOpenSnapshot()
			}
			// The snapshot stores the raw disk bytes, not the injected ones, so it
			// dedups against the first save's pre-write backup instead of doubling
			// every file.
			if _, bErr := s.versions.Backup(key, f.AbsPath, data); bErr != nil {
				s.logger.Printf("First-open snapshot failed for %s: %v", f.RelPath, bErr)
			} else if provisional {
				// This snapshot's identity was freshly minted; mark it so it is
				// reclaimed if no save ever makes it durable.
				if pErr := s.versions.SetProvisional(key, f.AbsPath, true); pErr != nil {
					s.logger.Printf("Could not mark provisional history for %s: %v", f.RelPath, pErr)
				}
			}
			pruneKey = key
		}
	}
	f.Unlock()

	// Bulk pruning runs on the store lock only, never inside f.Lock().
	if pruneKey != "" {
		s.versions.MaybePrune(pruneKey, f.AbsPath)
	}

	// The token is a save capability, and it rides only into real documents. A
	// silent fetch() (Sec-Fetch-Dest: empty), an iframe, or a subresource request
	// gets the same bytes without the token, so harvesting a sibling page's
	// capability needs a visible top-level tab per victim instead of an invisible
	// loop. Absent means a legacy browser without Sec-Fetch support, which keeps
	// the document behavior; every current browser sends the header.
	if dest := r.Header.Get("Sec-Fetch-Dest"); dest == "" || dest == "document" {
		served = htmlutil.InjectToken(served, f.Token)
	}

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
	w.Write(served)
}

// serveAsset serves a file that was never opened directly: an asset (css, js,
// image) or a linked page referenced by an opened file. Allowed only under the
// folder tree of an opened file, and served without token injection, so linked
// pages cannot save. rawPath is the request path before extractFilePath
// truncation, so asset paths containing ".html" in a directory name stay intact.
func (s *Server) serveAsset(w http.ResponseWriter, r *http.Request, rawPath string, mode dataMode) {
	absPath, err := ValidatePath(rawPath, s.sessions.HomeDir())
	if err != nil {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}

	// Everything from here down to the scope check is decided on the LEXICAL path,
	// with no filesystem call, so none of it can reveal whether the requested file
	// exists. Internal and hidden paths are refused outright; both tests are pure
	// string work and name locations a page can already infer.
	if s.isInternal(absPath) {
		s.logger.Printf("Denying request for internal path %s", absPath)
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	// Hidden files and directories are never served as assets and never prompt, so
	// a granted folder cannot expose its .git, .env, or .ssh. A file the user
	// explicitly opened is served by handleServeFile above, never here.
	if session.HasHiddenComponent(s.sessions.HomeDir(), absPath) {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}

	// Scope is classified before any existence check, so an out-of-scope read ends
	// in the same fixed 403 whether or not anything is at that path. Resolving
	// first made a missing file a fast 404 and a present one a 403, which let a
	// page enumerate the whole non-hidden home tree without being granted anything.
	//
	// This decides only whether to ASK. It never authorizes a read: that stays with
	// OpenAsset's held os.Root capability and the RealPath guard below, both of
	// which judge the path the descriptor actually landed on, so a lexical path
	// that looks in scope but resolves elsewhere is still refused.
	//
	// Do NOT add an existence check before the prompt, here or in the broker.
	// Prompting only for paths that exist rebuilds this oracle at directory
	// granularity: a missing folder would 403 instantly while a present one held
	// until the user answered.
	if _, _, inScope := s.sessions.AssetRoot(absPath); !inScope {
		// Hold the request open and ask the user to widen reads; on Allow the same
		// request resumes with no reload, on Deny a fixed 403.
		rc := http.NewResponseController(w)
		_ = rc.SetWriteDeadline(time.Time{})
		if !s.broker.await(r.Context(), absPath) {
			write403(w)
			return
		}
	}

	resolved, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	absPath = filepath.Clean(resolved)

	// Re-checked on the resolved path: the lexical tests above cannot see a symlink
	// pointing into the versions/config tree or into a hidden directory.
	if s.isInternal(absPath) {
		s.logger.Printf("Denying request for internal path %s", absPath)
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	if session.HasHiddenComponent(s.sessions.HomeDir(), absPath) {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}

	// The exact instant a swapped directory component would take effect: the path
	// has been resolved and cleared, and the capability open has not happened yet.
	// Nil in production; a test installs it to hold a request here while it stages
	// the swap, which is the only way to reach this window on purpose.
	if s.beforeAssetCapabilityOpen != nil {
		s.beforeAssetCapabilityOpen()
	}

	file, authorized, err := s.sessions.OpenAsset(absPath)
	if !authorized {
		// The lexical path was in scope, or was just granted, but the resolved one
		// is covered by no root: a symlink leaving its root, or a root revoked
		// mid-request. Refuse rather than prompt to grant whatever the link pointed
		// at.
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
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

	// TOCTOU guard, bound to the open descriptor. os.Root follows relative symlinks
	// that resolve within the root, and the config/versions tree can sit inside an
	// opened root, so a directory component swapped for a symlink between the
	// EvalSymlinks check above and this capability open could have redirected the read
	// into that tree. Ask the OS for the real path of the descriptor we actually hold
	// and refuse if it is internal or hidden. Unlike a second EvalSymlinks+Stat, this
	// cannot be defeated by re-swapping the pathname: RealPath reports where the held
	// inode lives, not what a name currently points at. The checks above stay as the
	// fast path for the common, unraced case.
	real, err := platform.RealPath(file)
	if err != nil {
		s.logger.Printf("Could not resolve real path for asset %s: %v", absPath, err)
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	real = filepath.Clean(real)
	if s.isInternal(real) || session.HasHiddenComponent(s.sessions.HomeDir(), real) {
		s.logger.Printf("Denying asset resolving to internal/hidden path: %s", absPath)
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}

	// The data projection attaches HERE, after the whole ladder: lexical validation, the internal
	// and hidden filters, the scope gate and its prompt, the capability open, the regular-file test
	// and the descriptor-bound RealPath re-check. Nothing above this line knows a data face exists,
	// which is what makes "can never read a file the plain GET could not" structural rather than
	// something to keep verifying.
	if mode.active() {
		raw, rErr := io.ReadAll(file)
		if rErr != nil {
			s.logger.Printf("Error reading asset %s for extraction: %v", absPath, rErr)
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		s.writeExtracted(w, raw, mode)
		return
	}

	// A read-only HTML Clay document reached by a real user navigation forks to
	// the bannered path: buffered, nonce-bearing, uncacheable. Everything else
	// (assets, fetches, iframes, non-.htmlclay pages) streams below unchanged.
	if s.shouldOfferOpen(r, real) {
		s.serveReadOnlyWithBanner(w, file, real)
		return
	}

	name := filepath.Base(absPath)

	// An SVG is a document: it can carry <script>, and served inline from this
	// origin it runs with the same authority as the page beside it. Uploads accept
	// SVG precisely BECAUSE serving it inert is possible, so this header is what
	// makes that decision safe. Unconditional, because provenance is not knowable
	// here: a file that arrived through /_/upload and one the person saved into
	// the folder themselves look identical at serve time.
	if ext := strings.ToLower(filepath.Ext(name)); ext == ".svg" || ext == ".svgz" {
		w.Header().Set("Content-Disposition", "attachment")
		w.Header().Set("X-Content-Type-Options", "nosniff")
	}

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

// write403 is the fixed response for a denied out-of-scope read. It carries no
// path and no proposed root: those appear only in the native dialog and the
// tray, never in a body a page can read. Classified before any existence check,
// so it is not an oracle for which out-of-scope files exist. The classification
// above it is lexical, so a denial cannot be used to test whether a file exists.
func write403(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-HTMLClay-Error", "read-access-required")
	w.WriteHeader(http.StatusForbidden)
	w.Write([]byte(`{"type":"about:blank","title":"Read access required","status":403,` +
		`"detail":"This page requested a file outside its permitted folder; access was not granted."}`))
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

	data, err := s.readRegisteredFile(f.AbsPath)
	if err != nil {
		if errors.Is(err, errRefusedInternal) {
			s.logger.Printf("Refusing to read %s: resolves to a protected location", f.AbsPath)
			s.writeError(w, http.StatusNotFound, "not found")
			return
		}
		s.logger.Printf("Error reading %s: %v", f.AbsPath, err)
		s.writeError(w, http.StatusInternalServerError, "read error")
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(data)
}

// trustedWriteRevoked reports whether f's only claim to a save capability was
// trusted-folder coverage that has since been revoked. Untrusting a folder
// closes its origin and unregisters its files, so the token normally dies with
// it; this closes the gap for a request that resolved its token before the
// revoke landed, so revocation bites even a session already looked up.
//
// It reads the session's installed roots rather than the declared list on
// purpose: this is the write path, and what authorizes a write is the held
// capability, not what config says.
func (s *Server) trustedWriteRevoked(f *session.File) bool {
	via := s.sessions.Via(f.AbsPath)
	return via.Has(session.ViaTrusted) &&
		!via.Has(session.ViaOsOpen) &&
		!s.sessions.TrustedCovers(f.AbsPath)
}

func (s *Server) handleSave(w http.ResponseWriter, r *http.Request) {
	f, ok := s.lookupSession(w, r)
	if !ok {
		return
	}
	if s.trustedWriteRevoked(f) {
		s.writeError(w, http.StatusUnauthorized, "invalid token")
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

	// Spec §3: /_/save takes the document as text, and this route has exactly one
	// body shape. A JSON body is refused rather than guessed at, because a host
	// that quietly wrote an envelope's `content` would be inventing a second shape
	// for the one route the spec says has only one. Anything a save needs to say
	// beyond the document travels in a header, and an unstripped snapshot belongs
	// to the §10 relay, never to a save.
	if isJSONContentType(r.Header.Get("Content-Type")) {
		uploadError(w, http.StatusUnsupportedMediaType, "unsupported-media-type",
			"/_/save takes the document as text, not JSON.")
		return
	}

	body = htmlutil.StripToken(body)
	// A banner that reached a token-holding tab (live-sync can push one into an
	// edit-mode page) must never autosave itself into the file.
	body = htmlutil.StripBanner(body)

	// A valid save is always a full document (the browser serializes
	// documentElement.outerHTML). Reject anything without an <html> tag so a
	// stray request cannot overwrite the file with a fragment or junk.
	if !htmlutil.HasHTMLTag(body) {
		s.writeError(w, http.StatusBadRequest, "body is not an HTML document")
		return
	}

	f.Lock()
	current, readErr := s.readRegisteredFile(f.AbsPath)
	// A registered path that now resolves into protected state (a swapped-in symlink
	// to the config/versions tree, or out of home) must not be read into a backup or
	// written over; refuse the whole save rather than proceed. A plain not-found is
	// left to the normal first-save path below.
	if errors.Is(readErr, errRefusedInternal) {
		f.Unlock()
		s.logger.Printf("Refusing to save %s: resolves to a protected location", f.AbsPath)
		s.writeError(w, http.StatusNotFound, "not found")
		return
	}

	// The backup identity comes from the key resolved at first serve, never from
	// the bytes on disk or in the body. Deriving it from disk meant that on a first
	// save the id-less on-disk bytes (the host no longer writes the id) keyed by
	// path hash while everything later keyed by id, orphaning the pre-save backup.
	// A save that somehow precedes a serve resolves identity the same way serving
	// does; the body's own id is never adopted, so a pasted-in foreign id cannot
	// move the history (model B′).
	key, _ := s.ensureHistoryKeyLocked(f, body)

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

	// A truncation in progress must not be overwritten. os.WriteFile truncates and
	// then writes, so a writer paused in that gap leaves zero bytes on disk that
	// are not a document at all; watchEmptyQuiet exists to wait exactly that out.
	// Saving over them in the meantime is worse than a stale tab: rename unlinks
	// the inode the external writer still has open, so its later write lands on a
	// file with no name and is lost, with no version of it anywhere. An autosave
	// firing on its own debounce is the ordinary way to reach this, and the tab has
	// no idea the file was emptied underneath it.
	//
	// The refusal lifts on its own, because it is keyed on the watcher's pending
	// candidate: within watchEmptyQuiet either the writer's real bytes arrive or
	// the empty state publishes, and a save that still means to overwrite then goes
	// through against a document the page has actually been shown.
	if readErr == nil && stale && len(current) == 0 && s.watcher.emptyPending(f.AbsPath) {
		f.Unlock()
		s.logger.Printf("Refusing to save over a truncation in progress: %s", f.RelPath)
		s.writeError(w, http.StatusConflict,
			"the file was just emptied on disk; retry once that change reaches the page")
		return
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
		// This save makes the history durable. The backup above already defaulted
		// the meta to non-provisional, but a save that only deduplicated skips that
		// write, so clear the flag explicitly.
		if pErr := s.versions.SetProvisional(key, f.AbsPath, false); pErr != nil {
			s.logger.Printf("Could not clear provisional flag for %s: %v", f.RelPath, pErr)
		}
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

// isJSONContentType reports whether a Content-Type declares JSON, including the
// `+json` structured suffix.
//
// It splits on ';' rather than going through mime.ParseMediaType. A header this
// route REFUSES does not have to be well formed, and ParseMediaType fails on
// something like `application/json;;` — treating that failure as "not JSON" is the
// most permissive possible reading of bytes we could not understand, and it let the
// old envelope through to be written to the file verbatim.
func isJSONContentType(ct string) bool {
	mediaType, _, _ := strings.Cut(ct, ";")
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	return mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
}

// replaceFile renames tmp over target, retrying briefly before giving up.
//
// Windows will not replace a file that another handle currently has open, and Go
// opens files without FILE_SHARE_DELETE, so any concurrent reader blocks the
// rename. htmlclay supplies its own reader: the watcher re-reads the served file
// every 250ms to notice outside edits, so a save that lands during that read
// fails with "Access is denied" and the user is told their save did not work.
// The read itself lasts microseconds, so the first backoff clears it in practice.
//
// On POSIX the first attempt always succeeds and this never sleeps.
func replaceFile(tmpPath, targetPath string) error {
	var err error
	for _, backoff := range []time.Duration{0, 5, 15, 40, 100} {
		if backoff > 0 {
			time.Sleep(backoff * time.Millisecond)
		}
		if err = os.Rename(tmpPath, targetPath); err == nil {
			return nil
		}
	}
	return err
}

// beforeAtomicReplace runs inside atomicWriteFile once the replacement bytes are
// fully on disk in the temp file and before the rename that publishes them. That
// is the one instant at which a reader could see a torn file if the write were
// not staged, so it is the instant a test wants to hold. Tests only; nil
// everywhere else.
var beforeAtomicReplace func()

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

	if beforeAtomicReplace != nil {
		beforeAtomicReplace()
	}

	if err := replaceFile(tmpPath, targetPath); err != nil {
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

	rf, err := s.openRegisteredFile(f.AbsPath)
	if err != nil {
		if errors.Is(err, errRefusedInternal) {
			s.logger.Printf("Refusing meta for %s: resolves to a protected location", f.AbsPath)
			s.writeError(w, http.StatusNotFound, "not found")
			return
		}
		s.logger.Printf("Error stat %s: %v", f.AbsPath, err)
		s.writeError(w, http.StatusInternalServerError, "stat error")
		return
	}
	defer rf.Close()

	info, err := rf.Stat()
	if err != nil {
		s.logger.Printf("Error stat %s: %v", f.AbsPath, err)
		s.writeError(w, http.StatusInternalServerError, "stat error")
		return
	}

	// Report the tracked identity, which the host injects when serving. Between
	// first serve and first save the disk carries no id, so reading it off disk
	// would report none while the served document already has one.
	var htmlclayID string
	f.Lock()
	if id, ok := versions.IDFromKey(f.HistoryKey()); ok {
		htmlclayID = id
	}
	f.Unlock()
	if htmlclayID == "" {
		if data, rErr := io.ReadAll(rf); rErr == nil {
			htmlclayID = htmlutil.ReadHTMLClayID(data)
		}
	}

	meta := fileMeta{
		Spec: specVersion,
		// `sync` is announced because this host serves both halves of the §10 address.
		// It relays snapshots only and refuses a `document` body; §10 is informative in
		// v1, and updating viewers on save rather than by client relay is the flow the
		// section itself describes as usual.
		Extensions: []string{"sync", "upload"},
		// RelPath comes from filepath.Rel, so on Windows it arrives with
		// backslashes; `path` is the field a client builds a URL from, and the URL
		// this same document is served at is forward-slashed. AbsolutePath stays
		// OS-native on purpose -- it names a file on disk, not a route.
		Path:         filepath.ToSlash(f.RelPath),
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
