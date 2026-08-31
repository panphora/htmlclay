package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/panphora/htmlclay/internal/htmlutil"
	"github.com/panphora/htmlclay/internal/platform"
	"github.com/panphora/htmlclay/internal/session"
	"github.com/panphora/htmlclay/internal/versions"
)

const (
	// laneLive carries pre-strip peer snapshots and notifications to edit-mode
	// tabs. laneSaved carries only post-strip on-disk HTML, the same bytes any
	// viewer could GET.
	laneLive  = "live"
	laneSaved = "saved"

	// maxLiveSyncSize matches the clients' 12MB save ceiling.
	maxLiveSyncSize = 12 * 1024 * 1024
	// maxSenderLen bounds the client-supplied sender id.
	maxSenderLen = 128

	// subQueueSize bounds each subscriber's queue. A subscriber that cannot keep
	// up is evicted rather than allowed to grow memory without limit.
	subQueueSize = 32
	// sseWriteDeadline is applied fresh before every frame, so a subscriber stuck
	// inside Write fails on its own instead of pinning a goroutine forever. It
	// stays strictly under ShutdownBudget: at 10s against a 3s graceful shutdown a
	// blocked write necessarily outlived the budget, so shutdown always timed out
	// into a forced close.
	sseWriteDeadline = 2 * time.Second
	// keepaliveInterval keeps intermediaries and idle-timeout logic from closing
	// an otherwise silent stream.
	keepaliveInterval = 25 * time.Second

	// Replay is a bounded, incarnation-aware cache. A frame lives at most
	// replayFrameTTL; each (path incarnation, lane) bucket holds at most
	// perIncarnationMaxFrames / perIncarnationMaxBytes; the whole cache holds at
	// most globalMaxReplayFrames / globalMaxReplayBytes, maxInactiveIncarnations
	// idle incarnations, and maxDisconnectedCursors disconnected resume cursors.
	// A frame larger than perIncarnationMaxBytes is delivered live but never
	// retained: there is no oversize exception.
	replayFrameTTL          = 5 * time.Minute
	cursorTTL               = 5 * time.Minute
	perIncarnationMaxFrames = 64
	perIncarnationMaxBytes  = 16 * 1024 * 1024
	globalMaxReplayFrames   = 512
	globalMaxReplayBytes    = 64 * 1024 * 1024
	maxInactiveIncarnations = 256
	maxDisconnectedCursors  = 1024
	janitorInterval         = 30 * time.Second

	// seqPersistWindow is how far ahead of the live sequence the high-water mark is
	// persisted, so the counter survives a restart without an fsync per event.
	seqPersistWindow = 10000

	// maxResumeIDLen bounds the client-supplied resume id.
	maxResumeIDLen = 128
)

// ShutdownBudget is how long graceful shutdown may take. cmd/htmlclay/main.go uses it for its
// shutdown context, and sseWriteDeadline is defined strictly under it, so the two
// cannot drift apart into a shutdown that always force-closes.
const ShutdownBudget = 3 * time.Second

// Documented limit: browsers cap HTTP/1.1 connections at six per origin, and an
// SSE stream holds one for the life of the page. Once six htmlclay tabs are open
// on one origin, a seventh request (including a save) queues behind them. This is
// a real constraint of the transport, not something the server can raise.
const maxUsefulTabs = 6

type subscriber struct {
	key  string
	lane string
	ch   chan []byte
	done chan struct{}
	once sync.Once

	// lastEventID is the client's Last-Event-ID, zero when it is a fresh
	// connection with nothing to catch up on.
	lastEventID int64

	// resumeID is the client's per-start resume-id query parameter. Native
	// EventSource reuses the same URL, and thus the same resumeID, on reconnect,
	// so a stream that died before parsing any SSE id still resumes from the
	// server-recorded cursor.
	resumeID string

	// removed is set once by the coordinator so remove and eviction are
	// idempotent for one subscriber. Guarded by streamCoordinator.mu.
	removed bool
}

func (sub *subscriber) stop() {
	sub.once.Do(func() { close(sub.done) })
}

// retainedFrame is one frame held for reconnect replay.
type retainedFrame struct {
	seq         int64
	frame       []byte
	publishedAt time.Time
}

// laneBucket holds one lane's retained frames for one incarnation, plus the
// high-water mark of what it could not keep.
type laneBucket struct {
	frames []retainedFrame
	bytes  int

	// droppedThrough is the highest seq this bucket dropped or declined to
	// retain. A subscriber resuming below it missed something replay cannot
	// return, which is what puts resync on its cursor frame.
	//
	// It is the only recovery marker. A separate needsResync flag alongside it
	// said nothing this field does not already say, and only one of the two drop
	// paths ever set it, so the two disagreed on the cap-eviction case.
	droppedThrough int64
}

// incarnation is one generation of the file at a path. A new file at the same
// path is a new incarnation and never inherits the old one's retained frames.
//
// The anchor answers "is this still the same file", and how it does that differs
// by operating system in a way the hub deliberately knows nothing about: see
// platform.Anchor. What matters here is that an anchor is owned. Whoever replaces
// one closes the one it displaced, and the reaper closes the anchors of the
// incarnations it drops.
type incarnation struct {
	generation int64
	anchor     *platform.Anchor
	lastTouch  time.Time
	live       *laneBucket
	saved      *laneBucket
}

func (inc *incarnation) bucket(lane string) *laneBucket {
	if lane == laneSaved {
		return inc.saved
	}
	return inc.live
}

// resumeCursor is the server-recorded baseline for one (path incarnation, lane,
// resume id), so a stream that died before parsing its first SSE id can resume.
type resumeCursor struct {
	path         string
	generation   int64
	lane         string
	resumeID     string
	baseline     int64
	disconnectAt time.Time // zero while an active stream owns it
	touched      time.Time
}

// hub owns every SSE subscriber and the single broadcast sequence counter shared
// by the relay leg (B3) and the watcher (B4). There is exactly one counter. It
// also owns incarnation-aware, bounded replay and the resume cursors.
type hub struct {
	mu      sync.Mutex
	subs    map[string]map[*subscriber]struct{}
	seq     int64
	closing chan struct{}
	closed  bool

	// incs is one incarnation per active path; cursors is the disconnected/active
	// resume cursors keyed by (path, generation, lane, resume id). replayFrames
	// and replayBytes are the global running totals used to enforce the global
	// caps in O(1).
	incs         map[string]*incarnation
	cursors      map[string]*resumeCursor
	replayFrames int
	replayBytes  int

	// seqPath persists the sequence high-water mark. Seeding from the wall clock
	// alone meant a backward clock change plus a restart put every new sequence
	// below what an open client had retained, and both clients then discarded every
	// update until real time caught up. Empty disables persistence.
	seqPath   string
	persisted int64

	// now is an injectable clock; nil means time.Now. It drives frame TTL and
	// cursor TTL so a test can advance time deterministically.
	now func() time.Time

	janitorStop chan struct{}
}

func newHub(seqPath string) *hub {
	seq := time.Now().UnixMilli()
	persisted := int64(0)
	if hw, ok := readSeqHighWater(seqPath); ok {
		persisted = hw
		if hw >= seq {
			seq = hw + 1
		}
	}
	return &hub{
		subs:    make(map[string]map[*subscriber]struct{}),
		incs:    make(map[string]*incarnation),
		cursors: make(map[string]*resumeCursor),
		// Seed from wall-clock milliseconds, as the parity implementation does, but
		// never below the persisted high-water mark. A counter restarting below what
		// the client retained is rejected and the stream silently stops updating.
		seq:       seq,
		persisted: persisted,
		closing:   make(chan struct{}),
		seqPath:   seqPath,
	}
}

func (h *hub) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

// nextSeq allocates the next sequence. Caller must hold h.mu, so allocation and
// enqueue are one ordered operation.
func (h *hub) nextSeq() int64 {
	now := time.Now().UnixMilli()
	if now > h.seq {
		h.seq = now
	} else {
		h.seq++
	}
	// Persist a window ahead rather than every allocation, so the cost is one small
	// write per seqPersistWindow events while a restart still resumes above
	// anything already handed out.
	if h.seq >= h.persisted {
		h.persisted = h.seq + seqPersistWindow
		writeSeqHighWater(h.seqPath, h.persisted)
	}
	return h.seq
}

func readSeqHighWater(path string) (int64, bool) {
	if path == "" {
		return 0, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}

func writeSeqHighWater(path string, v int64) {
	if path == "" {
		return
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".htmlclay-seq-*")
	if err != nil {
		return
	}
	tmpPath := tmp.Name()
	if _, err := tmp.WriteString(strconv.FormatInt(v, 10)); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
	}
}

// startJanitor runs periodic expiry so idle replay, cursors, and incarnations do
// not survive their bounds even with no traffic. Idempotent; the Server starts it.
func (h *hub) startJanitor() {
	h.mu.Lock()
	if h.closed || h.janitorStop != nil {
		h.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	h.janitorStop = stop
	h.mu.Unlock()
	go func() {
		t := time.NewTicker(janitorInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				h.expire()
			}
		}
	}()
}

// expire runs one sweep of the bounded caches. Also invoked from add and retain.
func (h *hub) expire() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.expireLocked()
}

// add registers a subscriber, records or looks up its resume cursor after
// observing the current file incarnation, and returns the resume baseline (the
// seq the cursor frame should carry), the frames to replay, and whether the gap
// below that baseline is unrecoverable. It never pushes replay into the bounded
// live queue; the writer sends the returned slice first.
func (h *hub) add(sub *subscriber) (baseline int64, replay [][]byte, resync bool) {
	probe := probeIdentity(sub.key)

	h.mu.Lock()
	defer h.mu.Unlock()
	h.expireLocked()
	if h.closed {
		probe.Close()
		sub.stop()
		return h.seq, nil, false
	}
	set, ok := h.subs[sub.key]
	if !ok {
		set = make(map[*subscriber]struct{})
		h.subs[sub.key] = set
	}
	set[sub] = struct{}{}

	// The generation this path had BEFORE identity is observed. Zero means the hub
	// was holding nothing for it, which after a reconnect means its incarnation was
	// reaped: the frames aged out, the cursor expired, and reapIncarnationsLocked
	// then deleted the whole record including the drop high-water. A rolled
	// generation means the file itself was replaced and its buckets were cleared.
	// Either way the server can no longer prove what this subscriber missed, and
	// the marker it would have consulted is gone with the record.
	prevGeneration := int64(0)
	if prev, ok := h.incs[sub.key]; ok {
		prevGeneration = prev.generation
	}

	// Observe identity BEFORE selecting replay: a same-path B is recognized and
	// rolls the generation before any A frame can be returned.
	inc := h.observeIdentityLocked(sub.key, probe)
	from, resumed := h.resumePointLocked(inc, sub)

	bucket := inc.bucket(sub.lane)
	for _, rf := range bucket.frames {
		if rf.seq > from {
			replay = append(replay, rf.frame)
		}
	}

	// Only a subscriber claiming a prior position can be behind; a first connection
	// resumes at the current sequence with nothing to recover. It is behind if the
	// record it is resuming into is not the one it left, or if frames above its
	// resume point were dropped.
	sameIncarnation := prevGeneration != 0 && prevGeneration == inc.generation
	return from, replay, resumed && (!sameIncarnation || from < bucket.droppedThrough)
}

// resumePointLocked decides where this subscriber resumes and keeps its cursor
// active. An explicit Last-Event-ID for the current incarnation wins unless it is
// a future id above the high-water; otherwise the saved cursor baseline is used;
// a first connection records the current sequence as its baseline.
//
// resumed distinguishes the first two from the third: it says the subscriber
// claims a position it held earlier, and so is the only kind of subscriber that
// can be behind.
func (h *hub) resumePointLocked(inc *incarnation, sub *subscriber) (from int64, resumed bool) {
	if sub.lastEventID > 0 && sub.lastEventID <= h.seq {
		if sub.resumeID != "" {
			h.activateCursorLocked(inc, sub, sub.lastEventID)
		}
		return sub.lastEventID, true
	}
	if sub.resumeID == "" {
		return h.seq, false
	}
	ck := cursorKey(sub.key, inc.generation, sub.lane, sub.resumeID)
	if c, ok := h.cursors[ck]; ok && c.generation == inc.generation {
		c.disconnectAt = time.Time{}
		c.touched = h.clock()
		return c.baseline, true
	}
	h.cursors[ck] = &resumeCursor{
		path:       sub.key,
		generation: inc.generation,
		lane:       sub.lane,
		resumeID:   sub.resumeID,
		baseline:   h.seq,
		touched:    h.clock(),
	}
	return h.seq, false
}

// activateCursorLocked marks a Last-Event-ID reconnect's cursor active so a later
// header-less reconnect has a baseline to fall back to. An existing baseline is
// never lowered.
func (h *hub) activateCursorLocked(inc *incarnation, sub *subscriber, baseline int64) {
	ck := cursorKey(sub.key, inc.generation, sub.lane, sub.resumeID)
	c, ok := h.cursors[ck]
	if !ok {
		c = &resumeCursor{
			path:       sub.key,
			generation: inc.generation,
			lane:       sub.lane,
			resumeID:   sub.resumeID,
			baseline:   baseline,
		}
		h.cursors[ck] = c
	}
	c.disconnectAt = time.Time{}
	c.touched = h.clock()
}

func cursorKey(path string, gen int64, lane, resumeID string) string {
	return path + "\x00" + strconv.FormatInt(gen, 10) + "\x00" + lane + "\x00" + resumeID
}

// ensureIncarnationLocked returns the incarnation for path, creating a fresh
// generation-1 one without touching the disk if none exists.
func (h *hub) ensureIncarnationLocked(path string) *incarnation {
	inc, ok := h.incs[path]
	if !ok {
		inc = &incarnation{
			generation: 1,
			lastTouch:  h.clock(),
			live:       &laneBucket{},
			saved:      &laneBucket{},
		}
		h.incs[path] = inc
	}
	return inc
}

// probeIdentity reads a file's identity off the disk, returning nil when the path
// cannot be identified at all right now.
//
// Every caller does this BEFORE taking h.mu, because a stalled disk must not hold
// up every subscriber on every other path. That is safe only because all four
// probing entry points run under the per-file session.File lock, and exactly one
// session.File exists per absolute path, so probe and install cannot be split by
// another observation of the same file. Anything new that probes a path must hold
// that lock too: without it a stale probe can be installed over a fresher anchor,
// leaving the hub's idea of the file behind the disk's.
func probeIdentity(path string) *platform.Anchor {
	a, err := platform.NewAnchor(path)
	if err != nil {
		return nil
	}
	return a
}

// observeIdentityLocked compares probe against the incarnation's anchor and rolls
// the generation when the file is no longer provably the one we retained frames
// for, clearing the old buckets and cursors. It takes ownership of probe: it
// either installs it or closes it.
//
// A failed probe against an incarnation we HAD identified rolls, because "I could
// not read it" is not evidence that it did not change. That direction is chosen
// deliberately: a needless roll costs a tab its replay buffer, while a missed one
// paints a dead document's bytes into a page showing a live one. A failed probe
// against a path we never identified changes nothing, since there is no
// established identity for the retained frames to be wrong about, and rolling
// forever on a filesystem that cannot answer would break resume outright.
//
// A server-authorized atomic write does not reach here as a change:
// acceptServerReplacement re-anchors first, so the next observation either
// matches or adopts.
func (h *hub) observeIdentityLocked(path string, probe *platform.Anchor) *incarnation {
	inc := h.ensureIncarnationLocked(path)
	if inc.anchor.Same(probe) {
		probe.Close()
		return inc
	}
	if inc.anchor == nil {
		if probe != nil {
			inc.anchor = probe
			inc.lastTouch = h.clock()
		}
		return inc
	}
	h.clearBucketsLocked(inc)
	h.clearCursorsForPathLocked(path)
	inc.anchor.Close()
	inc.anchor = probe
	inc.generation++
	inc.lastTouch = h.clock()
	return inc
}

// acceptServerReplacement re-anchors an existing incarnation to the file that now
// sits at the path, without rolling the generation: save, id injection, clone
// fork and restore are all changes to the same logical file.
//
// A failed probe installs nil rather than keeping the old anchor. Keeping it
// would leave the incarnation naming the file we just replaced, so the next
// observation would find a mismatch and roll the generation on the tab's own
// save, throwing away the replay buffer of the tab that made it. Nil means the
// next observation adopts whatever is there, which is right: under f.Lock,
// immediately after our own successful write, "could not read it" is a Windows
// delete-pending window or a transient, not somebody else's replacement.
//
// The interest check runs first and on its own so a path nobody is streaming
// costs one uncontended mutex and no disk access at all. The incarnation can
// still be reaped between the two locks, which the second lookup handles.
// Caller holds f.Lock.
func (h *hub) acceptServerReplacement(path string) {
	h.mu.Lock()
	_, interested := h.incs[path]
	h.mu.Unlock()
	if !interested {
		return
	}
	probe := probeIdentity(path)

	h.mu.Lock()
	defer h.mu.Unlock()
	inc, ok := h.incs[path]
	if !ok {
		probe.Close()
		return
	}
	inc.anchor.Close()
	inc.anchor = probe
	inc.lastTouch = h.clock()
}

// markAbsent clears an incarnation's buffers when the watcher sees the file gone,
// without emitting a deletion event.
//
// The anchor deliberately survives, which on Unix means the deleted file's blocks
// are not reclaimed until the last tab disconnects. That is the price of the
// anti-recycling guarantee: dropping the anchor here would free the inode for the
// next file created, which is exactly the confusion it exists to prevent.
//
// A disappearance also rolls the generation, but only for a path nothing has ever
// been able to identify. That is the one case where identity cannot defend
// against delete-then-recreate, so vanishing is the only evidence we will ever
// get that a document ended. Everywhere else the anchor already covers it, and
// rolling here would be actively wrong: the watcher records absence during the
// brief gap of an atomic replacement, our own saves included, so an unconditional
// roll would throw away the replay buffer of the tab that just saved.
func (h *hub) markAbsent(path string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	inc, ok := h.incs[path]
	if !ok {
		return
	}
	h.clearBucketsLocked(inc)
	if inc.anchor == nil {
		h.clearCursorsForPathLocked(path)
		inc.generation++
	}
}

func (h *hub) clearBucketsLocked(inc *incarnation) {
	h.replayFrames -= len(inc.live.frames) + len(inc.saved.frames)
	h.replayBytes -= inc.live.bytes + inc.saved.bytes
	inc.live = &laneBucket{}
	inc.saved = &laneBucket{}
}

func (h *hub) clearCursorsForPathLocked(path string) {
	for k, c := range h.cursors {
		if c.path == path {
			delete(h.cursors, k)
		}
	}
}

// retainLocked records a frame in its incarnation-lane bucket, then enforces the
// per-incarnation and global caps. A frame too large to retain leaves a resync
// marker instead. Caller holds h.mu.
func (h *hub) retainLocked(path, lane string, seq int64, f []byte) {
	inc := h.ensureIncarnationLocked(path)
	inc.lastTouch = h.clock()
	b := inc.bucket(lane)
	if len(f) > perIncarnationMaxBytes {
		if seq > b.droppedThrough {
			b.droppedThrough = seq
		}
		return
	}
	b.frames = append(b.frames, retainedFrame{seq: seq, frame: f, publishedAt: h.clock()})
	b.bytes += len(f)
	h.replayFrames++
	h.replayBytes += len(f)
	for len(b.frames) > perIncarnationMaxFrames || b.bytes > perIncarnationMaxBytes {
		h.dropOldestFromBucket(b)
	}
	h.enforceGlobalCapsLocked()
}

func (h *hub) dropOldestFromBucket(b *laneBucket) {
	if len(b.frames) == 0 {
		return
	}
	f := b.frames[0]
	b.frames = b.frames[1:]
	b.bytes -= len(f.frame)
	h.replayFrames--
	h.replayBytes -= len(f.frame)
	if f.seq > b.droppedThrough {
		b.droppedThrough = f.seq
	}
}

func (h *hub) enforceGlobalCapsLocked() {
	for h.replayFrames > globalMaxReplayFrames || h.replayBytes > globalMaxReplayBytes {
		if !h.dropGlobalOldestLocked() {
			break
		}
	}
}

func (h *hub) dropGlobalOldestLocked() bool {
	var oldest *laneBucket
	var oldestAt time.Time
	for _, inc := range h.incs {
		for _, b := range []*laneBucket{inc.live, inc.saved} {
			if len(b.frames) == 0 {
				continue
			}
			at := b.frames[0].publishedAt
			if oldest == nil || at.Before(oldestAt) {
				oldest, oldestAt = b, at
			}
		}
	}
	if oldest == nil {
		return false
	}
	h.dropOldestFromBucket(oldest)
	return true
}

// expireLocked drops frames past their TTL, disconnected cursors past theirs,
// and reaps idle incarnations and surplus cursors. Caller holds h.mu.
func (h *hub) expireLocked() {
	now := h.clock()
	fcut := now.Add(-replayFrameTTL)
	for _, inc := range h.incs {
		for _, b := range []*laneBucket{inc.live, inc.saved} {
			for len(b.frames) > 0 && b.frames[0].publishedAt.Before(fcut) {
				h.dropOldestFromBucket(b)
			}
		}
	}
	ccut := now.Add(-cursorTTL)
	for k, c := range h.cursors {
		if !c.disconnectAt.IsZero() && c.disconnectAt.Before(ccut) {
			delete(h.cursors, k)
		}
	}
	h.reapIncarnationsLocked()
	h.capDisconnectedCursorsLocked()
}

func (h *hub) hasCursorLocked(path string) bool {
	for _, c := range h.cursors {
		if c.path == path {
			return true
		}
	}
	return false
}

// reapIncarnationsLocked closes and drops fully idle incarnations, then enforces
// the inactive-incarnation cap by dropping the least recently touched.
func (h *hub) reapIncarnationsLocked() {
	for path, inc := range h.incs {
		if len(h.subs[path]) > 0 {
			continue
		}
		if len(inc.live.frames) > 0 || len(inc.saved.frames) > 0 {
			continue
		}
		if h.hasCursorLocked(path) {
			continue
		}
		inc.anchor.Close()
		delete(h.incs, path)
	}

	type idle struct {
		path string
		inc  *incarnation
	}
	var inactive []idle
	for path, inc := range h.incs {
		if len(h.subs[path]) == 0 {
			inactive = append(inactive, idle{path, inc})
		}
	}
	for len(inactive) > maxInactiveIncarnations {
		oldest := 0
		for i := 1; i < len(inactive); i++ {
			if inactive[i].inc.lastTouch.Before(inactive[oldest].inc.lastTouch) {
				oldest = i
			}
		}
		victim := inactive[oldest]
		h.clearBucketsLocked(victim.inc)
		h.clearCursorsForPathLocked(victim.path)
		victim.inc.anchor.Close()
		delete(h.incs, victim.path)
		inactive[oldest] = inactive[len(inactive)-1]
		inactive = inactive[:len(inactive)-1]
	}
}

// capDisconnectedCursorsLocked drops the oldest disconnected cursors past the cap.
func (h *hub) capDisconnectedCursorsLocked() {
	type dc struct {
		key string
		at  time.Time
	}
	var disconnected []dc
	for k, c := range h.cursors {
		if !c.disconnectAt.IsZero() {
			disconnected = append(disconnected, dc{k, c.disconnectAt})
		}
	}
	for len(disconnected) > maxDisconnectedCursors {
		oldest := 0
		for i := 1; i < len(disconnected); i++ {
			if disconnected[i].at.Before(disconnected[oldest].at) {
				oldest = i
			}
		}
		delete(h.cursors, disconnected[oldest].key)
		disconnected[oldest] = disconnected[len(disconnected)-1]
		disconnected = disconnected[:len(disconnected)-1]
	}
}

// dropSubscribers stops every subscriber on key. Stopping closes each writer's
// done channel; the writers exit and run their handlers' deferred coordinator
// removal, so membership and watcher references unwind through the one
// existing teardown path.
func (h *hub) dropSubscribers(key string) {
	h.mu.Lock()
	subs := make([]*subscriber, 0, len(h.subs[key]))
	for sub := range h.subs[key] {
		subs = append(subs, sub)
	}
	h.mu.Unlock()
	for _, sub := range subs {
		sub.stop()
	}
}

// remove drops a subscriber and marks its resume cursors disconnected so they
// expire on the cursor TTL rather than immediately.
func (h *hub) remove(sub *subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set, ok := h.subs[sub.key]
	if !ok {
		return
	}
	if _, ok := set[sub]; !ok {
		return
	}
	delete(set, sub)
	if len(set) == 0 {
		delete(h.subs, sub.key)
	}
	if sub.resumeID != "" {
		now := h.clock()
		for _, c := range h.cursors {
			if c.path == sub.key && c.lane == sub.lane && c.resumeID == sub.resumeID && c.disconnectAt.IsZero() {
				c.disconnectAt = now
			}
		}
	}
}

func (h *hub) subscriberCount(key string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs[key])
}

// enqueue retains one pre-serialized frame for reconnect replay, then posts it to
// every subscriber on lane. A subscriber whose bounded queue is full is removed
// from the delivery set and unblocked, and returned so the coordinator can drop
// its watcher reference; the retained frame means its reconnect recovers the
// event rather than losing it.
//
// It reports how many subscribers actually took the frame, which is what the
// watcher's receipt is built on. Caller must hold h.mu.
func (h *hub) enqueue(key, lane string, seq int64, f []byte) (accepted int, evicted []*subscriber) {
	h.retainLocked(key, lane, seq, f)

	set := h.subs[key]
	for sub := range set {
		if sub.lane != lane {
			continue
		}
		if offer(sub, [][]byte{f}) {
			accepted++
		} else {
			evicted = append(evicted, sub)
		}
	}
	for _, sub := range evicted {
		delete(set, sub)
		sub.stop()
	}
	if len(set) == 0 {
		delete(h.subs, key)
	}
	return accepted, evicted
}

// offer posts every frame to sub, keeping insertion order. It reports false as
// soon as the bounded queue is full, which makes the subscriber a candidate for
// eviction.
func offer(sub *subscriber, frames [][]byte) bool {
	for _, f := range frames {
		select {
		case sub.ch <- f:
		default:
			return false
		}
	}
	return true
}

type livePayload struct {
	HTML        string          `json:"html"`
	Sender      string          `json:"sender"`
	Seq         int64           `json:"seq"`
	IdentityMap json.RawMessage `json:"identityMap,omitempty"`
	// Spec §6: the stamp of what this host stored for the bytes the sending tab just
	// saved. Carried unread, and omitted entirely when the sender did not send one, so
	// the frame a pre-spec client receives is byte-identical to what it received before.
	//
	// It rides on a snapshot and can never travel alone: a stamp with no content would
	// tell a receiving tab it is in step with disk without giving it the bytes to be in
	// step with, and that tab's next save would then pass If-Match and overwrite a save
	// it had never received.
	Etag string `json:"etag,omitempty"`
}

type notifyPayload struct {
	Type    string          `json:"type"`
	MsgType string          `json:"msgType"`
	Msg     string          `json:"msg"`
	Seq     int64           `json:"seq"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// frame serializes one SSE frame, carrying the shared sequence as the SSE id so a
// reconnecting EventSource resumes exactly where it stopped. Both clients already
// discard any payload whose seq is at or below their retained high-water mark, so
// a replayed frame they have seen is harmless.
//
// HTML escaping is off, matching the parity implementations' JSON.stringify:
// these payloads are whole HTML documents, and escaping every angle bracket would
// inflate them for no benefit. An SSE frame is never embedded in an HTML context.
func frame(seq int64, v interface{}) []byte {
	var buf bytes.Buffer
	buf.WriteString("id: ")
	buf.WriteString(strconv.FormatInt(seq, 10))
	buf.WriteString("\ndata: ")
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil
	}
	// Encode appends one newline; an SSE frame ends with a blank line.
	buf.WriteByte('\n')
	return buf.Bytes()
}

// encodeExternalChangeData builds the notification data payload that carries
// on-disk HTML to edit-mode tabs. It needs its own encoder because escaping
// must be off at THIS level: json.Marshal pre-escapes every angle bracket into
// six bytes, and the outer frame encoder cannot undo escaping already baked
// into a RawMessage. Encode appends one newline, which a RawMessage must not
// carry.
func encodeExternalChangeData(html string) json.RawMessage {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(struct {
		Kind   string `json:"kind"`
		HTML   string `json:"html"`
		Sender string `json:"sender"`
	}{Kind: "external-change", HTML: html, Sender: "file-system"}); err != nil {
		return nil
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n"))
}

// cursorFrame is a named SSE event carrying the resume baseline as its id. It
// does not reach onmessage, so it never looks like data; it exists only so a
// native EventSource records an id as early as possible on connect.
//
// resync says the server could not retain everything between where this client
// resumes and that baseline, so what it is holding is stale in a way no replay
// will fix. The client's repair is the token-free fetch of the served page it
// already runs when a change arrives too large to send, so the flag needs no
// machinery beyond noticing it.
func cursorFrame(seq int64, resync bool) []byte {
	var buf bytes.Buffer
	buf.WriteString("event: cursor\nid: ")
	buf.WriteString(strconv.FormatInt(seq, 10))
	buf.WriteString("\ndata: {\"seq\":")
	buf.WriteString(strconv.FormatInt(seq, 10))
	if resync {
		buf.WriteString(",\"resync\":true")
	}
	buf.WriteString("}\n\n")
	return buf.Bytes()
}

// relay broadcasts a peer snapshot to the live lane. It never persists, backs up,
// or advances either per-file record. It returns any evicted subscribers for the
// coordinator to drop watcher-side.
func (h *hub) relay(key, html, sender, etag string, identityMap json.RawMessage) []*subscriber {
	h.mu.Lock()
	defer h.mu.Unlock()
	seq := h.nextSeq()
	f := frame(seq, livePayload{HTML: html, Sender: sender, Seq: seq, IdentityMap: identityMap, Etag: etag})
	if f == nil {
		return nil
	}
	_, evicted := h.enqueue(key, laneLive, seq, f)
	return evicted
}

// broadcastSaved sends post-strip on-disk HTML to the saved lane. Used by disk
// saves and restores.
func (h *hub) broadcastSaved(key, html, sender string) []*subscriber {
	h.mu.Lock()
	defer h.mu.Unlock()
	seq := h.nextSeq()
	f := frame(seq, livePayload{HTML: html, Sender: sender, Seq: seq})
	if f == nil {
		return nil
	}
	_, evicted := h.enqueue(key, laneSaved, seq, f)
	return evicted
}

// publishExternalChange is what an external edit produces: a notification on the
// live lane carrying the on-disk HTML as first-class content, and the stable
// on-disk HTML on the saved lane. It observes identity first, so an external
// replacement rolls the generation before the new frame is retained.
//
// The live-lane content rides the notification's data field rather than a
// livePayload: old clients destructure `data` and only forward it (inert), and
// new clients route it through dirty-region protection before morphing, so an
// edit-mode tab's unsaved DOM state is never silently discarded. HTML larger
// than maxLiveSyncSize falls back to a bare notification; the new client
// recovers the content with a token-free fetch of the served page.
//
// Sequence allocation and enqueue happen together under one lock so the watcher
// and the relay leg share a single ordering.
//
// It reports whether a subscriber actually took the frame, which is what lets the
// watcher record the hash and stop looking. Anything weaker is a receipt for a
// delivery that never happened. In particular a resume cursor does NOT count: the
// argument that its reconnect replays the retained frame holds only inside the
// frame and cursor TTLs, and past them the incarnation is reaped, so the client
// would come back to an empty replay against a change already recorded as
// reported. Not recording costs one redundant frame when replay did work; the
// client discards or re-morphs identical content either way.
func (h *hub) publishExternalChange(key, msg, html string) (delivered bool, evicted []*subscriber) {
	probe := probeIdentity(key)

	h.mu.Lock()
	defer h.mu.Unlock()

	// Shutdown closes the hub before it stops the watcher, so a poll already inside
	// publish can arrive here afterwards. Without this it would recreate an
	// incarnation in a hub nobody will ever reap again, and the anchor it installed
	// would keep a descriptor open for the life of the process.
	if h.closed {
		probe.Close()
		return false, nil
	}

	h.observeIdentityLocked(key, probe)

	var data json.RawMessage
	if len(html) <= maxLiveSyncSize {
		data = encodeExternalChangeData(html)
	}

	accepted := 0
	nSeq := h.nextSeq()
	if n := frame(nSeq, notifyPayload{Type: "notification", MsgType: "warning", Msg: msg, Seq: nSeq, Data: data}); n != nil {
		took, dropped := h.enqueue(key, laneLive, nSeq, n)
		accepted += took
		evicted = append(evicted, dropped...)
	}
	bSeq := h.nextSeq()
	if b := frame(bSeq, livePayload{HTML: html, Sender: "file-system", Seq: bSeq}); b != nil {
		took, dropped := h.enqueue(key, laneSaved, bSeq, b)
		accepted += took
		evicted = append(evicted, dropped...)
	}
	return accepted > 0, evicted
}

// notifyWarning sends a warning notification to the live lane.
func (h *hub) notifyWarning(key, msg string) []*subscriber {
	h.mu.Lock()
	defer h.mu.Unlock()
	seq := h.nextSeq()
	if n := frame(seq, notifyPayload{Type: "notification", MsgType: "warning", Msg: msg, Seq: seq}); n != nil {
		_, evicted := h.enqueue(key, laneLive, seq, n)
		return evicted
	}
	return nil
}

// shutdown closes every stream and clears all replay state. Called before
// http.Server.Shutdown, because active streams otherwise hold graceful shutdown
// open until its timeout and are then force-closed.
func (h *hub) shutdown() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	close(h.closing)
	stop := h.janitorStop
	h.janitorStop = nil
	for key, set := range h.subs {
		for sub := range set {
			sub.stop()
		}
		delete(h.subs, key)
	}
	for path, inc := range h.incs {
		inc.anchor.Close()
		delete(h.incs, path)
	}
	h.cursors = make(map[string]*resumeCursor)
	h.replayFrames = 0
	h.replayBytes = 0
	h.mu.Unlock()

	if stop != nil {
		close(stop)
	}
}

// resolvePageURL turns a client-supplied page URL into the registered file it
// names.
//
// This is routing, not authentication. It grants no new privilege under
// htmlclay's existing origin-wide trust model, where one served page can already
// request another registered path and receive that file's token. Per-file
// isolation would need a capability and a client change.
func (s *Server) resolvePageURL(r *http.Request, raw string) (*session.File, bool) {
	if raw == "" {
		return nil, false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, false
	}
	if u.Scheme != "http" || u.Host != r.Host {
		return nil, false
	}

	// url.Parse has already decoded the path exactly once.
	rel := strings.TrimPrefix(u.Path, "/")
	rel = extractFilePath(rel)
	if rel == "" {
		rel = "index.html"
	}

	absPath, err := ValidatePath(rel, s.sessions.HomeDir())
	if err != nil {
		return nil, false
	}
	resolved, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return nil, false
	}
	return s.sessions.LookupByPath(filepath.Clean(resolved))
}

func (s *Server) handleLiveSyncStream(w http.ResponseWriter, r *http.Request) {
	// Spec §10 names the parameter document-url, matching the Document-URL
	// header the save lane already uses. page-url is the pre-spec spelling and
	// is still read, because a host being lenient about how it is addressed
	// costs nothing and a stream that silently 404s is hard to diagnose.
	href := r.URL.Query().Get("document-url")
	if href == "" {
		href = r.URL.Query().Get("page-url")
	}
	f, ok := s.resolvePageURL(r, href)
	if !ok {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}

	lane := laneLive
	if r.URL.Query().Get("lane") == laneSaved {
		lane = laneSaved
	}

	resumeID := parseResumeID(r)
	if resumeID == "" {
		s.logger.Printf("live-sync stream for %s has no resume-id; Last-Event-ID recovery only", f.RelPath)
	}

	rc := http.NewResponseController(w)
	// Clear the write deadline for this connection only. Zeroing the server-wide
	// WriteTimeout would remove the bound from every other request.
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		s.logger.Printf("live-sync: cannot clear write deadline: %v", err)
		http.Error(w, "Not Implemented", http.StatusNotImplemented)
		return
	}

	sub := &subscriber{
		key:         f.AbsPath,
		lane:        lane,
		ch:          make(chan []byte, subQueueSize),
		done:        make(chan struct{}),
		lastEventID: parseLastEventID(r),
		resumeID:    resumeID,
	}

	// The subscriber is registered BEFORE the headers are flushed, so nothing that
	// happens during setup has no recipient. Registration observes the current
	// incarnation and selects replay under f.Lock, and the coordinator raises the
	// watcher reference and hub membership in one critical section. Frames arriving
	// before the flush sit in the bounded queue and go out after the replay slice.
	f.Lock()
	baseline, replay, resync := s.coord.add(sub, f)
	f.Unlock()
	defer s.coord.remove(sub, f)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		s.logger.Printf("live-sync: cannot flush stream: %v", err)
		return
	}

	// The cursor frame first, so a native EventSource records an id as early as
	// possible, then the bounded replay slice, then the live queue.
	if !writeSSE(rc, w, cursorFrame(baseline, resync)) {
		return
	}
	for _, fr := range replay {
		if !writeSSE(rc, w, fr) {
			return
		}
	}

	ticker := time.NewTicker(keepaliveInterval)
	defer ticker.Stop()

	// This goroutine is the single writer for this connection, so keepalives and
	// broadcasts never write concurrently to one ResponseWriter, and the hub lock
	// is never held during a network write.
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.hub.closing:
			return
		case <-sub.done:
			return
		case msg := <-sub.ch:
			if !writeSSE(rc, w, msg) {
				return
			}
		case <-ticker.C:
			if !writeSSE(rc, w, []byte(": keepalive\n\n")) {
				return
			}
		}
	}
}

// parseLastEventID reads the client's resume point. EventSource sends the header
// automatically on reconnect, and both clients also accept the query form.
func parseLastEventID(r *http.Request) int64 {
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = r.URL.Query().Get("lastEventId")
	}
	v, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || v < 0 {
		return 0
	}
	return v
}

// parseResumeID reads and validates the client's resume-id query parameter: 1 to
// maxResumeIDLen URL-safe (RFC 3986 unreserved) bytes. An absent or malformed id
// returns "", which the stream still serves via Last-Event-ID alone.
func parseResumeID(r *http.Request) string {
	raw := r.URL.Query().Get("resume-id")
	if raw == "" || len(raw) > maxResumeIDLen {
		return ""
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			continue
		}
		return ""
	}
	return raw
}

func writeSSE(rc *http.ResponseController, w http.ResponseWriter, msg []byte) bool {
	// A rolling per-write deadline, so an evicted slow subscriber actually
	// unblocks instead of leaking a goroutine stuck inside Write.
	if err := rc.SetWriteDeadline(time.Now().Add(sseWriteDeadline)); err != nil {
		return false
	}
	if _, err := w.Write(msg); err != nil {
		return false
	}
	return rc.Flush() == nil
}

// handleLiveSyncSave is relay-only. It never persists its payload, backs it up,
// writes it to disk, or advances either per-file record, and it broadcasts to the
// live lane only.
func (s *Server) handleLiveSyncSave(w http.ResponseWriter, r *http.Request) {
	// Spec §10 addresses the relay with Document-URL, the same header the save
	// lane uses. Page-URL is the pre-spec spelling and is still read.
	href := r.Header.Get("Document-URL")
	if href == "" {
		href = r.Header.Get("Page-URL")
	}
	f, ok := s.resolvePageURL(r, href)
	if !ok {
		s.writeError(w, http.StatusNotFound, "unknown page")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxLiveSyncSize)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			s.writeError(w, http.StatusRequestEntityTooLarge, "body too large (max 12MB)")
			return
		}
		s.writeError(w, http.StatusInternalServerError, "read error")
		return
	}

	// Pointers, not strings, so "absent" and "present but empty" stay different
	// answers. The lane is chosen by which field the caller NAMED, and a body that
	// names a lane with nothing in it is a broken client, not a missing field.
	var payload struct {
		Snapshot    *string         `json:"snapshot"`
		Document    *string         `json:"document"`
		HTML        *string         `json:"html"`
		Sender      string          `json:"sender"`
		IdentityMap json.RawMessage `json:"identityMap"`
		Etag        string          `json:"etag"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid json body")
		return
	}

	// §10 names the field for its audience: a snapshot goes to the other editors,
	// a document to the viewers. `html` is the pre-spec spelling of `snapshot` and
	// stays forever, because documents saved under it go on running with no way for
	// a library update to reach their inline script.
	//
	// PRESENCE is the test, not emptiness, which is the rule hyperclay's relay holds
	// and the reason is the same: a real snapshot paired with a broken document is a
	// confused client, and guessing which half it meant would silently send editor
	// content to viewers or the reverse. Exactly one lane, or the body is refused.
	//
	// `html` is the pre-spec spelling of `snapshot` and is read ON THE PRE-SPEC
	// ADDRESS ONLY. Nothing frozen posts it to /_/sync, because that address is new:
	// both clients pair the key with the address in one wire profile chosen once for
	// the life of the page, so a client on the spec address sends `snapshot`. Reading
	// it here anyway would buy nothing and cost the thing this train is for, since
	// hyperclay's spec route recognises only `snapshot` and `document`, and one
	// address answering differently on three hosts is the whole class of bug.
	legacyAddress := r.URL.Path != "/_/sync"

	lane, relayHTML := "", ""
	named := 0
	if payload.Snapshot != nil || (legacyAddress && payload.HTML != nil) {
		lane, named = laneLive, named+1
		if payload.Snapshot != nil {
			relayHTML = *payload.Snapshot
		} else {
			relayHTML = *payload.HTML
		}
	}
	if payload.Document != nil {
		lane, relayHTML = laneSaved, *payload.Document
		named++
	}
	if named != 1 {
		s.writeError(w, http.StatusBadRequest, "send exactly one of snapshot or document")
		return
	}

	// The same bar the save lane holds bytes to, and literally the same predicate:
	// this content reaches the open tabs through a morph, so a fragment or a JSON
	// blob turns each of them into something that is not a document. HasHTMLTag and
	// not IsCompleteHTMLDocument, because the stricter one also demands a closing
	// tag, which would refuse as a RELAY the exact bytes this host accepts as a
	// SAVE, and which both other hosts relay happily.
	//
	// ON THE SPEC ROUTE ONLY. /_/live-sync/save is the pre-spec address and it has
	// always taken any non-empty string, including the body innerHTML that
	// hyperclayjs's exported captureBodyForSync() returns. A document saved against
	// that API goes on running for years with no way for a library update to reach
	// its inline script, so adding a rule here would break it permanently and
	// silently. A client posting to the spec address is one that can be told.
	if !legacyAddress && !htmlutil.HasHTMLTag([]byte(relayHTML)) {
		s.writeError(w, http.StatusUnprocessableEntity, "not a complete HTML document")
		return
	}
	if relayHTML == "" {
		s.writeError(w, http.StatusBadRequest, "missing snapshot")
		return
	}

	// §10: the message "MAY also carry `sender`". It is how a page recognises and
	// drops its own update coming back, so a client that sends none is only giving
	// up an echo it did not want; it is not a malformed request. Requiring it made
	// the conformance page's own relay probe fail against this host.
	if len(payload.Sender) > maxSenderLen {
		s.writeError(w, http.StatusBadRequest, "invalid sender")
		return
	}

	// The relay is a path that hands a full document to a browser, so it owes
	// exactly what the other three owe, and forBrowser is where both halves live so
	// that no caller can do one and forget the other. This one used to strip and not
	// stamp.
	//
	// The strip is §9 and normative: "a host strips it from every frame it fans
	// out". A save token is minted for one person and one file, so a tab that adopts
	// somebody else's saves AS them, and goes on saving after its own access ends,
	// because revoking a person's access cannot reach a credential issued to
	// another. clayjs strips on the client too; the obligation is on the host
	// precisely because a buggy or hostile client is the case that matters.
	//
	// The stamp repairs a sender that dropped the id. A client that treats
	// `documentid` as save chrome relays an identity-free document to every peer; a
	// peer that saves it writes a file with no id, and moving that file while this
	// host is closed makes the next open mint a NEW identity rather than find the
	// history it already has. Both tabs on a relay hold the same file, so the id put
	// back is the one the receiver already carries, never a different one.
	//
	// §9's rule read backwards is about the TOKEN: a stripped frame must not remove
	// the receiving tab's own. Restoring the id does not touch that.
	f.Lock()
	key := f.HistoryKey()
	f.Unlock()
	relayHTML = forBrowser([]byte(relayHTML), key)

	identityMap := payload.IdentityMap
	if len(identityMap) > 0 {
		// Hosted parity requires a non-null, non-array object.
		trimmed := strings.TrimSpace(string(identityMap))
		if !strings.HasPrefix(trimmed, "{") {
			s.writeError(w, http.StatusBadRequest, "identityMap must be an object")
			return
		}
	}

	if lane == laneSaved {
		// Viewers, on the same lane a save publishes to. Nothing is written: §10 is
		// flat about it, and the usual flow needs no client relay here at all, since
		// a save already pushes the new document out. This exists for the case §10
		// names, a client wanting viewers updated without a save behind it.
		//
		// identityMap is deliberately not carried: it pairs elements across a morph
		// between two EDITORS, and a viewer has no working state to preserve. The etag
		// is dropped here for the matching reason: it tells a tab which version its next
		// save is answering, and a viewer makes no saves.
		s.coord.broadcastSaved(f, relayHTML, payload.Sender)
	} else {
		s.coord.relay(f, relayHTML, payload.Sender, payload.Etag, identityMap)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(`{"ok":true}`))
}

// forBrowser turns bytes on disk into the bytes a browser should be handed: the
// save token removed, the document's tracked identity put back.
//
// Both halves in one place on purpose. Every path that sends a full document to a
// browser owes both, and they are easy to get half right: the strip has always
// been here, and the stamp was missing from all three callers.
//
// The stamp matters because the id lives on disk only once the client's own save
// has carried it back, and a restore strips it outright (spec §4: the host never
// writes one). So the bytes broadcast after a save, a restore or an outside edit
// routinely carry no identity at all, and a receiving tab morphs them onto its
// live document. A client that does not know to protect the attribute then loses
// its copy, and the next save from that tab writes a file with no id, orphaning
// every version ever taken of it. Serving already re-stamps for exactly this
// reason; a broadcast is the same host handing the same document to the same
// browser, so it owes the same thing.
//
// key is the caller's already-resolved history key, so this takes no lock and
// cannot be called with a key derived from whatever happens to be on disk now.
func forBrowser(data []byte, key string) string {
	stripped := htmlutil.StripToken(data)
	if id, ok := versions.IDFromKey(key); ok {
		stripped = htmlutil.SetHTMLClayID(stripped, id)
	}
	return string(stripped)
}

// broadcastDiskHTML publishes bytes that just landed on disk to the saved lane.
// Caller holds f.Lock() and passes the history key it resolved there.
func (s *Server) broadcastDiskHTML(f *session.File, data []byte, key string) {
	s.coord.broadcastSaved(f, forBrowser(data, key), "file-system")
}
