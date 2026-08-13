package server

import (
	"github.com/panphora/htmlclay/logging"
)

// LiveSync bundles the process-wide live-sync machinery: the pub/sub hub, the
// file watcher, and the coordinator that keeps their membership in step. One
// LiveSync is shared by every per-tree site. It keys all state by absolute file
// path, and absolute paths are unique across the single home directory, so
// sharing one instance never collides between sites.
type LiveSync struct {
	hub     *hub
	watcher *watcher
	coord   *streamCoordinator
}

// NewLiveSync builds the shared live-sync runtime. seqPath is the persisted
// sequence high-water mark, which lives beside the backups in a private 0700
// directory the server refuses to serve.
func NewLiveSync(seqPath string, logger *logging.Logger) *LiveSync {
	h := newHub(seqPath)
	wt := newWatcher(logger)
	co := newStreamCoordinator(h, wt)
	wt.coord = co
	h.startJanitor()
	return &LiveSync{hub: h, watcher: wt, coord: co}
}

// Shutdown stops the watcher and closes every SSE stream across all sites. Call
// it once at process exit, before shutting down the per-site HTTP servers, so
// graceful shutdown does not wait on open SSE connections.
func (ls *LiveSync) Shutdown() {
	ls.hub.shutdown()
	ls.watcher.shutdown()
}

// DropSubscribers closes every SSE stream for path, across every site. Used by
// revocation: a live stream resolved its *session.File once and would otherwise
// loop forever after the registration died. Each stopped stream unwinds through
// its handler's deferred coordinator remove, which drops the watcher reference
// and hub membership idempotently — teardown by eviction, not a second
// bookkeeping path.
func (ls *LiveSync) DropSubscribers(path string) {
	ls.hub.dropSubscribers(path)
}
