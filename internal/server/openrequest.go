package server

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/panphora/htmlclay/internal/htmlutil"
	"github.com/panphora/htmlclay/internal/session"
)

const (
	// openNonceTTL bounds how long a served banner's offer stays redeemable, so
	// a nonce sitting in a backgrounded tab cannot be replayed hours later. A
	// reload mints a fresh one, which is the recovery the banner suggests.
	openNonceTTL = 10 * time.Minute
	// openNonceCap bounds the outstanding-nonce map. Minting requires a real
	// user navigation, so the cap is generous for humans and a wall for loops;
	// at the cap new serves simply go bannerless until nonces expire.
	openNonceCap = 512
)

// openNonce is one single-use offer to open the exact file that was served.
// realPath is the descriptor-verified location of the served bytes
// (platform.RealPath at serve time), so the offer names and opens the file the
// user actually looked at, not whatever a pathname later points to.
type openNonce struct {
	realPath string
	expires  time.Time
}

func generateNonce() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("cannot generate nonce: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// mintOpenNonce records a single-use offer for realPath and returns its nonce.
func (s *Server) mintOpenNonce(realPath string) (string, error) {
	nonce, err := generateNonce()
	if err != nil {
		return "", err
	}
	now := time.Now()
	s.openMu.Lock()
	defer s.openMu.Unlock()
	if s.openNonces == nil {
		s.openNonces = make(map[string]openNonce)
	}
	for n, o := range s.openNonces {
		if o.expires.Before(now) {
			delete(s.openNonces, n)
		}
	}
	if len(s.openNonces) >= openNonceCap {
		return "", fmt.Errorf("open-nonce cap reached")
	}
	s.openNonces[nonce] = openNonce{realPath: realPath, expires: now.Add(openNonceTTL)}
	return nonce, nil
}

// redeemOpenNonce burns nonce and returns the file it was bound to. Burning
// happens on first presentation, whatever the dialog's outcome: a second click
// needs a reload, and a denied offer cannot be re-presented.
func (s *Server) redeemOpenNonce(nonce string) (string, bool) {
	s.openMu.Lock()
	defer s.openMu.Unlock()
	o, ok := s.openNonces[nonce]
	if !ok {
		return "", false
	}
	delete(s.openNonces, nonce)
	if o.expires.Before(time.Now()) {
		return "", false
	}
	return o.realPath, true
}

// suppressOpenDenied stops the whole directory from re-asking for the rest of
// the session, matching the read broker's denied-ancestor suppression: one Deny
// answers for the tree, not just the one path.
func (s *Server) suppressOpenDenied(dir string) {
	s.openMu.Lock()
	defer s.openMu.Unlock()
	s.openDenied = append(s.openDenied, dir)
}

func (s *Server) openDeniedCovers(path string) bool {
	s.openMu.Lock()
	defer s.openMu.Unlock()
	for _, root := range s.openDenied {
		if session.EqualOrUnder(path, root) {
			return true
		}
	}
	return false
}

// shouldOfferOpen reports whether this read-only serve gets the banner: an
// HTML Clay document, fetched by a real user navigation (Sec-Fetch-User is only
// sent on user-activated navigations, and Dest must be document — a silent
// fetch() or iframe never mints a nonce), with the trust seam wired and the
// file's directory not under a this-session Deny.
func (s *Server) shouldOfferOpen(r *http.Request, real string) bool {
	if s.hooks.TrustRequest == nil {
		return false
	}
	if r.Method != http.MethodGet {
		return false
	}
	if !strings.EqualFold(filepath.Ext(real), ".htmlclay") {
		return false
	}
	if r.Header.Get("Sec-Fetch-Dest") != "document" || r.Header.Get("Sec-Fetch-User") != "?1" {
		return false
	}
	return !s.openDeniedCovers(real)
}

// serveReadOnlyWithBanner is the forked serve path for a bannered document. It
// buffers and injects rather than streaming, and deliberately bypasses
// ServeContent and every cache validator: injected bytes would corrupt the
// content ETag and Range math, and a 304 could revive a burned nonce's banner.
func (s *Server) serveReadOnlyWithBanner(w http.ResponseWriter, file *os.File, real string) {
	data, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	served := data
	if nonce, err := s.mintOpenNonce(real); err == nil {
		served = htmlutil.InjectBanner(data, htmlutil.WrapBanner(openBannerHTML(nonce)))
	} else {
		s.logger.Printf("No open banner for %s: %v", real, err)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(served)
}

// openBannerHTML renders the read-only banner. Fixed-position, so its
// end-of-document DOM placement is invisible; everything inline, so it needs no
// asset requests of its own. The page holds only the nonce — never a path —
// and the nonce resolves server-side to the exact file that was served.
//
// The button trusts the file's whole folder rather than opening the one file,
// so the wording names the folder and the native dialog that follows states the
// full consequence. One click from a read-only page to an editable project is
// the point; the dialog is what makes it a decision.
func openBannerHTML(nonce string) []byte {
	return []byte(`<div style="position:fixed;left:0;right:0;bottom:0;z-index:2147483647;display:flex;align-items:center;gap:12px;padding:10px 16px;background:#1c1c1e;color:#f2f2f7;font:13px/1.4 -apple-system,system-ui,sans-serif;box-shadow:0 -1px 8px rgba(0,0,0,.35)">` +
		`<span style="flex:1">Read-only view. This file was opened from a link.</span>` +
		`<button style="padding:6px 14px;border:0;border-radius:6px;background:#0a84ff;color:#fff;font:inherit;cursor:pointer" ` +
		`onclick="var e=this,b=e.parentNode;e.disabled=true;fetch('/_/open-request',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({nonce:'` + nonce + `'})}).then(function(r){return r.json()}).then(function(d){if(d&&d.ok&&d.url){location.href=d.url}else{b.firstElementChild.textContent=(d&&d.error)==='expired'?'This offer expired. Reload the page to ask again.':'Not trusted.';e.remove()}}).catch(function(){e.disabled=false})">Trust this folder</button>` +
		`<button style="padding:6px 10px;border:0;border-radius:6px;background:#3a3a3c;color:#f2f2f7;font:inherit;cursor:pointer" onclick="this.parentNode.remove()">Dismiss</button>` +
		`</div>`)
}

// writeOpenRefused is the fixed refusal for /_/open-request. Like write403 it
// carries no path and no reason detail beyond the one coarse code the banner
// needs for its wording, so a page cannot mine refusals.
func writeOpenRefused(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusForbidden)
	fmt.Fprintf(w, `{"ok":false,"error":%q}`, code)
}

// handleOpenRequest turns a banner click into a native dialog and, on approval,
// trust for the served file's folder. The request names only a nonce; the file
// identity, the folder derived from it, the dialog text, and the resulting URL
// are all server-side facts, so the page cannot steer any of them.
//
// openedByUser is false here on purpose: the banner is shown precisely because
// the file was reached by a link rather than opened, and the dialog says so on
// its first line.
func (s *Server) handleOpenRequest(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenRefused(w, "denied")
		return
	}
	var payload struct {
		Nonce string `json:"nonce"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Nonce == "" {
		s.writeError(w, http.StatusBadRequest, "missing nonce")
		return
	}

	real, ok := s.redeemOpenNonce(payload.Nonce)
	if !ok {
		writeOpenRefused(w, "expired")
		return
	}
	if s.openDeniedCovers(real) {
		writeOpenRefused(w, "denied")
		return
	}
	hook := s.hooks.TrustRequest
	if hook == nil {
		writeOpenRefused(w, "denied")
		return
	}

	var url string
	var allowed bool
	// The dialog runs under the broker's one prompting flag, so it can never
	// stack over (or under) a read-grant prompt.
	if !s.broker.runPrompt(func() { url, allowed = hook(real, false) }) {
		writeOpenRefused(w, "denied")
		return
	}
	if !allowed {
		s.suppressOpenDenied(filepath.Dir(real))
		s.logger.Printf("Trust request from the banner denied for %s; directory suppressed for this session", real)
		writeOpenRefused(w, "denied")
		return
	}

	s.logger.Printf("Trust request from the banner approved for %s", real)
	noStoreJSON(w)
	fmt.Fprintf(w, `{"ok":true,"url":%q}`, url)
}
