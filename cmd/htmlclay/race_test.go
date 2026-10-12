package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/panphora/htmlclay/internal/session"
)

// The interleaving the reviewers reproduced: the router has looked a site up and not yet
// armed it when the folder is untrusted. Every layer must refuse: the arm, the manager,
// the hosted server, and the router.
func TestClosedSiteNeverServesAfterALateArm(t *testing.T) {
	home, _ := resolveSymlinks(t.TempDir())
	proj := filepath.Join(home, "proj")
	page := filepath.Join(proj, "index.htmlclay")
	writeTestFile(t, page, "<html><body>REVIEW_CANARY</body></html>")
	a := newTestApp(t, home)
	if err := a.trustFolder(proj); err != nil {
		t.Fatal(err)
	}
	s := liveSite(t, a, proj)
	live := s.url("proj/index.htmlclay")
	if code, body := fetchNav(t, live); code != 200 || !strings.Contains(body, "REVIEW_CANARY") {
		t.Fatalf("precondition: %d %s", code, body)
	}
	if err := a.untrustFolder(proj); err != nil {
		t.Fatal(err)
	}
	s.armOnce = sync.Once{}
	s.armOnce.Do(s.arm)
	if s.sessions.TrustedCovers(page) {
		t.Fatal("a late arm re-installed the trusted root")
	}
	if err := s.sessions.InstallTrustedRoot(proj); !errors.Is(err, session.ErrRevoked) {
		t.Fatalf("InstallTrustedRoot after close = %v, want ErrRevoked", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/proj/index.htmlclay", nil)
	req.Host = s.host
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Site", "none")
	w := httptest.NewRecorder()
	s.srv.ServeHTTP(w, req)
	if w.Code == http.StatusOK || strings.Contains(w.Body.String(), "REVIEW_CANARY") {
		t.Fatalf("the closed site's server served the file: %d %s", w.Code, w.Body.String())
	}
	code, body := fetchNav(t, live)
	if code != http.StatusNotFound || strings.Contains(body, "REVIEW_CANARY") {
		t.Fatalf("the retired hostname served the file: %d %s", code, body)
	}
	if !s.closed.Load() {
		t.Fatal("close did not mark the site closed")
	}
	select {
	case <-s.ctx.Done():
	default:
		t.Fatal("close did not cancel the site's context")
	}
}
