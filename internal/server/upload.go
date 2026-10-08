package server

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Uploads (spec §9). A document that cannot upload has to put a picture INSIDE
// itself as a data: URL, which costs a third more bytes than the file, on this
// save and on every future save, and in every stored version. Storing it beside
// the document instead is what this lane is for.
//
// The save token is the whole credential, as it is for saving. It authorizes
// writes into the assets folder of the ONE document it names and nowhere else,
// which is strictly less than the save capability the same token already carries,
// so it needs no new consent step. A revoked trusted write refuses uploads by the
// same path it refuses saves.

const maxUploadSize = 25 << 20

// The multipart framing around the one file part: boundaries, part headers and
// a long filename. The body may exceed the advertised cap by this much, so a
// file of exactly maxBytes is accepted; the file's own bytes are capped below.
const maxUploadEnvelope = 64 << 10

// Refused by type. A document or a script stored beside a document and served
// from the same origin is stored XSS: the file the person just uploaded would
// execute with the document's own authority. The enumerated set mirrors
// hyperclay.com's DOCUMENT_UPLOAD_EXTENSIONS; documentMimeType is the belt for
// anything the set misses.
//
// SVG is deliberately absent. It is accepted and served inert instead (see the
// Content-Disposition on the asset lane), because refusing it would break a
// legitimate and very common kind of image for a threat that serving already
// answers.
var refusedUploadExt = map[string]bool{
	".html": true, ".htm": true, ".shtml": true, ".xhtml": true, ".xht": true, ".htmlclay": true,
	".xml": true, ".xsl": true, ".xslt": true, ".mathml": true, ".mml": true,
	".rss": true, ".atom": true, ".rdf": true,
	".js": true, ".mjs": true, ".cjs": true,
	".ecma": true,
}

var documentMimeType = regexp.MustCompile(`^(?i)(text/html|application/xhtml\+xml|text/xml|application/xml|[a-z0-9.-]+/[a-z0-9.+-]*\+xml|text/mathml|text/javascript|application/javascript|application/x-javascript|application/ecmascript|text/ecmascript)$`)

// refusedUpload reports whether an upload with this extension could be served
// as a page or a script. An empty or unknown extension is not refused: the
// asset lane serves it as application/octet-stream, which a browser downloads.
func refusedUpload(ext string) bool {
	ext = strings.ToLower(ext)
	if ext == ".svg" || ext == ".svgz" {
		return false
	}
	if refusedUploadExt[ext] {
		return true
	}
	if ext == "" {
		return false
	}
	ctype, _, _ := strings.Cut(mime.TypeByExtension(ext), ";")
	return documentMimeType.MatchString(strings.TrimSpace(ctype))
}

// inAssetsFolder reports whether a file sits directly inside a document's
// uploads folder, which is where every upload is stored. Called with the real
// path of the open file, compared without case: on a case-insensitive disk
// "Assets-doc" names the same folder.
func inAssetsFolder(realPath string) bool {
	return strings.HasPrefix(strings.ToLower(filepath.Base(filepath.Dir(realPath))), "assets-")
}

var documentExt = map[string]bool{
	".htmlclay": true, ".html": true, ".htm": true, ".xhtml": true,
}

// uploadError answers in the shape the spec's clients branch on: a `code` from
// the §3 registry plus a human message. writeError's {ok,error} body is left
// alone because every other route already answers in it.
func uploadError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"ok": false, "msg": msg, "msgType": "error", "code": code,
	})
}

// assetsDirFor returns the folder that holds one document's uploads: beside the
// document and named after it, so a folder of documents does not turn into one
// shared pile, and so the URL written into the page resolves relative to the
// document itself.
func assetsDirFor(absPath string) (dir, folder string) {
	base := filepath.Base(absPath)
	if ext := filepath.Ext(base); documentExt[strings.ToLower(ext)] {
		base = strings.TrimSuffix(base, ext)
	}
	folder = "assets-" + base
	return filepath.Join(filepath.Dir(absPath), folder), folder
}

// assetsFolderName is the library folder for one document's uploads, slugged to
// the alphabet hyperclay.com accepts for folders so the layout is the same on
// every host: "My Board.v2.htmlclay" -> "assets-my-board-v2".
func assetsFolderName(absPath string) string {
	base := filepath.Base(absPath)
	if ext := filepath.Ext(base); documentExt[strings.ToLower(ext)] {
		base = strings.TrimSuffix(base, ext)
	}
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(base) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			if r == '-' {
				if dash {
					continue
				}
				dash = true
			} else {
				dash = false
			}
			b.WriteRune(r)
			continue
		}
		if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		slug = "document"
	}
	return "assets-" + slug
}

// splitUploadName reduces a client-supplied filename to a bare stem and
// extension. Both separators are stripped, not just the platform's: a Windows
// browser sends a backslash path and filepath.Base leaves it whole on unix. A
// leading dot goes too, so an upload cannot create a hidden file that htmlclay
// then refuses to serve.
func splitUploadName(filename string) (stem, ext string) {
	name := filename[strings.LastIndexAny(filename, `/\`)+1:]
	name = strings.ReplaceAll(name, "\x00", "")
	name = strings.TrimLeft(name, ".")
	if name == "" {
		name = "file"
	}
	ext = filepath.Ext(name)
	stem = strings.TrimSuffix(name, ext)
	if stem == "" {
		stem = "file"
	}
	return stem, ext
}

// uploadHashMin is how many hex characters of the keyed digest the shortest
// candidate name carries. 32 is 128 bits: every HTML Clay site can read the
// library by path, so a name must not be guessable from the document's own name,
// which is the only other thing an attacker knows. Same bytes still converge on
// one file; a real prefix collision lengthens the tail, up to the whole digest.
const uploadHashMin = 32

// libraryKeyFile is the library's own secret, hidden beside the uploads folders.
// The asset lane refuses a hidden component and the exporter skips one, so it is
// never served and never packaged.
const libraryKeyFile = ".library-key"

// libraryKeyLen is the key's size in bytes: 256 bits, which is the block size of
// the HMAC-SHA256 it keys and more than enough that no one can guess it.
const libraryKeyLen = 32

// loadOrCreateLibraryKey returns the library's naming key, creating it on first
// use. Every filesystem call goes through an *os.Root held on the library, so a
// symlinked library or key cannot send the write anywhere else.
//
// The bytes are written to a hidden temp file, fsynced, and only then published
// under the key's name with an exclusive hard link, so the name never exists
// half-written: a crash leaves an orphan temp file rather than a short key, and
// two uploads racing on a fresh library both end up with the winner's key rather
// than two keys and two names for one file. The loser of the race reads back what
// the winner published.
//
// A key of the wrong length is therefore not something this code can produce, and
// it is not repaired. The repair would have to rename the key away, and a caller
// that read a short key would then rename away the good key another caller had
// just published: nothing here can tell an abandoned key from one a person is
// editing, so the length is checked and the fix is named instead.
func loadOrCreateLibraryKey(dir string) ([]byte, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()

	key, err := readLibraryKey(root, dir)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return key, err
	}

	tmp, err := writeLibraryKeyTemp(root)
	if err != nil {
		return nil, err
	}
	defer root.Remove(tmp)

	if err := root.Link(tmp, libraryKeyFile); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	return readLibraryKey(root, dir)
}

// readLibraryKey reads the published key and refuses a wrong length with the fix,
// rather than guessing at one: a short key is a guessable key, and two sites
// reading two different keys would name one file two ways.
func readLibraryKey(root *os.Root, dir string) ([]byte, error) {
	key, err := root.ReadFile(libraryKeyFile)
	if err != nil {
		return nil, err
	}
	if len(key) != libraryKeyLen {
		return nil, fmt.Errorf("%s in %s is %d bytes, want %d: delete it and upload again", libraryKeyFile, dir, len(key), libraryKeyLen)
	}
	return key, nil
}

// writeLibraryKeyTemp writes a fresh key to a uniquely named hidden file in the
// library and fsyncs it, so the bytes are on disk before the name is published.
// The caller publishes or removes it; nothing here is ever the key's own name.
func writeLibraryKeyTemp(root *os.Root) (string, error) {
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	name := libraryKeyFile + ".tmp-" + hex.EncodeToString(nonce[:])
	fh, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}

	key := make([]byte, libraryKeyLen)
	_, randErr := rand.Read(key)
	_, writeErr := fh.Write(key)
	syncErr := fh.Sync()
	closeErr := fh.Close()
	if err := errors.Join(randErr, writeErr, syncErr, closeErr); err != nil {
		root.Remove(name)
		return "", err
	}
	return name, nil
}

// uploadKey is the library's naming key for this server, read once and kept. A
// failure is not cached: the library may be created between two uploads, and the
// next upload must be able to succeed. An empty library has no key at all.
func (s *Server) uploadKeyBytes() ([]byte, error) {
	s.uploadKeyMu.Lock()
	defer s.uploadKeyMu.Unlock()
	if s.uploadKey != nil {
		return s.uploadKey, nil
	}
	if s.uploadsDir == "" {
		return nil, errors.New("no uploads library")
	}
	key, err := loadOrCreateLibraryKey(s.uploadsDir)
	if err != nil {
		return nil, err
	}
	s.uploadKey = key
	return key, nil
}

// uploadDigest is the hex HMAC-SHA256 of a file's bytes under the library key,
// scoped to the folder the file is stored in. Keyed, not a bare content hash: the
// hash of a short text or a known image is something any page on any HTML Clay
// port can compute, and a name it can compute is a name it can probe for.
//
// The folder is inside the digest so that a name learned in one document's folder
// says nothing about another's. Every page can upload, read the name it is handed
// back, and then request that name in someone else's folder: an unkeyed-by-folder
// digest answers 200 and confirms those exact bytes are there. Two documents whose
// stems slug to the same folder already share that folder and its files, so this
// hides nothing they can already see.
func uploadDigest(key []byte, folder string, data []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(folder))
	mac.Write([]byte{0})
	mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil))
}

// storeUpload writes the bytes under a name derived from them and the library key
// inside folder, beneath the library directory it is given. Every filesystem call
// goes through an *os.Root held on that directory, so a symlinked folder (or a
// ".." smuggled into a name) can never send the write anywhere else; the folder
// itself must be a real directory, not a link.
//
// The bytes go to a hidden temp file first and are published with an exclusive
// hard link, so the final name never exists half-written and never replaces a
// file already there. The digest is what removes the race, not a lock: two
// uploads of DIFFERENT bytes get different names and never contend, and two
// uploads of the SAME bytes converge on one file, with whichever loses the link
// reading back what the winner published and agreeing with it. The tail
// lengthens only on a real digest-prefix collision between different content.
func storeUpload(parentDir, folder, stem, ext string, key, data []byte) (string, error) {
	parent, err := os.OpenRoot(parentDir)
	if err != nil {
		return "", err
	}
	defer parent.Close()
	if err := parent.Mkdir(folder, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	info, err := parent.Lstat(folder)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a folder", folder)
	}
	dir, err := parent.OpenRoot(folder)
	if err != nil {
		return "", err
	}
	defer dir.Close()

	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	tmp := ".upload-" + hex.EncodeToString(nonce[:]) + ".tmp"
	fh, err := dir.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", err
	}
	defer dir.Remove(tmp)
	_, writeErr := fh.Write(data)
	syncErr := fh.Sync()
	closeErr := fh.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return "", err
	}

	digest := uploadDigest(key, folder, data)
	for n := uploadHashMin; n <= len(digest); n += 2 {
		name := stem + "-" + digest[:n] + ext
		err := dir.Link(tmp, name)
		if err == nil {
			return name, nil
		}
		if !errors.Is(err, os.ErrExist) {
			// exFAT drives and many network shares have no hard links. There the
			// final name is created exclusively and written in place, which still
			// never replaces a file but can be seen half-written.
			return storeInPlace(dir, stem, digest, ext, data)
		}
		existing, readErr := dir.ReadFile(name)
		if readErr == nil && bytes.Equal(existing, data) {
			return name, nil
		}
	}
	return "", errors.New("no free name for that file")
}

func storeInPlace(dir *os.Root, stem, digest, ext string, data []byte) (string, error) {
	for n := uploadHashMin; n <= len(digest); n += 2 {
		name := stem + "-" + digest[:n] + ext
		fh, err := dir.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			if !errors.Is(err, os.ErrExist) {
				return "", err
			}
			existing, readErr := dir.ReadFile(name)
			if readErr == nil && bytes.Equal(existing, data) {
				return name, nil
			}
			continue
		}
		_, writeErr := fh.Write(data)
		closeErr := fh.Close()
		if err := errors.Join(writeErr, closeErr); err != nil {
			dir.Remove(name)
			return "", err
		}
		return name, nil
	}
	return "", errors.New("no free name for that file")
}

// hostPathSegment percent-encodes one segment of a returned upload URL: every byte
// outside the unreserved URL set becomes %XX with uppercase hex. This is stricter
// than url.PathEscape, which leaves the sub-delimiters @ + & = $ : , raw. A raw
// one of those ends a link for the exporter's scanner, so the file the document
// links would be cut out of the zip, and one is enough to break a URL a client
// pastes anywhere. The route decodes through PathValue, so a strictly encoded
// link still resolves.
func hostPathSegment(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&0x0f])
	}
	return b.String()
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	f, ok := s.lookupSession(w, r)
	if !ok {
		return
	}
	if s.trustedWriteRevoked(f) {
		s.writeError(w, http.StatusUnauthorized, "invalid token")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize+maxUploadEnvelope)
	file, header, err := r.FormFile("file")
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			uploadError(w, http.StatusRequestEntityTooLarge, "too-large", "File too large (max 25MB)")
			return
		}
		uploadError(w, http.StatusBadRequest, "bad-request", `Expected one file part named "file"`)
		return
	}
	defer file.Close()

	stem, ext := splitUploadName(header.Filename)
	if refusedUpload(ext) {
		uploadError(w, http.StatusUnsupportedMediaType, "unsupported-type", "That kind of file cannot be uploaded")
		return
	}

	data, err := io.ReadAll(io.LimitReader(file, maxUploadSize+1))
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			uploadError(w, http.StatusRequestEntityTooLarge, "too-large", "File too large (max 25MB)")
			return
		}
		uploadError(w, http.StatusBadRequest, "bad-request", "Could not read the upload")
		return
	}
	if len(data) > maxUploadSize {
		uploadError(w, http.StatusRequestEntityTooLarge, "too-large", "File too large (max 25MB)")
		return
	}
	if len(data) == 0 {
		uploadError(w, http.StatusBadRequest, "bad-request", "Empty file")
		return
	}

	folder := assetsFolderName(f.AbsPath)
	if s.uploadsDir == "" {
		uploadError(w, http.StatusInternalServerError, "error", "Could not store the file")
		return
	}
	if err := os.MkdirAll(s.uploadsDir, 0o755); err != nil {
		s.logger.Printf("Error creating the uploads library: %v", err)
		uploadError(w, http.StatusInternalServerError, "error", "Could not store the file")
		return
	}

	key, err := s.uploadKeyBytes()
	if err != nil {
		s.logger.Printf("Error reading the uploads library key: %v", err)
		uploadError(w, http.StatusInternalServerError, "error", "Could not store the file")
		return
	}

	name, err := storeUpload(s.uploadsDir, folder, stem, ext, key, data)
	if err != nil {
		s.logger.Printf("Error storing upload in %s: %v", folder, err)
		uploadError(w, http.StatusInternalServerError, "error", "Could not store the file")
		return
	}

	// A host path into the library, answered by GET /_/uploads/ on every site
	// server. Every byte outside the URL path alphabet is escaped, while the stored
	// name keeps its own characters: a raw space breaks srcset, where a space
	// separates candidates, and the exporter reads a raw @ + & = $ : , as the end of
	// a link and would leave the file out of the zip.
	served := "/_/uploads/" + hostPathSegment(folder) + "/" + hostPathSegment(name)

	noStoreJSON(w)
	json.NewEncoder(w).Encode(map[string]any{
		"ok": true, "msg": "Uploaded", "msgType": "success",
		"uploads": []map[string]any{{"name": name, "url": served, "bytes": len(data)}},
	})
}

// handleLibraryUpload serves one file out of the per-computer uploads library at
// GET /_/uploads/<folder>/<name>, the path every upload is written back as. No
// token: the request comes from an <img> or a fetch, which carry none, and the
// library holds only files the person uploaded themselves. HostValidationMiddleware
// still wraps the whole mux.
//
// os.Root is the containment, not string matching: "..", an absolute path and a
// symlink that leaves the library are all refused by the OS. A hidden segment is
// refused too, so the temp file an interrupted upload leaves is not served. Only
// a regular file is answered; a folder is 404, never a listing.
func (s *Server) handleLibraryUpload(w http.ResponseWriter, r *http.Request) {
	if s.uploadsDir == "" {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	root, err := os.OpenRoot(s.uploadsDir)
	if err != nil {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	defer root.Close()

	rel := r.PathValue("path")
	for _, seg := range strings.Split(rel, "/") {
		if strings.HasPrefix(seg, ".") {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
	}
	file, err := root.Open(rel)
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

	// Everything here arrived as an upload, so a page or a script type is handed
	// over as a download whatever its extension claims.
	name := filepath.Base(rel)
	setAssetHeaders(w, name, refusedUpload(strings.ToLower(filepath.Ext(name))))
	http.ServeContent(w, r, name, info.ModTime(), file)
}

// specVersion is the Malleable HTML File specification this host answers for.
const specVersion = 1
