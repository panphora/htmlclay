package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/panphora/htmlclay/logging"
	"github.com/panphora/htmlclay/session"
	"github.com/panphora/htmlclay/versions"
)

type Server struct {
	httpServer *http.Server
	listener   net.Listener
	sessions   *session.Manager
	port       int
	logger     *logging.Logger
	versions   *versions.Store
	hub        *hub
	watcher    *watcher
}

func New(ln net.Listener, sessions *session.Manager, logger *logging.Logger, store *versions.Store) *Server {
	port := ln.Addr().(*net.TCPAddr).Port
	h := newHub()
	s := &Server{
		listener: ln,
		sessions: sessions,
		port:     port,
		logger:   logger,
		versions: store,
		hub:      h,
		watcher:  newWatcher(h, logger),
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /_/read/{token}", s.handleRead)
	mux.HandleFunc("POST /_/save/{token}", s.handleSave)
	mux.HandleFunc("GET /_/meta/{token}", s.handleMeta)
	mux.HandleFunc("GET /_/versions/{token}", s.handleListVersions)
	mux.HandleFunc("GET /_/version/{token}/{name}", s.handleReadVersion)
	mux.HandleFunc("POST /_/restore/{token}/{name}", s.handleRestoreVersion)
	// Registered ahead of the catch-all.
	mux.HandleFunc("GET /_/live-sync/stream", s.handleLiveSyncStream)
	mux.HandleFunc("POST /_/live-sync/save", s.handleLiveSyncSave)
	mux.HandleFunc("GET /{path...}", s.handleServeFile)

	handler := s.loggingMiddleware(mux)
	handler = HostValidationMiddleware(handler, port)

	s.httpServer = &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	return s
}

func (s *Server) Start() error {
	s.logger.Printf("Server listening on 127.0.0.1:%d", s.port)
	err := s.httpServer.Serve(s.listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown closes every SSE stream before handing off to http.Server.Shutdown.
// Without that, active streams hold graceful shutdown open until its timeout and
// are then force-closed.
func (s *Server) Shutdown(ctx context.Context) error {
	s.hub.shutdown()
	s.watcher.shutdown()
	return s.httpServer.Shutdown(ctx)
}

func (s *Server) Close() error {
	s.hub.shutdown()
	s.watcher.shutdown()
	return s.httpServer.Close()
}

func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(rw, r)
		s.logger.Printf("%s %s %d %s", r.Method, redactPath(r.URL.Path), rw.status, time.Since(start))
	})
}

// redactPath hides the session token in token-bearing routes so the secret is
// never written to the log file or stderr.
func redactPath(p string) string {
	for _, prefix := range []string{
		"/_/save/", "/_/read/", "/_/meta/",
		"/_/versions/", "/_/version/", "/_/restore/",
	} {
		if strings.HasPrefix(p, prefix) {
			return prefix + "<redacted>"
		}
	}
	return p
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the underlying writer for Flush and
// SetWriteDeadline. Without it SSE cannot flush at all.
func (rw *responseWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}

func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
