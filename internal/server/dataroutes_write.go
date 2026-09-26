package server

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/panphora/htmlclay/internal/dataapi"
	"github.com/panphora/htmlclay/internal/htmlutil"
	"github.com/panphora/htmlclay/internal/session"
	"github.com/panphora/htmlclay/internal/specwire"
	"github.com/panphora/htmlclay/internal/versions"
)

const maxDataWriteSize = 1 << 20

// writeErrorBody is the write face's error shape, shared with hyperclay-local's
// applySiteDataLocal: details carries the engine's structured refusals.
type writeErrorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

func writeWriteError(w http.ResponseWriter, status int, body writeErrorBody) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(body); err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	dataHeaders(w)
	w.WriteHeader(status)
	w.Write(bytes.TrimRight(buf.Bytes(), "\n"))
}

// handleDataAPIWrite serves POST /_/api/<path>: apply a JSON body to the document through its own
// api rules tag, content only, and commit the result exactly as a save does.
func (s *Server) handleDataAPIWrite(w http.ResponseWriter, r *http.Request) {
	isBrowser, ok := wireCaller(r)
	if !ok {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	if !isJSONContentType(r.Header.Get("Content-Type")) {
		writeWriteError(w, http.StatusUnsupportedMediaType, writeErrorBody{
			Error:   "Unsupported Media Type",
			Message: "POST /_/api takes Content-Type: application/json.",
		})
		return
	}

	rawPath := r.PathValue("path")
	if strings.Trim(rawPath, "/") == "" {
		writeWriteError(w, http.StatusBadRequest, writeErrorBody{
			Error:   "Missing path",
			Message: "Write to a file, for example /_/api/notes.htmlclay.",
		})
		return
	}
	relPath := extractFilePath(rawPath)
	if !isExtractable(relPath) {
		writeWriteError(w, http.StatusNotFound, writeErrorBody{
			Error:   "Unsupported file type",
			Message: "Only .html and .htmlclay files publish a data API.",
		})
		return
	}

	f, status, body := s.writableFile(r, relPath)
	if f == nil {
		writeWriteError(w, status, body)
		return
	}
	if isBrowser && subtle.ConstantTimeCompare([]byte(r.Header.Get(helperTokenHeader)), []byte(f.Token)) != 1 {
		writeWriteError(w, http.StatusForbidden, writeErrorBody{
			Error:   "Save-Token required",
			Message: "A page writes through /_/api only with the target file's Save-Token header.",
		})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxDataWriteSize)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeWriteError(w, http.StatusRequestEntityTooLarge, writeErrorBody{
				Error:   "Payload Too Large",
				Message: "The JSON body is limited to 1 MB.",
			})
			return
		}
		writeWriteError(w, http.StatusBadRequest, writeErrorBody{Error: "Invalid JSON body", Message: "The request body could not be read."})
		return
	}
	data, err := dataapi.ParseStrict(string(raw))
	if err != nil {
		writeWriteError(w, http.StatusBadRequest, writeErrorBody{Error: "Invalid JSON body", Message: "The request body is not valid JSON."})
		return
	}

	s.writeApplied(w, r, f, data)
}

// writableFile resolves the request path to a registered file this route may write, or answers why
// not. Every refusal for an unregistered path is decided on the path's text and the in-memory
// trusted-folder list, so it cannot reveal whether a file exists.
func (s *Server) writableFile(r *http.Request, relPath string) (*session.File, int, writeErrorBody) {
	notFound := writeErrorBody{Error: "Not Found", Message: "No such document."}
	absPath, err := ValidatePath(relPath, s.sessions.HomeDir())
	if err != nil || s.isInternal(absPath) {
		return nil, http.StatusNotFound, notFound
	}
	if resolved, err := filepath.EvalSymlinks(absPath); err == nil {
		absPath = filepath.Clean(resolved)
	}

	f, ok := s.sessions.LookupByPath(absPath)
	if !ok && s.mayRegisterForWrite(absPath) {
		if url, routed := s.hooks.Route(absPath); routed {
			if f, ok = s.sessions.LookupByPath(absPath); !ok {
				return nil, http.StatusConflict, writeErrorBody{
					Error:   "Wrong origin",
					Message: "This document is served from " + url + ". Send the write there.",
				}
			}
		}
	}
	if !ok {
		return nil, http.StatusForbidden, writeErrorBody{
			Error:   "Not writable",
			Message: "Open this document in HTML Clay, or keep it in a trusted folder, before writing to it.",
		}
	}
	if s.trustedWriteRevoked(f) {
		return nil, http.StatusForbidden, writeErrorBody{
			Error:   "Not writable",
			Message: "This document's folder is no longer trusted.",
		}
	}
	return f, 0, writeErrorBody{}
}

// mayRegisterForWrite is shouldAutoRegister's path test without its navigation gate and its
// per-server cap: a write names one file explicitly, so it is not the fan-out the cap limits.
func (s *Server) mayRegisterForWrite(absPath string) bool {
	return s.hooks.Route != nil &&
		strings.EqualFold(filepath.Ext(absPath), ".htmlclay") &&
		s.trustedCovers(absPath) &&
		!session.HasHiddenComponent(s.sessions.HomeDir(), absPath) &&
		!s.isInternal(absPath)
}

// writeApplied is handleSave's commit sequence with the body computed from the stored bytes.
func (s *Server) writeApplied(w http.ResponseWriter, r *http.Request, f *session.File, data dataapi.Value) {
	f.Lock()
	current, readErr := s.readRegisteredFile(f.AbsPath)
	if readErr != nil {
		f.Unlock()
		if errors.Is(readErr, errRefusedInternal) || errors.Is(readErr, os.ErrNotExist) {
			writeWriteError(w, http.StatusNotFound, writeErrorBody{Error: "Site content not found", Message: "The document has no content on disk."})
			return
		}
		s.logger.Printf("Data write could not read %s: %v", f.RelPath, readErr)
		writeWriteError(w, http.StatusInternalServerError, writeErrorBody{Error: "Write failed", Message: "Could not read the stored document."})
		return
	}

	if ifMatch, sent := listHeader(r, "If-Match"); sent && !specwire.IfMatchSatisfied(ifMatch, current) {
		etag := specwire.Etag(current)
		f.Unlock()
		w.Header().Set("ETag", etag)
		writeWriteError(w, http.StatusPreconditionFailed, writeErrorBody{
			Error:   "Precondition Failed",
			Message: f.Name + " changed since you read it. Nothing was written.",
		})
		return
	}

	res, err := dataapi.WriteDocument(htmlutil.StripToken(current), data, "api")
	if err != nil {
		f.Unlock()
		status, body := mapWriteError(err)
		if status == http.StatusInternalServerError {
			s.logger.Printf("Data write of %s failed: %v", f.RelPath, err)
		}
		writeWriteError(w, status, body)
		return
	}

	written := current
	key := ""
	if res.Changed {
		written = res.HTML
		key, _ = s.ensureHistoryKeyLocked(f, current)
		if !s.versions.HasHistory(key, f.AbsPath) {
			if _, bErr := s.versions.Backup(key, f.AbsPath, current); bErr != nil {
				s.logger.Printf("Pre-write backup failed for %s: %v", f.RelPath, bErr)
			}
		}
		if _, bErr := s.versions.Backup(key, f.AbsPath, written); bErr != nil {
			s.logger.Printf("Backup failed for %s: %v", f.RelPath, bErr)
		}
		if err := atomicWriteFile(f.AbsPath, written); err != nil {
			f.Unlock()
			s.logger.Printf("Error writing %s: %v", f.AbsPath, err)
			writeWriteError(w, http.StatusInternalServerError, writeErrorBody{Error: "Write failed", Message: "write error"})
			return
		}
		f.RecordServerWrite(versions.Hash(written))
		f.NoteWriteByThisHost()
		if pErr := s.versions.SetProvisional(key, f.AbsPath, false); pErr != nil {
			s.logger.Printf("Could not clear provisional flag for %s: %v", f.RelPath, pErr)
		}
		s.coord.acceptServerReplacement(f)
		s.broadcastDiskHTML(f, written, key)
	}
	f.Unlock()

	if res.Changed {
		s.versions.MaybePrune(key, f.AbsPath)
		s.logger.Printf("Wrote data into %s (%d bytes, spliced=%v)", f.RelPath, len(written), res.Spliced)
	}
	s.writeExtracted(w, written, dataMode{face: faceAPI})
}

// mapWriteError gives the write engine's refusals the bodies hyperclay-local sends, and defers to
// mapDataError for the rules-tag failures both faces share.
func mapWriteError(err error) (int, writeErrorBody) {
	var noTag *dataapi.NoRulesTag
	if errors.As(err, &noTag) {
		return http.StatusBadRequest, writeErrorBody{Error: "No api rules tag", Message: `This page has no rules tag with data-rules-name~="api".`}
	}
	var rejected *dataapi.WriteRejected
	if errors.As(err, &rejected) {
		return http.StatusBadRequest, writeErrorBody{Error: "Write rejected", Message: err.Error(), Details: map[string]any{
			"unknownKeys": rejected.UnknownKeys, "unmatched": rejected.Unmatched,
		}}
	}
	var refused *dataapi.WriteRefused
	if errors.As(err, &refused) {
		return http.StatusBadRequest, writeErrorBody{Error: "Write refused", Message: err.Error(), Details: refused.Refusals}
	}
	var shape *dataapi.ShapeMismatch
	if errors.As(err, &shape) {
		return http.StatusBadRequest, writeErrorBody{Error: "Shape mismatch", Message: err.Error(), Details: shape.Mismatches}
	}
	var empty *dataapi.EmptyListInsert
	if errors.As(err, &empty) {
		return http.StatusBadRequest, writeErrorBody{Error: "Cannot grow list", Message: err.Error(), Details: empty.Path}
	}
	status, body := mapDataError(err, faceAPI)
	return status, writeErrorBody{Error: body.Error, Message: body.Message}
}
