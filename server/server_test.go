package server

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/panphora/htmlclay/logging"
	"github.com/panphora/htmlclay/session"
	"github.com/panphora/htmlclay/versions"
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
