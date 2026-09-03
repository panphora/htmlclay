package server

import (
	_ "embed"
	"net/http"
)

// The shared live-sync worker of spec §10. A SharedWorker script must be served
// from the page's own origin, which is this host, so the client library cannot
// bring its own: every host that announces `sync` serves this file.
//
// The bytes are a constant and touch nothing on disk. They are served outside
// sameOrigin because a worker script is fetched as a same-origin subresource by
// whichever page asks, and refusing it would only strand a page that has already
// learned from /_/meta that the worker is here.
//
// no-cache rather than no-store: the browser revalidates on every worker start,
// so an upgraded host is picked up the next time the origin has no worker
// running. A worker already running keeps its code until every tab on the origin
// closes, which the versioned page contract (`v: 1`) makes safe.

//go:embed sync-worker.js
var syncWorkerJS []byte

func (s *Server) handleSyncWorker(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(syncWorkerJS)
}
