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
)

type Server struct {
	httpServer *http.Server
	listener   net.Listener
	sessions   *session.Manager
	port       int
	logger     *logging.Logger
}

func New(ln net.Listener, sessions *session.Manager, logger *logging.Logger) *Server {
	port := ln.Addr().(*net.TCPAddr).Port
	s := &Server{
		listener: ln,
		sessions: sessions,
		port:     port,
		logger:   logger,
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /_/read/{token}", s.handleRead)
	mux.HandleFunc("POST /_/save/{token}", s.handleSave)
	mux.HandleFunc("GET /_/meta/{token}", s.handleMeta)
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

func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

func (s *Server) Close() error {
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
	for _, prefix := range []string{"/_/save/", "/_/read/", "/_/meta/"} {
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
