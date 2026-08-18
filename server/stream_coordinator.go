package server

import (
	"encoding/json"
	"sync"

	"github.com/panphora/htmlclay/session"
)

// streamCoordinator owns subscriber lifecycle across the hub and the watcher, so
// hub membership and the watcher reference move together and an eviction drops the
// watcher reference before it forgets the hub member. Its lock is acquired before
// the watcher and hub locks; publish paths take the hub lock without it. The
// watcher and hub never call back into the coordinator while holding their own
// locks, so there is no cycle. Lock order overall: session.File, coordinator,
// watcher, hub.
type streamCoordinator struct {
	mu      sync.Mutex
	hub     *hub
	watcher *watcher
}

func newStreamCoordinator(h *hub, wt *watcher) *streamCoordinator {
	return &streamCoordinator{hub: h, watcher: wt}
}

// add registers the subscriber, observes the current incarnation, and raises the
// watcher reference in one critical section, returning the resume baseline, the
// replay slice the writer sends first, and whether replay leaves a gap. Caller
// holds f.Lock.
func (co *streamCoordinator) add(sub *subscriber, f *session.File) (int64, [][]byte, bool) {
	co.mu.Lock()
	defer co.mu.Unlock()
	baseline, replay, resync := co.hub.add(sub)
	co.watcher.watch(f)
	return baseline, replay, resync
}

// lease raises a watch reference with no hub subscription behind it, so a wire
// handler attached to a file keeps that file polled while no tab is open. It goes
// through the coordinator rather than straight to the watcher so every watch
// reference in the process is taken under one lock, and it is refcounted by the
// same watch/unwatch pair, so a handler reconnecting before the previous stream
// has finished tearing down never drops the entry.
//
// Unlike add, it takes no file lock and needs none: it touches no per-file record,
// and holding nothing above the coordinator lock cannot invert the order.
func (co *streamCoordinator) lease(f *session.File) {
	co.mu.Lock()
	defer co.mu.Unlock()
	co.watcher.watch(f)
}

// unlease drops a lease raised by lease.
func (co *streamCoordinator) unlease(f *session.File) {
	co.mu.Lock()
	defer co.mu.Unlock()
	co.watcher.unwatch(f)
}

// pokeWatcher forwards a handler's "I finished writing" hint to the file's watch
// entry, and does nothing for a file nobody is watching. Through the coordinator
// for the same reason lease is: every touch of the watcher goes under one lock,
// in the documented order.
func (co *streamCoordinator) pokeWatcher(f *session.File) {
	co.mu.Lock()
	defer co.mu.Unlock()
	co.watcher.poke(f.AbsPath)
}

// remove drops the watcher reference first, then the hub membership, then stops
// the writer. It is idempotent for one subscriber, so an eviction and the
// handler's own defer cannot double-count the watcher reference.
func (co *streamCoordinator) remove(sub *subscriber, f *session.File) {
	co.mu.Lock()
	defer co.mu.Unlock()
	if sub.removed {
		return
	}
	sub.removed = true
	co.watcher.unwatch(f)
	co.hub.remove(sub)
	sub.stop()
}

// evict removes, watcher-first, every subscriber a publish could not deliver to.
// The frame was retained before the failed offer, so each reconnect recovers it.
func (co *streamCoordinator) evict(f *session.File, evicted []*subscriber) {
	for _, sub := range evicted {
		co.remove(sub, f)
	}
}

func (co *streamCoordinator) relay(f *session.File, html, sender string, identityMap json.RawMessage) {
	co.evict(f, co.hub.relay(f.AbsPath, html, sender, identityMap))
}

func (co *streamCoordinator) broadcastSaved(f *session.File, html, sender string) {
	co.evict(f, co.hub.broadcastSaved(f.AbsPath, html, sender))
}

func (co *streamCoordinator) notifyWarning(f *session.File, msg string) {
	co.evict(f, co.hub.notifyWarning(f.AbsPath, msg))
}

// publishExternalChange retains and delivers a watcher-detected change and reports
// whether it reached an audience, so the watcher advances suppression only on a
// durable receipt (publish before record).
//
// The receipt is delivery, not the watch. Those used to be the same thing, because
// a live-sync subscription was the only thing that raised a watch reference. A wire
// handler's lease makes a file watched with no subscribers at all, and a watch-based
// receipt there would record the hash of a change nobody received, leaving it
// suppressed forever once the retained frame aged out. Caller holds f.Lock.
func (co *streamCoordinator) publishExternalChange(f *session.File, msg, html string) bool {
	delivered, evicted := co.hub.publishExternalChange(f.AbsPath, msg, html)
	co.evict(f, evicted)
	return delivered
}

// acceptServerReplacement re-anchors the incarnation after a server-authorized
// atomic write without rolling the generation. Caller holds f.Lock.
func (co *streamCoordinator) acceptServerReplacement(f *session.File) {
	co.hub.acceptServerReplacement(f.AbsPath)
}

// markAbsent clears an incarnation's buffers when the watcher sees the file gone.
func (co *streamCoordinator) markAbsent(path string) {
	co.hub.markAbsent(path)
}
