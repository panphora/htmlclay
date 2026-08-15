package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/panphora/htmlclay/logging"
	"github.com/panphora/htmlclay/session"
	"github.com/panphora/htmlclay/versions"
)

type Server struct {
	httpServer   *http.Server
	listener     net.Listener
	sessions     *session.Manager
	port         int
	logger       *logging.Logger
	versions     *versions.Store
	internalDir  string
	broker       *broker
	ls           *LiveSync
	ownsLiveSync bool
	hub          *hub
	watcher      *watcher
	coord        *streamCoordinator

	// hooks are the app-level decisions this server cannot make itself. A nil
	// field disables that route; a zero Hooks (tests, standalone servers)
	// disables all of them.
	hooks Hooks
	// openMu guards the open-request nonce map, the denied-directory lists, and
	// the auto-registration counter.
	openMu         sync.Mutex
	openNonces     map[string]openNonce
	openDenied     []string
	trustDenied    []string
	autoRegistered int
}

// SeqPath is where the live-sync sequence high-water mark lives, beside the
// backups in a private 0700 directory the server refuses to serve.
func SeqPath(store *versions.Store) string {
	return filepath.Join(store.BaseDir(), ".livesync-seq")
}

// New builds a server that owns its own live-sync runtime and tears it down on
// Shutdown/Close. Used by tests and any single-server caller.
func New(ln net.Listener, sessions *session.Manager, logger *logging.Logger, store *versions.Store) *Server {
	s := newServer(ln, sessions, logger, store, NewLiveSync(SeqPath(store), logger))
	s.ownsLiveSync = true
	return s
}

// NewWithLiveSync builds a server that shares an injected live-sync runtime with
// its sibling sites. The runtime is owned by the process, not by any one server,
// so Shutdown/Close leave it running; the process shuts it down once.
func NewWithLiveSync(ln net.Listener, sessions *session.Manager, logger *logging.Logger, store *versions.Store, ls *LiveSync) *Server {
	return newServer(ln, sessions, logger, store, ls)
}

func newServer(ln net.Listener, sessions *session.Manager, logger *logging.Logger, store *versions.Store, ls *LiveSync) *Server {
	port := ln.Addr().(*net.TCPAddr).Port
	s := &Server{
		listener: ln,
		sessions: sessions,
		port:     port,
		logger:   logger,
		versions: store,
		broker:   newBroker(sessions, logger, defaultConfirm),
		ls:       ls,
		hub:      ls.hub,
		watcher:  ls.watcher,
		coord:    ls.coord,
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /_/read/{token}", s.handleRead)
	mux.HandleFunc("POST /_/save/{token}", sameOrigin(s.handleSave))
	mux.HandleFunc("GET /_/meta/{token}", s.handleMeta)
	mux.HandleFunc("GET /_/versions/{token}", s.handleListVersions)
	mux.HandleFunc("GET /_/version/{token}/{name}", s.handleReadVersion)
	mux.HandleFunc("POST /_/restore/{token}/{name}", sameOrigin(s.handleRestoreVersion))
	// Registered ahead of the catch-all. The stream is a read, but it joins the
	// gate: live-sync authorizes by registered path rather than by token, so
	// without the gate it is reachable by any page the user's browser has open.
	mux.HandleFunc("GET /_/live-sync/stream", sameOrigin(s.handleLiveSyncStream))
	mux.HandleFunc("POST /_/live-sync/save", sameOrigin(s.handleLiveSyncSave))
	// Both endpoints keep their 1.2.0 URLs. User HTML calls
	// /_/workspace-request/{token} directly, so the concept renames in Go and
	// not on the wire; /_/open-request is only ever called by bytes this server
	// injects, but there is nothing to gain by moving it.
	mux.HandleFunc("POST /_/open-request", sameOrigin(s.handleOpenRequest))
	mux.HandleFunc("POST /_/workspace-request/{token}", sameOrigin(s.handleTrustRequest))
	// The data API sits ahead of the catch-all. Verified with httptest: "/_/api/{path...}" does NOT
	// match bare "/_/api" (ServeMux 307-redirects it), hence the explicit first line; "/_/api/" with
	// a trailing slash DOES match the wildcard with path == "", so the handler answers 400 for an
	// empty path either way; "/_/apix" still falls to the catch-all; and "/_%2Fapi/x" decodes into a
	// single segment and cannot smuggle into this route.
	//
	// This shadows any real user file at ~/_/api/…, which is the one behavior change.
	mux.HandleFunc("GET /_/api", s.handleDataAPI)
	mux.HandleFunc("GET /_/api/{path...}", s.handleDataAPI)

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

// SetInternalDir marks a directory the server must never serve, whatever the
// read roots say. htmlclay's own config tree holds its log and settings, and a
// grant that happens to cover that directory must not turn into a read path.
// Denying on the serve path is structural; a grant-time guard can only ever
// cover the grants it happens to see.
func (s *Server) SetInternalDir(dir string) { s.internalDir = dir }

// SetSiteLabel names this site in the permission dialog so the user knows which
// page is asking. Optional; the broker falls back to a generic label.
func (s *Server) SetSiteLabel(label string) { s.broker.label = label }

// isInternal reports whether absPath belongs to htmlclay's own state and must be
// refused outright, before any existence check, so the denial is not an oracle.
func (s *Server) isInternal(absPath string) bool {
	if s.versions.Contains(absPath) {
		return true
	}
	return s.internalDir != "" && session.EqualOrUnder(absPath, s.internalDir)
}

func (s *Server) Start() error {
	s.logger.Printf("Server listening on 127.0.0.1:%d", s.port)
	err := s.httpServer.Serve(s.listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown releases parked permission requests and closes every SSE stream
// before handing off to http.Server.Shutdown. Both otherwise hold graceful
// shutdown open until its timeout and are then force-closed. A server that shares
// an injected live-sync runtime leaves it running (the process shuts the shared
// runtime down once, before the per-site HTTP servers).
func (s *Server) Shutdown(ctx context.Context) error {
	s.broker.shutdown()
	if s.ownsLiveSync {
		s.ls.Shutdown()
	}
	return s.httpServer.Shutdown(ctx)
}

func (s *Server) Close() error {
	s.broker.shutdown()
	if s.ownsLiveSync {
		s.ls.Shutdown()
	}
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
		"/_/workspace-request/",
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
