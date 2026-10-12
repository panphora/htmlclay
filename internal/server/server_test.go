package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/panphora/htmlclay/internal/logging"
	"github.com/panphora/htmlclay/internal/session"
	"github.com/panphora/htmlclay/internal/versions"
)

// newTestManager builds a Manager whose held read-root handles are closed when the
// test ends. Windows refuses to remove a directory while a handle to it is open, so
// a Manager left holding its os.Root fails the t.TempDir cleanup and marks the test
// failed even though every assertion passed. POSIX unlink hides this everywhere else,
// which is why it only ever surfaced on the Windows CI runner.
//
// Construct managers in tests through this, not session.NewManagerWithHome.
func newTestManager(t *testing.T, home string) *session.Manager {
	t.Helper()
	m := session.NewManagerWithHome(home)
	t.Cleanup(m.RevokeAll)
	return m
}

func TestHostValidationMiddleware(t *testing.T) {
	mgr := newTestManager(t, t.TempDir())
	logger := logging.NewStdout()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv := New(ln, mgr, logger, versions.New(t.TempDir()))

	req := httptest.NewRequest("GET", "/test.htmlclay", nil)
	req.Host = "evil.com:12345"
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

func TestHostValidationAccepts(t *testing.T) {
	mgr := newTestManager(t, t.TempDir())
	logger := logging.NewStdout()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv := New(ln, mgr, logger, versions.New(t.TempDir()))

	req := httptest.NewRequest("GET", "/nonexistent", nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	// A valid host reaches the handler, where this out-of-scope path draws the read
	// gate's own fixed 403 (tagged with X-HTMLClay-Error). Only an untagged 403 is
	// the middleware's rejection. The old expectation of any non-403 relied on a
	// missing path short-circuiting to 404, which was the existence oracle.
	if w.Code == http.StatusForbidden && w.Header().Get("X-HTMLClay-Error") == "" {
		t.Error("should not be forbidden for valid host")
	}
}

func TestHostedServerAnswersItsOwnHostOnly(t *testing.T) {
	home := t.TempDir()
	mgr := newTestManager(t, home)
	store := versions.New(t.TempDir())
	ls := NewLiveSync(store, logging.NewStdout())
	t.Cleanup(ls.Shutdown)
	ours := func(h string) bool { return IsSiteHost(h, 24190) }
	srv := NewHosted("w4-3f2a.localhost:24190", ours, mgr, logging.NewStdout(), store, ls)
	if srv.Host() != "w4-3f2a.localhost:24190" || srv.port != 24190 {
		t.Fatalf("host %q port %d", srv.Host(), srv.port)
	}
	if err := srv.Start(); err == nil {
		t.Fatal("a hosted server must refuse Start: it has no listener")
	}
	req := httptest.NewRequest("GET", "/_/meta", nil)
	req.Host = "w4-3f2a.localhost:24190"
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("own host: %d, want 200", w.Code)
	}
	req.Host = "127.0.0.1:24190"
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("foreign host: %d, want 403", w.Code)
	}
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
