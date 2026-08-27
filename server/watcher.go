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
	// watchEmptyQuiet is the interval an EMPTY candidate must hold still for. It is
	// much longer than watchQuiet because zero bytes are more often an artifact than
	// a change: os.WriteFile truncates, then writes, and a writer descheduled
	// between the two leaves a zero-byte file holding perfectly still, which at
	// watchQuiet is indistinguishable from settled content. Publishing that blanks
	// the reader's tab. A file that is genuinely empty stays empty, so the
	// discriminator is time rather than content, and an empty file still publishes,
	// just later.
	watchEmptyQuiet = 3 * time.Second
)

// Honest limit, and it is a limit rather than a guarantee: no finite quiet
// interval can prove a paused non-atomic writer has finished, and HasHTMLTag
// accepts `<html><body>partial`. What the watcher promises is best-effort
// stability with a documented paused-writer residual. The one case held to a
// different standard is a paused writer's truncate, whose zero bytes must hold
// still for watchEmptyQuiet rather than watchQuiet. External editors also
// ignore session.File's lock, which is only an in-process mutex, so a write
// landing between the final revalidation and the enqueue is an unavoidable
// residual race.

// statKey is the metadata fingerprint checked before and after each read, and
// the one compared against the previous read to decide whether to read at all.
// Size and modtime alone can miss a same-size write with a preserved timestamp,
// so file identity is included where the platform reports it.
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
	// quiet interval, whether the file is currently missing, and the fingerprint
	// of the last read that revalidated cleanly.
	pendingHash string
	pendingData []byte
	pendingAt   time.Time
	absent      bool
	lastStat    statKey
	haveStat    bool

	// removed is set under the watcher lock when the last subscriber leaves. tick
	// copies entry pointers outside that lock, so without this an unwatch could
	// land mid-check and the orphaned check would still record its hash into
	// lastStableObservation and publish into an empty hub. That change was then
	// suppressed forever, and the user never saw it.
	//
	// rearm is set under the same lock when a subscriber joins an entry that
	// already existed, and consumed by the next poll.
	//
	// poke is set the same way when a wire handler reports it finished writing
	// this file. Every other field here belongs to the poll goroutine alone;
	// these three are the only ones another goroutine writes, which is why they
	// travel together through entryState.
	removed bool
	rearm   bool
	poke    bool
}

// forget drops the candidate and the read fingerprint together, so a poll that
// could not get a coherent look at the file reads it unconditionally next time
// rather than trusting metadata it never validated. Called on the poll goroutine.
func (e *watchEntry) forget() {
	e.pendingHash = ""
	e.pendingData = nil
	e.haveStat = false
}

// watcher polls currently-subscribed files only. It starts on the first
// subscriber and stops on the last: session.Manager never unregisters, so polling
// its whole table would grow without bound.
type watcher struct {
	mu      sync.Mutex
	entries map[string]*watchEntry
	running bool
	closed  bool
	stopCh  chan struct{}

	coord  *streamCoordinator
	logger *logging.Logger
	poll   time.Duration
	quiet  time.Duration
	// emptyQuiet is the quiet interval for a zero-byte candidate. See watchEmptyQuiet.
	emptyQuiet time.Duration

	// versions is the backup store. The watcher writes to it and never resolves an
	// identity in it: see publishConfirmed.
	versions *versions.Store

	wg sync.WaitGroup
}

func newWatcher(store *versions.Store, logger *logging.Logger) *watcher {
	return &watcher{
		entries:    make(map[string]*watchEntry),
		logger:     logger,
		poll:       watchPoll,
		quiet:      watchQuiet,
		emptyQuiet: watchEmptyQuiet,
		versions:   store,
	}
}

func (wt *watcher) watch(f *session.File) {
	wt.mu.Lock()
	defer wt.mu.Unlock()

	// Shutdown is terminal. The hub refuses new subscribers once closed, but the
	// coordinator still calls watch, and listeners keep accepting until each
	// site's HTTP server drains. Without this a late stream would start a fresh
	// polling goroutine after shutdown returned, racing its own wg.Wait.
	if wt.closed {
		return
	}

	if e, ok := wt.entries[f.AbsPath]; ok {
		e.refs++
		// An entry that already existed may have armed its read fingerprint while
		// nobody was subscribed, which a wire handler's lease makes ordinary, and a
		// change published to nobody is never recorded. Re-reading for the arriving
		// subscriber is what turns that unrecorded change into one it hears about.
		// An entry created below starts unarmed and reads on its first poll anyway.
		e.rearm = true
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

// poke asks the next poll to stop waiting out this file's quiet interval. A wire
// handler that reports it finished has finished writing, and the page's spinner
// ends on that same frame, so without this the text arrives a poll plus a quiet
// interval later.
//
// It is a hint and never an authorization. The candidate still faces two reads a
// poll apart (see check), then the re-read and hash revalidation in
// publishConfirmed, so a poke on a half-written file publishes nothing.
//
// Its validity is one poll. check consumes it whether or not it had a candidate
// to spend it on, exactly as it consumes rearm, because a poke that outlived its
// request would shorten an unrelated later write's quiet interval, which is the
// one thing that interval exists to prevent.
func (wt *watcher) poke(path string) {
	wt.mu.Lock()
	defer wt.mu.Unlock()
	if e, ok := wt.entries[path]; ok {
		e.poke = true
	}
}

// emptyPending reports whether this path currently has a zero-byte candidate
// waiting out watchEmptyQuiet. The save path asks before it overwrites: those
// zero bytes are far more often a writer paused between its truncate and its
// write than a finished document, and writing over them unlinks the inode that
// writer still holds open.
//
// Deliberately keyed on a watcher that is actually watching. If nothing is, no
// one will ever publish the candidate, and a guard that could not lift would wedge
// the file against every future save.
func (wt *watcher) emptyPending(path string) bool {
	wt.mu.Lock()
	defer wt.mu.Unlock()
	e, ok := wt.entries[path]
	return ok && e.pendingHash != "" && len(e.pendingData) == 0
}

func (wt *watcher) shutdown() {
	wt.mu.Lock()
	wt.closed = true
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
	removed, rearm, poke := wt.entryState(e)
	if removed {
		return
	}
	if rearm {
		e.haveStat = false
	}
	path := e.file.AbsPath

	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() {
		// A vanished file is not a change event. Record the absence and wait. If
		// it reappears with different content, that is one change event. This also
		// covers the brief gap during an atomic replacement, which is why deletion
		// must not fire on its own. On the transition to absent, clear the
		// incarnation's buffers so nothing from the old file replays past its life.
		if !e.absent {
			e.absent = true
			wt.coord.markAbsent(path)
		}
		e.forget()
		return
	}

	// With no candidate pending, an unchanged fingerprint means an unchanged file
	// and the read is skipped. This is what makes a wire handler's watch lease
	// affordable: the lease keeps a file polled with no tab open, and reading a
	// whole document four times a second for as long as an agent is attached is not
	// something to do in the background.
	//
	// The residual is a write that lands with the same size, modtime and file
	// identity as the last one read, which takes a filesystem whose timestamp
	// resolution is coarser than the gap between two writes. The next write visible
	// in metadata surfaces both.
	//
	// A pending candidate is still always re-read, unconditionally, because the
	// quiet interval is a claim about content rather than about metadata.
	fingerprint := statOf(before)
	if e.pendingHash == "" && e.haveStat && fingerprint == e.lastStat {
		return
	}

	// Metadata is checked before and after each read.
	data, err := os.ReadFile(path)
	if err != nil {
		e.forget()
		return
	}
	after, err := os.Lstat(path)
	if err != nil || fingerprint != statOf(after) {
		e.forget()
		return
	}

	e.lastStat = fingerprint
	e.haveStat = true
	e.absent = false
	hash := versions.Hash(data)

	if e.pendingHash != hash {
		e.pendingHash = hash
		e.pendingData = data
		e.pendingAt = time.Now()
		// A poke says the write is finished, so this candidate does not have to
		// wait the interval out. It does not get to skip the interval entirely:
		// a candidate this poll saw for the first time has been read exactly
		// once, and publishing on a single read is what the interval exists to
		// prevent. Leaving half a poll on the clock buys the confirming read and
		// nothing else. Half rather than a whole poll so that ticker jitter and
		// the read itself cannot push it into a third poll; never forward, so a
		// watcher polling slower than its own interval is unaffected. A file
		// still being written hashes differently next poll and starts over.
		// Never for an empty candidate: that one is held to emptyQuiet precisely
		// because a poke cannot speak for the writer it guards against, and
		// backdating here would shorten the hold before the empty branch below
		// ever gets to refuse the poke.
		if poke && len(data) != 0 {
			if wait := wt.quiet - wt.poll/2; wait > 0 {
				e.pendingAt = e.pendingAt.Add(-wait)
			}
		}
		return
	}
	// An empty candidate is held to watchEmptyQuiet instead, and a poke does not
	// shorten it. A poke says our own writer finished; the writer this guards
	// against is the one we cannot see, paused between its truncate and its write.
	// A real write lands within the longer interval and replaces this candidate, so
	// a truncate never reaches publish while a genuinely empty file still does.
	quiet := wt.quiet
	if len(data) == 0 {
		quiet = wt.emptyQuiet
		poke = false
	}
	if !poke && time.Since(e.pendingAt) < quiet {
		return
	}

	wt.publish(e, hash, data)
}

// entryState reads the three fields another goroutine may write, in one
// acquisition, and consumes the rearm and poke requests.
func (wt *watcher) entryState(e *watchEntry) (removed, rearm, poke bool) {
	wt.mu.Lock()
	defer wt.mu.Unlock()
	removed, rearm, poke = e.removed, e.rearm, e.poke
	e.rearm = false
	e.poke = false
	return removed, rearm, poke
}

// publish versions and broadcasts a confirmed change, then prunes.
//
// Pruning takes the store lock, and the documented order is file lock before
// store lock, so it runs only once publishConfirmed has released f.Lock. Inside
// the file lock it would stall that file's saves behind a directory sweep.
func (wt *watcher) publish(e *watchEntry, hash string, data []byte) {
	if key := wt.publishConfirmed(e, hash, data); key != "" {
		wt.versions.MaybePrune(key, e.file.AbsPath)
	}
}

// publishConfirmed reacquires the file lock, revalidates that the candidate is
// still the current disk content, versions it, and asks the coordinator to retain
// and deliver the change. It returns the history key to prune, or "" for nothing.
//
// It advances lastStableObservation only after the coordinator confirms the change
// reached an audience: publish before record, so a change that could not be
// delivered is never marked as reported.
//
// What rediscovers it is a subscriber arriving, not the next poll. The read gate in
// check would otherwise skip the file as metadata-unchanged, and clearing the gate
// here instead would put the watcher into a permanent 4-reads-a-second loop for
// exactly the case the lease makes ordinary: an agent editing a file with no tab
// open. So the entry keeps the change unrecorded and quiet, and watch rearms the
// read when someone joins.
//
// The backup runs BEFORE the broadcast and regardless of its outcome. An external
// edit is the one write to this file that no other path versions, and whether a
// browser happened to be listening has nothing to do with whether the bytes are
// worth keeping. It stores the raw disk bytes, matching the first-open snapshot, so
// it dedups against a save's pre-write backup instead of doubling every file.
//
// The key is READ and never resolved. Resolving one here would let the watcher
// mint a durable identity for a file nobody has opened, which is the rule that
// keeps identity a decision of the handlers; a file with no key yet simply is not
// versioned, and a wire handler's lease is what gives an unopened file one (see
// leaseForHandler). A backup failure logs and never suppresses the broadcast.
//
// The watcher lock is released before the coordinator call, so the coordinator can
// take the watcher lock again to evict watcher-first without a self-deadlock. Lock
// order is file lock, then coordinator, then watcher, then hub.
func (wt *watcher) publishConfirmed(e *watchEntry, hash string, data []byte) string {
	f := e.file

	f.Lock()
	defer f.Unlock()

	wt.mu.Lock()
	removed := e.removed
	e.pendingHash = ""
	e.pendingData = nil
	wt.mu.Unlock()

	// An entry whose last subscriber left must not record or publish: it would
	// advance lastStableObservation for a change nobody received, and that change
	// would then be suppressed forever on reconnect.
	if removed {
		return ""
	}

	// Suppression is by hash and stays valid until disk content diverges, rather
	// than expiring on a timer. Identical bytes are not a meaningful external
	// change, so there is no window in which the browser's own write resurfaces as
	// foreign.
	if f.LastStableObservation() == hash {
		return ""
	}

	fresh, err := os.ReadFile(f.AbsPath)
	if err != nil || versions.Hash(fresh) != hash {
		// The candidate could not be confirmed, so the fingerprint that came with it
		// is worthless: a transient read failure leaves size, modtime and identity
		// untouched, and without this the gate would skip the file on every later
		// poll while the subscriber holds content the server knows is stale.
		e.haveStat = false
		return ""
	}

	key := f.HistoryKey()
	if key != "" {
		if _, bErr := wt.versions.Backup(key, f.AbsPath, data); bErr != nil {
			wt.logger.Printf("Backup of external change to %s failed: %v", f.RelPath, bErr)
		}
	}

	msg := fmt.Sprintf("%s changed on disk outside this tab", f.Name)
	if wt.coord.publishExternalChange(f, msg, string(htmlutil.StripToken(data))) {
		f.RecordStableObservation(hash)
		wt.logger.Printf("External change detected in %s", f.RelPath)
	}
	return key
}
