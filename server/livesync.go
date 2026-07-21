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

	"github.com/panphora/htmlclay/htmlutil"
	"github.com/panphora/htmlclay/session"
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

// ShutdownBudget is how long graceful shutdown may take. main.go uses it for its
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
// recovery markers for frames it could no longer retain.
type laneBucket struct {
	frames         []retainedFrame
	bytes          int
	droppedThrough int64
	needsResync    bool
}

// incarnation is one generation of the file at a path. A new file at the same
// path is a new incarnation and never inherits the old one's retained frames.
// The anchor is an open read-only handle whose identity is compared with
// os.SameFile; keeping it open until comparison prevents Unix inode reuse from
// making a distinct new file look like the same one.
type incarnation struct {
	generation int64
	anchor     *os.File
	anchorInfo os.FileInfo
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
// seq the cursor frame should carry) plus the frames to replay. It never pushes
// replay into the bounded live queue; the writer sends the returned slice first.
func (h *hub) add(sub *subscriber) (baseline int64, replay [][]byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.expireLocked()
	if h.closed {
		sub.stop()
		return h.seq, nil
	}
	set, ok := h.subs[sub.key]
	if !ok {
		set = make(map[*subscriber]struct{})
		h.subs[sub.key] = set
	}
	set[sub] = struct{}{}

	// Observe identity BEFORE selecting replay: a same-path B is recognized and
	// rolls the generation before any A frame can be returned.
	inc := h.observeIdentityLocked(sub.key)
	from := h.resumePointLocked(inc, sub)

	bucket := inc.bucket(sub.lane)
	for _, rf := range bucket.frames {
		if rf.seq > from {
			replay = append(replay, rf.frame)
		}
	}
	return from, replay
}

// resumePointLocked decides where this subscriber resumes and keeps its cursor
// active. An explicit Last-Event-ID for the current incarnation wins unless it is
// a future id above the high-water; otherwise the saved cursor baseline is used;
// a first connection records the current sequence as its baseline.
func (h *hub) resumePointLocked(inc *incarnation, sub *subscriber) int64 {
	if sub.lastEventID > 0 && sub.lastEventID <= h.seq {
		if sub.resumeID != "" {
			h.activateCursorLocked(inc, sub, sub.lastEventID)
		}
		return sub.lastEventID
	}
	if sub.resumeID == "" {
		return h.seq
	}
	ck := cursorKey(sub.key, inc.generation, sub.lane, sub.resumeID)
	if c, ok := h.cursors[ck]; ok && c.generation == inc.generation {
		c.disconnectAt = time.Time{}
		c.touched = h.clock()
		return c.baseline
	}
	h.cursors[ck] = &resumeCursor{
		path:       sub.key,
		generation: inc.generation,
		lane:       sub.lane,
		resumeID:   sub.resumeID,
		baseline:   h.seq,
		touched:    h.clock(),
	}
	return h.seq
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

// observeIdentityLocked compares the live file against the anchor and rolls the
// generation on an external identity change, clearing the old buckets and
// cursors. A server-authorized atomic write does not reach here: acceptServer
// Replacement re-anchors first, so the next observe sees the same file.
func (h *hub) observeIdentityLocked(path string) *incarnation {
	inc := h.ensureIncarnationLocked(path)
	cur, err := os.Open(path)
	if err != nil {
		return inc
	}
	info, serr := cur.Stat()
	if serr != nil || !info.Mode().IsRegular() {
		cur.Close()
		return inc
	}
	if inc.anchor == nil {
		inc.anchor = cur
		inc.anchorInfo = info
		inc.lastTouch = h.clock()
		return inc
	}
	if os.SameFile(inc.anchorInfo, info) {
		cur.Close()
		return inc
	}
	// External replacement: roll to a new generation.
	h.clearBucketsLocked(inc)
	h.clearCursorsForPathLocked(path)
	inc.anchor.Close()
	inc.anchor = cur
	inc.anchorInfo = info
	inc.generation++
	inc.lastTouch = h.clock()
	return inc
}

// acceptServerReplacement re-anchors an existing incarnation to the file's
// current inode without rolling the generation: save, id injection, clone fork
// and restore are all changes to the same logical file. It never creates an
// incarnation for a path with no live-sync interest, so it opens no descriptor
// for a file nobody is streaming. Caller holds f.Lock.
func (h *hub) acceptServerReplacement(path string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	inc, ok := h.incs[path]
	if !ok {
		return
	}
	cur, err := os.Open(path)
	if err != nil {
		return
	}
	info, serr := cur.Stat()
	if serr != nil || !info.Mode().IsRegular() {
		cur.Close()
		return
	}
	if inc.anchor != nil {
		inc.anchor.Close()
	}
	inc.anchor = cur
	inc.anchorInfo = info
	inc.lastTouch = h.clock()
}

// markAbsent clears an incarnation's buffers when the watcher sees the file gone,
// without emitting a deletion event.
func (h *hub) markAbsent(path string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	inc, ok := h.incs[path]
	if !ok {
		return
	}
	h.clearBucketsLocked(inc)
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
		b.needsResync = true
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
		if inc.anchor != nil {
			inc.anchor.Close()
		}
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
		if victim.inc.anchor != nil {
			victim.inc.anchor.Close()
		}
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
// event rather than losing it. Caller must hold h.mu.
func (h *hub) enqueue(key, lane string, seq int64, f []byte) []*subscriber {
	h.retainLocked(key, lane, seq, f)

	set := h.subs[key]
	var evicted []*subscriber
	for sub := range set {
		if sub.lane != lane {
			continue
		}
		if !offer(sub, [][]byte{f}) {
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
	return evicted
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
}

type notifyPayload struct {
	Type    string `json:"type"`
	MsgType string `json:"msgType"`
	Msg     string `json:"msg"`
	Seq     int64  `json:"seq"`
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

// cursorFrame is a named SSE event carrying the resume baseline as its id. It
// does not reach onmessage, so it never looks like data; it exists only so a
// native EventSource records an id as early as possible on connect.
func cursorFrame(seq int64) []byte {
	var buf bytes.Buffer
	buf.WriteString("event: cursor\nid: ")
	buf.WriteString(strconv.FormatInt(seq, 10))
	buf.WriteString("\ndata: {\"seq\":")
	buf.WriteString(strconv.FormatInt(seq, 10))
	buf.WriteString("}\n\n")
	return buf.Bytes()
}

// relay broadcasts a peer snapshot to the live lane. It never persists, backs up,
// or advances either per-file record. It returns any evicted subscribers for the
// coordinator to drop watcher-side.
func (h *hub) relay(key, html, sender string, identityMap json.RawMessage) []*subscriber {
	h.mu.Lock()
	defer h.mu.Unlock()
	seq := h.nextSeq()
	f := frame(seq, livePayload{HTML: html, Sender: sender, Seq: seq, IdentityMap: identityMap})
	if f == nil {
		return nil
	}
	return h.enqueue(key, laneLive, seq, f)
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
	return h.enqueue(key, laneSaved, seq, f)
}

// publishExternalChange is what an external edit produces: a notification on the
// live lane, and the stable on-disk HTML on the saved lane. It observes identity
// first, so an external replacement rolls the generation before the new frame is
// retained.
//
// The live lane gets a notice and not content on purpose. After B0 every htmlclay
// tab is an edit-mode tab holding unsaved DOM state, and pushing content there
// would silently discard it.
//
// Sequence allocation and enqueue happen together under one lock so the watcher
// and the relay leg share a single ordering.
func (h *hub) publishExternalChange(key, msg, html string) []*subscriber {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.observeIdentityLocked(key)

	var evicted []*subscriber
	nSeq := h.nextSeq()
	if n := frame(nSeq, notifyPayload{Type: "notification", MsgType: "warning", Msg: msg, Seq: nSeq}); n != nil {
		evicted = append(evicted, h.enqueue(key, laneLive, nSeq, n)...)
	}
	bSeq := h.nextSeq()
	if b := frame(bSeq, livePayload{HTML: html, Sender: "file-system", Seq: bSeq}); b != nil {
		evicted = append(evicted, h.enqueue(key, laneSaved, bSeq, b)...)
	}
	return evicted
}

// notifyWarning sends a warning notification to the live lane.
func (h *hub) notifyWarning(key, msg string) []*subscriber {
	h.mu.Lock()
	defer h.mu.Unlock()
	seq := h.nextSeq()
	if n := frame(seq, notifyPayload{Type: "notification", MsgType: "warning", Msg: msg, Seq: seq}); n != nil {
		return h.enqueue(key, laneLive, seq, n)
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
		if inc.anchor != nil {
			inc.anchor.Close()
		}
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
	f, ok := s.resolvePageURL(r, r.URL.Query().Get("page-url"))
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
	baseline, replay := s.coord.add(sub, f)
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
	if !writeSSE(rc, w, cursorFrame(baseline)) {
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
	f, ok := s.resolvePageURL(r, r.Header.Get("Page-URL"))
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

	var payload struct {
		HTML        string          `json:"html"`
		Sender      string          `json:"sender"`
		IdentityMap json.RawMessage `json:"identityMap"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	if payload.HTML == "" {
		s.writeError(w, http.StatusBadRequest, "missing html")
		return
	}
	if payload.Sender == "" || len(payload.Sender) > maxSenderLen {
		s.writeError(w, http.StatusBadRequest, "invalid sender")
		return
	}

	identityMap := payload.IdentityMap
	if len(identityMap) > 0 {
		// Hosted parity requires a non-null, non-array object. hyperclay-local
		// drops the field entirely, so the two existing implementations are not
		// byte identical; follow hosted.
		trimmed := strings.TrimSpace(string(identityMap))
		if !strings.HasPrefix(trimmed, "{") {
			s.writeError(w, http.StatusBadRequest, "identityMap must be an object")
			return
		}
	}

	s.coord.relay(f, payload.HTML, payload.Sender, identityMap)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(`{"ok":true}`))
}

// broadcastDiskHTML publishes bytes that just landed on disk to the saved lane.
func (s *Server) broadcastDiskHTML(f *session.File, data []byte) {
	s.coord.broadcastSaved(f, string(htmlutil.StripToken(data)), "file-system")
}
