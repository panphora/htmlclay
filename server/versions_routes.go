package server

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"

	"github.com/panphora/htmlclay/htmlutil"
	"github.com/panphora/htmlclay/session"
	"github.com/panphora/htmlclay/versions"
)

// maxRestoreSize is the explicit ceiling on a version accepted back into a live
// file. HasHTMLTag is not sufficient on its own: it accepts `<html><body>partial`.
const maxRestoreSize = 50 * 1024 * 1024

// historyKey returns the file's resolved backup identity, resolving it once from
// current disk bytes if nothing has resolved it yet (a token used before the file
// was ever served). It is never re-derived once set: deriving the key per request
// meant an external delete or a stripped htmlclayid moved it to a path hash, so
// GET /_/versions returned empty and POST /_/restore 404'd while the id-keyed
// backups sat on disk.
//
// Caller must hold f.Lock().
func historyKey(f *session.File) string {
	if key := f.HistoryKey(); key != "" {
		return key
	}
	data, err := os.ReadFile(f.AbsPath)
	if err != nil {
		// An unreadable file cannot resolve an identity. Leave the key unset so a
		// later readable request can resolve it properly, and answer this one from
		// a key derived on the spot.
		return versions.Key(f.AbsPath, nil)
	}
	f.SetHistoryKey(versions.Key(f.AbsPath, data))
	return f.HistoryKey()
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
	entries, err := s.versions.List(historyKey(f), f.AbsPath)
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
	data, err := s.versions.Read(historyKey(f), f.AbsPath, name)
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

	key := historyKey(f)
	current, readErr := os.ReadFile(f.AbsPath)

	// The safety backup is mandatory, so a live file that exists but cannot be
	// read is a hard refusal rather than a skipped backup. Attempting the backup
	// only when the read succeeded, while letting the restore proceed whenever the
	// selected version read, destroyed an unreadable-but-present file with no
	// recovery copy: exactly what B2 says must never happen. A file that is simply
	// absent has nothing to lose and needs no safety copy.
	if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
		f.Unlock()
		s.logger.Printf("Refusing to restore %s: current file cannot be read: %v", f.RelPath, readErr)
		s.writeError(w, http.StatusInternalServerError, "current file cannot be read, so no safety backup is possible")
		return
	}

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

	// Restore writes the selected version's bytes without asserting any identity on
	// disk. The host never writes a documentid (spec §4): the id is re-injected at
	// the next serve from the tracked key, exactly as for a normal open, so a
	// restored-but-not-yet-saved file still resolves to the right history (model B′
	// rule 1 keys by the resident path). The version's own stored id is stripped,
	// never adopted; it may have come from a clone or a rename.
	data = htmlutil.StripToken(data)
	data = htmlutil.StripHTMLClayID(data)

	if wErr := atomicWriteFile(f.AbsPath, data); wErr != nil {
		f.Unlock()
		s.logger.Printf("Error restoring %s: %v", f.AbsPath, wErr)
		s.writeError(w, http.StatusInternalServerError, "write error")
		return
	}

	// A restore advances both per-file records, so it participates in save
	// suppression exactly like a save does, and emits a live notification.
	f.RecordServerWrite(versions.Hash(data))
	// Restoring engages the history, so it is no longer a throwaway first-open
	// snapshot: clear any provisional flag so it is never reclaimed.
	if pErr := s.versions.SetProvisional(key, f.AbsPath, false); pErr != nil {
		s.logger.Printf("Could not clear provisional flag for %s: %v", f.RelPath, pErr)
	}
	s.coord.acceptServerReplacement(f)
	s.broadcastDiskHTML(f, data)
	s.coord.notifyWarning(f, f.Name+" was restored from a backup")
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
