package server

import (
	"encoding/json"
	"net/http"
	"os"

	"github.com/panphora/htmlclay/htmlutil"
	"github.com/panphora/htmlclay/versions"
)

// maxRestoreSize is the explicit ceiling on a version accepted back into a live
// file. HasHTMLTag is not sufficient on its own: it accepts `<html><body>partial`.
const maxRestoreSize = 50 * 1024 * 1024

// keyFor derives the history key for a file from its current on-disk bytes.
// Caller must hold f.Lock().
func keyFor(absPath string) (string, []byte, error) {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return versions.Key(absPath, nil), nil, err
	}
	return versions.Key(absPath, data), data, nil
}

// noStoreJSON marks every token-bearing response uncacheable.
func noStoreJSON(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
}

func (s *Server) handleListVersions(w http.ResponseWriter, r *http.Request) {
	f, ok := s.lookupSession(w, r)
	if !ok {
		return
	}

	f.Lock()
	key, _, _ := keyFor(f.AbsPath)
	entries, err := s.versions.List(key, f.AbsPath)
	f.Unlock()

	if err != nil {
		s.logger.Printf("Error listing versions for %s: %v", f.RelPath, err)
		s.writeError(w, http.StatusInternalServerError, "list error")
		return
	}
	if entries == nil {
		entries = []versions.Entry{}
	}

	noStoreJSON(w)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":       true,
		"name":     f.Name,
		"versions": entries,
	})
}

func (s *Server) handleReadVersion(w http.ResponseWriter, r *http.Request) {
	f, ok := s.lookupSession(w, r)
	if !ok {
		return
	}

	// PathValue is already decoded exactly once by net/http. The name must be
	// exactly one generated filename; Read revalidates it and opens beneath the
	// resolved identity directory.
	name := r.PathValue("name")
	if _, _, err := versions.ParseEntryName(name); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid version name")
		return
	}

	f.Lock()
	key, _, _ := keyFor(f.AbsPath)
	data, err := s.versions.Read(key, f.AbsPath, name)
	f.Unlock()

	if err != nil {
		s.logger.Printf("Error reading version %s of %s: %v", name, f.RelPath, err)
		s.writeError(w, http.StatusNotFound, "version not found")
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(data)
}

func (s *Server) handleRestoreVersion(w http.ResponseWriter, r *http.Request) {
	f, ok := s.lookupSession(w, r)
	if !ok {
		return
	}

	name := r.PathValue("name")
	if _, _, err := versions.ParseEntryName(name); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid version name")
		return
	}

	f.Lock()

	key, current, readErr := keyFor(f.AbsPath)

	data, err := s.versions.Read(key, f.AbsPath, name)
	if err != nil {
		f.Unlock()
		s.logger.Printf("Error reading version %s of %s: %v", name, f.RelPath, err)
		s.writeError(w, http.StatusNotFound, "version not found")
		return
	}
	if len(data) > maxRestoreSize {
		f.Unlock()
		s.writeError(w, http.StatusRequestEntityTooLarge, "version is too large to restore")
		return
	}
	if !htmlutil.IsCompleteHTMLDocument(data) {
		f.Unlock()
		s.logger.Printf("Refusing to restore %s of %s: not a complete HTML document", name, f.RelPath)
		s.writeError(w, http.StatusBadRequest, "version is not a complete HTML document")
		return
	}

	// The safety backup before a restore is mandatory, unlike a normal save: a
	// read-only versions directory would otherwise allow a destructive restore
	// with no recovery copy. Success means a completely and atomically published
	// usable version; a verified identical existing version satisfies it.
	if readErr == nil {
		if _, bErr := s.versions.Backup(key, f.AbsPath, current); bErr != nil {
			f.Unlock()
			s.logger.Printf("Refusing to restore %s: safety backup failed: %v", f.RelPath, bErr)
			s.writeError(w, http.StatusInternalServerError, "could not create a safety backup")
			return
		}
	}

	// Preserve the file's canonical htmlclayid rather than adopting the one stored
	// inside the version, which may have come from a clone or a rename.
	data = htmlutil.StripToken(data)
	if id := htmlutil.ReadHTMLClayID(current); versions.IsCanonicalUUID(id) {
		data = htmlutil.SetHTMLClayID(data, id)
	} else {
		data = htmlutil.StripHTMLClayID(data)
	}

	if wErr := atomicWriteFile(f.AbsPath, data); wErr != nil {
		f.Unlock()
		s.logger.Printf("Error restoring %s: %v", f.AbsPath, wErr)
		s.writeError(w, http.StatusInternalServerError, "write error")
		return
	}

	// A restore advances both per-file records, so it participates in save
	// suppression exactly like a save does, and emits a live notification.
	f.RecordServerWrite(versions.Hash(data))
	s.broadcastDiskHTML(f, data)
	s.hub.notifyWarning(f.AbsPath, f.Name+" was restored from a backup")
	f.Unlock()

	s.versions.MaybePrune(key, f.AbsPath)

	s.logger.Printf("Restored %s from version %s", f.RelPath, name)
	noStoreJSON(w)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":      true,
		"msg":     "Restored " + name,
		"msgType": "success",
	})
}
