package server

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
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
}

var documentMimeType = regexp.MustCompile(`^(?i)(text/html|application/xhtml\+xml|text/xml|application/xml|application/[a-z0-9.+-]*\+xml|text/mathml|text/javascript|application/javascript)$`)

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

// inAssetsFolder reports whether a path sits inside a document's uploads folder.
func inAssetsFolder(absPath string) bool {
	for _, part := range strings.Split(filepath.ToSlash(filepath.Dir(absPath)), "/") {
		if strings.HasPrefix(part, "assets-") {
			return true
		}
	}
	return false
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

// storeUpload writes the bytes under a content-derived name inside folder,
// beneath the document's own directory. Every filesystem call goes through an
// *os.Root held on that directory, so a symlinked assets folder (or a ..
// smuggled into a name) can never send the write anywhere else; the folder
// itself must be a real directory, not a link.
//
// The bytes go to a hidden temp file first and are published with an exclusive
// hard link, so the final name never exists half-written and never replaces a
// file already there. The hash is what removes the race, not a lock: two
// uploads of DIFFERENT bytes get different names and never contend, and two
// uploads of the SAME bytes converge on one file, with whichever loses the link
// reading back what the winner published and agreeing with it. The tail
// lengthens only on a real hash-prefix collision between different content.
func storeUpload(parentDir, folder, stem, ext string, data []byte) (string, error) {
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

	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	for n := 6; n <= 32; n += 2 {
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
	for n := 6; n <= 32; n += 2 {
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

	dir, folder := assetsDirFor(f.AbsPath)

	name, err := storeUpload(filepath.Dir(f.AbsPath), folder, stem, ext, data)
	if err != nil {
		s.logger.Printf("Error storing upload in %s: %v", dir, err)
		uploadError(w, http.StatusInternalServerError, "error", "Could not store the file")
		return
	}

	// Read roots contain their whole subtree, so a document opened normally can
	// already read this folder through the root installed when it opened. The
	// exception is the one that makes this call necessary: installReadRoot refuses
	// the home directory itself, so a document sitting loose in ~ has NO root, and
	// without this grant its uploads would store fine and 404 on the way back.
	if err := s.sessions.GrantReadRoot(dir); err != nil {
		s.logger.Printf("Could not grant read access to %s: %v", dir, err)
	}

	// Percent-encoded per segment, while the stored name keeps its own
	// characters. A raw space renders through img src, because the browser
	// repairs it, and breaks in srcset, where a space separates candidates.
	served := url.PathEscape(folder) + "/" + url.PathEscape(name)

	noStoreJSON(w)
	json.NewEncoder(w).Encode(map[string]any{
		"ok": true, "msg": "Uploaded", "msgType": "success",
		"uploads": []map[string]any{{"name": name, "url": served, "bytes": len(data)}},
	})
}

// specVersion is the Malleable HTML File specification this host answers for.
const specVersion = 1
