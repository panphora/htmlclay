package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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

// Refused by extension. A document or a script stored beside a document and
// served from the same origin is stored XSS: the file the person just uploaded
// would execute with the document's own authority.
//
// SVG is deliberately absent. It is accepted and served inert instead (see the
// Content-Disposition on the asset lane), because refusing it would break a
// legitimate and very common kind of image for a threat that serving already
// answers.
var refusedUploadExt = map[string]bool{
	".html": true, ".htm": true, ".xhtml": true, ".htmlclay": true,
	".js": true, ".mjs": true, ".cjs": true,
	".xml": true, ".xht": true, ".xsl": true, ".xslt": true,
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

// storeUpload writes the bytes under a content-derived name. The hash is what
// removes the race, not a lock: two uploads of DIFFERENT bytes get different
// names and never contend, and two uploads of the SAME bytes converge on one
// file, with whichever loses the exclusive create reading back what the winner
// wrote and agreeing with it. The tail lengthens only on a real hash-prefix
// collision between different content.
func storeUpload(dir, stem, ext string, data []byte) (string, error) {
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	for n := 6; n <= 32; n += 2 {
		name := stem + "-" + digest[:n] + ext
		path := filepath.Join(dir, name)
		// O_EXCL: create or fail, never truncate. A plain write here would let a
		// second upload silently destroy the first.
		fh, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			if !errors.Is(err, os.ErrExist) {
				return "", err
			}
			existing, readErr := os.ReadFile(path)
			if readErr == nil && string(existing) == string(data) {
				return name, nil
			}
			continue
		}
		_, writeErr := fh.Write(data)
		closeErr := fh.Close()
		if writeErr != nil {
			os.Remove(path)
			return "", writeErr
		}
		if closeErr != nil {
			return "", closeErr
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

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
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
	if refusedUploadExt[strings.ToLower(ext)] {
		uploadError(w, http.StatusUnsupportedMediaType, "unsupported-type", "That kind of file cannot be uploaded")
		return
	}

	data, err := io.ReadAll(file)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			uploadError(w, http.StatusRequestEntityTooLarge, "too-large", "File too large (max 25MB)")
			return
		}
		uploadError(w, http.StatusBadRequest, "bad-request", "Could not read the upload")
		return
	}
	if len(data) == 0 {
		uploadError(w, http.StatusBadRequest, "bad-request", "Empty file")
		return
	}

	dir, folder := assetsDirFor(f.AbsPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.logger.Printf("Error creating assets dir %s: %v", dir, err)
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

	name, err := storeUpload(dir, stem, ext, data)
	if err != nil {
		s.logger.Printf("Error storing upload in %s: %v", dir, err)
		uploadError(w, http.StatusInternalServerError, "error", "Could not store the file")
		return
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
