package server

import (
	"context"
	"errors"
	"net"
	"net/http"
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

	mux.HandleFunc("GET /f/{path...}", s.handleServeFile)
	mux.HandleFunc("GET /read/{token}", s.handleRead)
	mux.HandleFunc("POST /save/{token}", s.handleSave)
	mux.HandleFunc("GET /meta/{token}", s.handleMeta)

	handler := s.loggingMiddleware(mux)
	handler = HostValidationMiddleware(handler, port)

	s.httpServer = &http.Server{
		Handler: handler,
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

func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(rw, r)
		s.logger.Printf("%s %s %d %s", r.Method, r.URL.Path, rw.status, time.Since(start))
	})
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}
