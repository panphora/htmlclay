package server

import (
	"net/http"
	"path/filepath"
	"strings"

	"github.com/panphora/htmlclay/session"
)

// autoRegisterCap bounds how many workspace auto-registration attempts one site
// will make in its lifetime. Each registration is permanent state (a token, a
// live-sync key); a page must not be able to mint them without bound. Past the
// cap, workspace files still serve read-only with the open banner, so nothing
// breaks — it stops being automatic.
const autoRegisterCap = 256

// workspaceRequestFunc is the app-level seam a page's workspace request routes
// through: refusal list, native dialog, config write, and per-site seeding all
// live behind it. It receives the requesting file's path (the folder is that
// file's directory — the page never names a folder) and reports whether the
// workspace was declared.
type workspaceRequestFunc func(requestingFile string) bool

// SetWorkspaceRequest wires the app-level workspace seam. With no hook wired
// the endpoint refuses everything.
func (s *Server) SetWorkspaceRequest(fn workspaceRequestFunc) { s.workspaceRequest = fn }

// registerSeam is the app-level open seam used by workspace auto-registration:
// route the path with workspace provenance, return the URL it is served at.
type registerSeamFunc func(absPath string) (url string, ok bool)

// SetRegisterSeam wires the auto-registration seam. Nil disables auto-register.
func (s *Server) SetRegisterSeam(fn registerSeamFunc) { s.registerSeam = fn }

// writeWorkspaceRefused is the fixed refusal for /_/workspace-request: no path,
// no reason, same body every time.
func writeWorkspaceRefused(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusForbidden)
	w.Write([]byte(`{"ok":false,"error":"denied"}`))
}

// handleWorkspaceRequest lets a page ask for its own folder to become a
// workspace. Two structural limits do most of the security work: the folder is
// derived from the requesting token's file (no folder argument exists), and
// only a token whose file the human personally opened (ViaOsOpen) may ask — a
// token minted by the open-request banner or by auto-registration is refused,
// which severs the read→write ladder at its first rung.
func (s *Server) handleWorkspaceRequest(w http.ResponseWriter, r *http.Request) {
	f, ok := s.lookupSession(w, r)
	if !ok {
		return
	}
	if !s.sessions.Via(f.AbsPath).Has(session.ViaOsOpen) {
		s.logger.Printf("Workspace request refused for %s: not an OS-opened file", f.RelPath)
		writeWorkspaceRefused(w)
		return
	}
	if s.workspaceDeniedCovers(f.AbsPath) {
		writeWorkspaceRefused(w)
		return
	}
	hook := s.workspaceRequest
	if hook == nil {
		writeWorkspaceRefused(w)
		return
	}

	var allowed bool
	// One native dialog at a time, shared with every other prompt this site can
	// raise.
	if !s.broker.runPrompt(func() { allowed = hook(f.AbsPath) }) {
		writeWorkspaceRefused(w)
		return
	}
	if !allowed {
		s.suppressWorkspaceDenied(filepath.Dir(f.AbsPath))
		s.logger.Printf("Workspace request denied for %s; folder suppressed for this session", f.RelPath)
		writeWorkspaceRefused(w)
		return
	}

	s.logger.Printf("Workspace declared from page request by %s", f.RelPath)
	noStoreJSON(w)
	w.Write([]byte(`{"ok":true}`))
}

// suppressWorkspaceDenied stops a denied folder from re-asking for the session.
func (s *Server) suppressWorkspaceDenied(dir string) {
	s.openMu.Lock()
	defer s.openMu.Unlock()
	s.workspaceDenied = append(s.workspaceDenied, dir)
}

func (s *Server) workspaceDeniedCovers(path string) bool {
	s.openMu.Lock()
	defer s.openMu.Unlock()
	for _, root := range s.workspaceDenied {
		if session.EqualOrUnder(path, root) {
			return true
		}
	}
	return false
}

// shouldAutoRegister decides, with no filesystem access at all, whether an
// unregistered path auto-registers. Order is load-bearing (T1.9): workspace
// scope first (a map lookup), then the hidden/internal string checks — so the
// refusal for an out-of-workspace path is indistinguishable from the refusal
// for a hidden one, and neither reveals whether anything exists at the path.
func (s *Server) shouldAutoRegister(r *http.Request, absPath string) bool {
	if s.registerSeam == nil {
		return false
	}
	if !strings.EqualFold(filepath.Ext(absPath), ".htmlclay") {
		return false
	}
	// Same gate as token injection (T1.10): only a real document load
	// registers. A silent fetch() of a workspace sibling serves read-only
	// through serveAsset instead.
	if dest := r.Header.Get("Sec-Fetch-Dest"); dest != "" && dest != "document" {
		return false
	}
	if !s.sessions.WorkspaceCovers(absPath) {
		return false
	}
	if session.HasHiddenComponent(s.sessions.HomeDir(), absPath) {
		return false
	}
	if s.isInternal(absPath) {
		return false
	}
	s.openMu.Lock()
	defer s.openMu.Unlock()
	if s.autoRegistered >= autoRegisterCap {
		return false
	}
	s.autoRegistered++
	return true
}

// autoRegister routes absPath through the app seam. When the seam lands the
// registration in this site, the caller serves it inline; when another site
// already hosts it (or hosts it now), the response is a redirect to that
// site's origin, preserving one File and one origin per path (T0.1). A seam
// failure falls back to the plain read-only asset path.
func (s *Server) autoRegister(w http.ResponseWriter, r *http.Request, absPath string) (*session.File, bool) {
	url, ok := s.registerSeam(absPath)
	if !ok {
		return nil, false
	}
	if f, ok := s.sessions.LookupByPath(absPath); ok {
		return f, false
	}
	http.Redirect(w, r, url, http.StatusFound)
	return nil, true
}
