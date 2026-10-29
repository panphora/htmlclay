package server

import (
	"fmt"
	"net/http"

	"github.com/panphora/htmlclay/internal/session"
)

// The page menu's file actions. Each is a POST carrying the document's session
// token, gated by sameOrigin, and each answers with JSON rather than a page: the
// page names what it wants and the HOST does the work, so a failure is a sentence
// the page can show instead of a navigation that leaves it.

// documentActions names the actions this host offers for one document, or nil
// when the app wired no hook that could serve any of them.
func (s *Server) documentActions(token string) *documentActions {
	if s.hooks.Reveal == nil && s.hooks.OpenFolder == nil && s.hooks.ExportDocument == nil {
		return nil
	}
	var actions documentActions
	if s.hooks.Reveal != nil {
		actions.Reveal = &postAction{Post: "/_/reveal/" + token}
	}
	if s.hooks.OpenFolder != nil {
		actions.Versions = &postAction{Post: "/_/versions-folder/" + token}
	}
	if s.hooks.ExportDocument != nil {
		actions.Export = &postAction{Post: "/_/export/" + token}
	}
	return &actions
}

// actionDocument resolves the token and refuses a trusted write that has been
// revoked, which is the same admission every other route on a document applies.
func (s *Server) actionDocument(w http.ResponseWriter, r *http.Request) (*session.File, bool) {
	f, ok := s.lookupSession(w, r)
	if !ok {
		return nil, false
	}
	if s.trustedWriteRevoked(f) {
		s.writeError(w, http.StatusUnauthorized, "invalid token")
		return nil, false
	}
	return f, true
}

// writeActionReply answers a file action. A refused action names a plain sentence
// rather than an error code: the page shows it as written, to a person.
func writeActionReply(w http.ResponseWriter, status int, message string) {
	noStoreJSON(w)
	w.WriteHeader(status)
	if message == "" {
		fmt.Fprint(w, `{"ok":true}`)
		return
	}
	fmt.Fprintf(w, `{"ok":false,"message":%q}`, message)
}

func (s *Server) handleReveal(w http.ResponseWriter, r *http.Request) {
	f, ok := s.actionDocument(w, r)
	if !ok {
		return
	}
	hook := s.hooks.Reveal
	if hook == nil {
		writeActionReply(w, http.StatusNotFound, "Couldn't show the file.")
		return
	}
	if err := hook(f.AbsPath); err != nil {
		s.logger.Printf("Error revealing %s: %v", f.AbsPath, err)
		writeActionReply(w, http.StatusInternalServerError, "Couldn't show the file.")
		return
	}
	writeActionReply(w, http.StatusOK, "")
}

func (s *Server) handleVersionsFolder(w http.ResponseWriter, r *http.Request) {
	f, ok := s.actionDocument(w, r)
	if !ok {
		return
	}
	hook := s.hooks.OpenFolder
	if hook == nil {
		writeActionReply(w, http.StatusNotFound, "Couldn't open the versions folder.")
		return
	}
	f.Lock()
	dir, err := s.versions.Folder(historyKey(f), f.AbsPath)
	f.Unlock()
	if err == nil {
		err = hook(dir)
	}
	if err != nil {
		s.logger.Printf("Error opening the versions folder for %s: %v", f.AbsPath, err)
		writeActionReply(w, http.StatusInternalServerError, "Couldn't open the versions folder.")
		return
	}
	writeActionReply(w, http.StatusOK, "")
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	f, ok := s.actionDocument(w, r)
	if !ok {
		return
	}
	hook := s.hooks.ExportDocument
	if hook == nil {
		writeActionReply(w, http.StatusNotFound, "Couldn't export the document.")
		return
	}
	if err := hook(f.AbsPath); err != nil {
		s.logger.Printf("Error exporting %s: %v", f.AbsPath, err)
		writeActionReply(w, http.StatusInternalServerError, "Couldn't export the document.")
		return
	}
	writeActionReply(w, http.StatusOK, "")
}
