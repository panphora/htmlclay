package server

import (
	"net/http"
	"path/filepath"
	"strings"

	"github.com/panphora/htmlclay/internal/session"
)

// autoRegisterCap bounds how many trusted-folder auto-registration attempts one
// site will make in its lifetime. Each registration is permanent state (a token,
// a live-sync key); a page must not be able to mint them without bound. Past the
// cap, files in a trusted folder still serve read-only with the banner, so
// nothing breaks — it stops being automatic.
const autoRegisterCap = 256

// writeTrustRefused is the fixed refusal for the page's trust request: no path,
// no reason, same body every time.
func writeTrustRefused(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusForbidden)
	w.Write([]byte(`{"ok":false,"error":"denied"}`))
}

// handleTrustRequest lets a page ask for its own folder to be trusted. Two
// structural limits do most of the security work: the folder is derived from the
// requesting token's file (no folder argument exists), and only a file the human
// opened themselves may ask. A token minted by auto-registration can never
// declare anything, which is why an already-covered file is answered before the
// provenance check rather than refused by it.
//
// It shares its app-side action with the read-only banner's button
// (handleOpenRequest). The two differ only in the front door: a token here, a
// nonce there, and openedByUser true here because the user opened this file.
func (s *Server) handleTrustRequest(w http.ResponseWriter, r *http.Request) {
	f, ok := s.lookupSession(w, r)
	if !ok {
		return
	}
	hook := s.hooks.TrustRequest
	if hook == nil {
		writeTrustRefused(w)
		return
	}
	// Already inside a live trusted folder: trusting again grants nothing, so
	// answer without a dialog and without a log line. This runs first because a
	// page that asks on every load would otherwise log a refusal for every
	// sibling on every view.
	//
	// The LIVE question, not the lexical one. A trusted folder deleted and
	// recreated still covers its files lexically while granting them nothing, and
	// answering yes here would make that state permanent: the page would be told
	// it is already trusted and never reach the dialog that re-pins the folder now
	// on disk.
	if s.trustedLive(f.AbsPath) {
		noStoreJSON(w)
		w.Write([]byte(`{"ok":true}`))
		return
	}
	via := s.sessions.Via(f.AbsPath)
	if !via.Has(session.ViaOsOpen) {
		s.logger.Printf("Trust request refused for %s: the user never opened this file", f.RelPath)
		writeTrustRefused(w)
		return
	}
	if s.trustDeniedCovers(f.AbsPath) {
		writeTrustRefused(w)
		return
	}

	var allowed, suppressed bool
	// One native dialog at a time, shared with every other prompt this site can
	// raise. The denial is checked again inside the lock: selecting a folder of
	// files and opening them at once queues one ask per file, and without the
	// second check every ask that cleared the first one before the user said No
	// goes on to raise its own dialog.
	if !s.broker.runPrompt(func() {
		if s.trustDeniedCovers(f.AbsPath) {
			suppressed = true
			return
		}
		_, allowed = hook(f.AbsPath, true)
	}) {
		writeTrustRefused(w)
		return
	}
	if suppressed {
		writeTrustRefused(w)
		return
	}
	if !allowed {
		s.suppressTrustDenied(filepath.Dir(f.AbsPath))
		s.logger.Printf("Trust request denied for %s; folder suppressed for this session", f.RelPath)
		writeTrustRefused(w)
		return
	}

	noStoreJSON(w)
	w.Write([]byte(`{"ok":true}`))
}

// suppressTrustDenied stops a denied folder from re-asking for the session.
func (s *Server) suppressTrustDenied(dir string) {
	s.openMu.Lock()
	defer s.openMu.Unlock()
	s.trustDenied = append(s.trustDenied, dir)
}

func (s *Server) trustDeniedCovers(path string) bool {
	s.openMu.Lock()
	defer s.openMu.Unlock()
	for _, root := range s.trustDenied {
		if session.EqualOrUnder(path, root) {
			return true
		}
	}
	return false
}

// trustedCovers asks the app whether absPath sits inside a declared trusted
// folder. With no hook wired nothing is covered, which disables
// auto-registration for a standalone server.
func (s *Server) trustedCovers(absPath string) bool {
	if s.hooks.TrustedCovers == nil {
		return false
	}
	return s.hooks.TrustedCovers(absPath)
}

// trustedLive asks the app the identity-checked version of the same question.
// The two are separate hooks because they are separate questions and only one of
// them may touch the disk; see Hooks.TrustedLive. With no hook wired nothing is
// live, so a standalone server always reaches the dialog.
func (s *Server) trustedLive(absPath string) bool {
	if s.hooks.TrustedLive == nil {
		return false
	}
	return s.hooks.TrustedLive(absPath)
}

// shouldAutoRegister decides, with no filesystem access at all, whether an
// unregistered path auto-registers. Order is load-bearing (T1.9): trusted-folder
// scope first (a memory lookup), then the hidden/internal string checks — so the
// refusal for a path outside every trusted folder is indistinguishable from the
// refusal for a hidden one, and neither reveals whether anything exists at the
// path. This is why TrustedCovers is documented as lexical: a stat in there
// would put a filesystem answer ahead of the string checks and undo the whole
// ordering.
func (s *Server) shouldAutoRegister(r *http.Request, absPath string) bool {
	if s.hooks.Route == nil {
		return false
	}
	if !strings.EqualFold(filepath.Ext(absPath), ".htmlclay") {
		return false
	}
	// Same gate as token injection (T1.10): only a real document load
	// registers. A silent fetch() of a sibling serves read-only through
	// serveAsset instead.
	if dest := r.Header.Get("Sec-Fetch-Dest"); dest != "" && dest != "document" {
		return false
	}
	if !s.trustedCovers(absPath) {
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
// failure falls back to the plain read-only asset path, which is also what
// happens when the folder's identity pin no longer matches.
func (s *Server) autoRegister(w http.ResponseWriter, r *http.Request, absPath string) (*session.File, bool) {
	url, ok := s.hooks.Route(absPath)
	if !ok {
		return nil, false
	}
	if f, ok := s.sessions.LookupByPath(absPath); ok {
		return f, false
	}
	http.Redirect(w, r, url, http.StatusFound)
	return nil, true
}
