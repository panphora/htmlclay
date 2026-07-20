package server

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/panphora/htmlclay/htmlutil"
	"github.com/panphora/htmlclay/logging"
	"github.com/panphora/htmlclay/session"
	"github.com/panphora/htmlclay/versions"
)

const (
	// watchPoll is the poll cadence.
	watchPoll = 250 * time.Millisecond
	// watchQuiet is how long a candidate must hold still before it is published.
	// Any content change restarts the interval.
	watchQuiet = 500 * time.Millisecond
)

// Honest limit, and it is a limit rather than a guarantee: no finite quiet
// interval can prove a paused non-atomic writer has finished, and HasHTMLTag
// accepts `<html><body>partial`. What the watcher promises is best-effort
// stability with a documented paused-writer residual. External editors also
// ignore session.File's lock, which is only an in-process mutex, so a write
// landing between the final revalidation and the enqueue is an unavoidable
// residual race.

// statKey is the metadata fingerprint checked before and after each read. Size
// and modtime alone can miss a same-size write with a preserved timestamp, so
// file identity is included where the platform reports it.
type statKey struct {
	size    int64
	modTime time.Time
	ident   string
}

func statOf(info os.FileInfo) statKey {
	return statKey{size: info.Size(), modTime: info.ModTime(), ident: fileIdentity(info)}
}

type watchEntry struct {
	file *session.File
	refs int

	// Watcher-internal tracking, not a per-file record: the candidate awaiting a
	// quiet interval, and whether the file is currently missing.
	pendingHash string
	pendingData []byte
	pendingAt   time.Time
	absent      bool

	// removed is set under the watcher lock when the last subscriber leaves. tick
	// copies entry pointers outside that lock, so without this an unwatch could
	// land mid-check and the orphaned check would still record its hash into
	// lastStableObservation and publish into an empty hub. That change was then
	// suppressed forever, and the user never saw it.
	removed bool
}

// watcher polls currently-subscribed files only. It starts on the first
// subscriber and stops on the last: session.Manager never unregisters, so polling
// its whole table would grow without bound.
type watcher struct {
	mu      sync.Mutex
	entries map[string]*watchEntry
	running bool
	stopCh  chan struct{}

	hub    *hub
	logger *logging.Logger
	poll   time.Duration
	quiet  time.Duration

	wg sync.WaitGroup
}

func newWatcher(h *hub, logger *logging.Logger) *watcher {
	return &watcher{
		entries: make(map[string]*watchEntry),
		hub:     h,
		logger:  logger,
		poll:    watchPoll,
		quiet:   watchQuiet,
	}
}

func (wt *watcher) watch(f *session.File) {
	wt.mu.Lock()
	defer wt.mu.Unlock()

	if e, ok := wt.entries[f.AbsPath]; ok {
		e.refs++
		return
	}
	wt.entries[f.AbsPath] = &watchEntry{file: f, refs: 1}

	if !wt.running {
		wt.running = true
		wt.stopCh = make(chan struct{})
		stop := wt.stopCh
		wt.wg.Add(1)
		go wt.loop(stop)
	}
}

func (wt *watcher) unwatch(f *session.File) {
	wt.mu.Lock()
	e, ok := wt.entries[f.AbsPath]
	if !ok {
		wt.mu.Unlock()
		return
	}
	e.refs--
	if e.refs > 0 {
		wt.mu.Unlock()
		return
	}
	e.removed = true
	delete(wt.entries, f.AbsPath)

	var stop chan struct{}
	if len(wt.entries) == 0 && wt.running {
		wt.running = false
		stop = wt.stopCh
		wt.stopCh = nil
	}
	wt.mu.Unlock()

	if stop != nil {
		close(stop)
	}
}

func (wt *watcher) shutdown() {
	wt.mu.Lock()
	var stop chan struct{}
	if wt.running {
		wt.running = false
		stop = wt.stopCh
		wt.stopCh = nil
	}
	for _, e := range wt.entries {
		e.removed = true
	}
	wt.entries = make(map[string]*watchEntry)
	wt.mu.Unlock()

	if stop != nil {
		close(stop)
	}
	wt.wg.Wait()
}

func (wt *watcher) loop(stop chan struct{}) {
	defer wt.wg.Done()
	ticker := time.NewTicker(wt.poll)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			wt.tick()
		}
	}
}

func (wt *watcher) tick() {
	wt.mu.Lock()
	entries := make([]*watchEntry, 0, len(wt.entries))
	for _, e := range wt.entries {
		entries = append(entries, e)
	}
	wt.mu.Unlock()

	for _, e := range entries {
		wt.check(e)
	}
}

// check runs one poll for one file. Every state field it touches belongs to the
// watcher; the two per-file records are read and written only under the file
// lock, at the very end.
func (wt *watcher) check(e *watchEntry) {
	if wt.isRemoved(e) {
		return
	}
	path := e.file.AbsPath

	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() {
		// A vanished file is not a change event. Record the absence and wait. If
		// it reappears with different content, that is one change event. This also
		// covers the brief gap during an atomic replacement, which is why deletion
		// must not fire on its own.
		e.absent = true
		e.pendingHash = ""
		e.pendingData = nil
		return
	}

	// Reread the pending candidate even when stat metadata is unchanged, and check
	// metadata before and after each read.
	data, err := os.ReadFile(path)
	if err != nil {
		e.pendingHash = ""
		e.pendingData = nil
		return
	}
	after, err := os.Lstat(path)
	if err != nil || statOf(before) != statOf(after) {
		e.pendingHash = ""
		e.pendingData = nil
		return
	}

	e.absent = false
	hash := versions.Hash(data)

	if e.pendingHash != hash {
		e.pendingHash = hash
		e.pendingData = data
		e.pendingAt = time.Now()
		return
	}
	if time.Since(e.pendingAt) < wt.quiet {
		return
	}

	wt.publish(e, hash, data)
}

func (wt *watcher) isRemoved(e *watchEntry) bool {
	wt.mu.Lock()
	defer wt.mu.Unlock()
	return e.removed
}

// publish reacquires the file lock, revalidates that the candidate is still the
// current disk content, compares against lastStableObservation, allocates the
// sequence from the shared counter and enqueues, all inside one critical section.
//
// The watcher lock is held across the removal check, the record and the enqueue,
// so unwatch cannot slip in between them. Lock order is file lock, then watcher
// lock, then hub lock; watch and unwatch take only the watcher lock, so there is
// no cycle.
func (wt *watcher) publish(e *watchEntry, hash string, data []byte) {
	f := e.file

	f.Lock()
	defer f.Unlock()

	wt.mu.Lock()
	defer wt.mu.Unlock()

	e.pendingHash = ""
	e.pendingData = nil

	// An entry whose last subscriber left must not record or publish: it would
	// advance lastStableObservation for a change nobody received, and that change
	// would then be suppressed forever on reconnect.
	if e.removed {
		return
	}

	// Suppression is by hash and stays valid until disk content diverges, rather
	// than expiring on a timer. Identical bytes are not a meaningful external
	// change, so there is no window in which the browser's own write resurfaces as
	// foreign.
	if f.LastStableObservation() == hash {
		return
	}

	fresh, err := os.ReadFile(f.AbsPath)
	if err != nil || versions.Hash(fresh) != hash {
		return
	}

	f.RecordStableObservation(hash)

	msg := fmt.Sprintf("%s changed on disk outside this tab", f.Name)
	wt.hub.publishExternalChange(f.AbsPath, msg, string(htmlutil.StripToken(data)))
	wt.logger.Printf("External change detected in %s", f.RelPath)
}
