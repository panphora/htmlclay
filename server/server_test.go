package server

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/panphora/htmlclay/logging"
	"github.com/panphora/htmlclay/session"
)

func TestHostValidationMiddleware(t *testing.T) {
	mgr := session.NewManagerWithHome(t.TempDir())
	logger := logging.NewStdout()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv := New(ln, mgr, logger)

	req := httptest.NewRequest("GET", "/f/test.htmlclay", nil)
	req.Host = "evil.com:12345"
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

func TestHostValidationAccepts(t *testing.T) {
	mgr := session.NewManagerWithHome(t.TempDir())
	logger := logging.NewStdout()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv := New(ln, mgr, logger)

	req := httptest.NewRequest("GET", "/f/nonexistent", nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code == http.StatusForbidden {
		t.Error("should not be forbidden for valid host")
	}
}
