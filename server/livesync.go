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

	// replayDepth and replayMaxBytes bound the per-key, per-lane replay buffer that
	// backs Last-Event-ID recovery. Frames are whole HTML documents, so the byte
	// cap rather than the count is what actually holds memory down.
	replayDepth    = 16
	replayMaxBytes = 4 * 1024 * 1024

	// seqPersistWindow is how far ahead of the live sequence the high-water mark is
	// persisted, so the counter survives a restart without an fsync per event.
	seqPersistWindow = 10000
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
}

func (sub *subscriber) stop() {
	sub.once.Do(func() { close(sub.done) })
}

// retainedFrame is one frame held for Last-Event-ID replay.
type retainedFrame struct {
	seq   int64
	frame []byte
}

// hub owns every SSE subscriber and the single broadcast sequence counter shared
// by the relay leg (B3) and the watcher (B4). There is exactly one counter.
type hub struct {
	mu      sync.Mutex
	subs    map[string]map[*subscriber]struct{}
	seq     int64
	closing chan struct{}
	closed  bool

	// replay holds the most recent frames per key and lane so a subscriber that
	// reconnects with Last-Event-ID gets what it missed. Delivery was previously
	// lossy in two places with no way to notice: a relay landing between the header
	// flush and hub.add had no recipient, and a subscriber whose bounded queue
	// filled was evicted along with the event that filled it.
	replay      map[string][]retainedFrame
	replayBytes map[string]int

	// seqPath persists the sequence high-water mark. Seeding from the wall clock
	// alone meant a backward clock change plus a restart put every new sequence
	// below what an open client had retained, and both clients then discarded every
	// update until real time caught up. Empty disables persistence.
	seqPath   string
	persisted int64
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
		subs:        make(map[string]map[*subscriber]struct{}),
		replay:      make(map[string][]retainedFrame),
		replayBytes: make(map[string]int),
		// Seed from wall-clock milliseconds, as the parity implementation does, but
		// never below the persisted high-water mark. A counter restarting below what
		// the client retained is rejected and the stream silently stops updating.
		seq:       seq,
		persisted: persisted,
		closing:   make(chan struct{}),
		seqPath:   seqPath,
	}
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

// add registers a subscriber.
//
// It deliberately reports nothing about being the first on a key. Watcher
// lifecycle is driven by the stream handler calling watch and unwatch once each,
// because inferring it from map transitions breaks the moment eviction removes a
// subscriber the handler will later remove again.
func (h *hub) add(sub *subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		sub.stop()
		return
	}
	set, ok := h.subs[sub.key]
	if !ok {
		set = make(map[*subscriber]struct{})
		h.subs[sub.key] = set
	}
	set[sub] = struct{}{}

	// Replay whatever this client missed while it was disconnected. A fresh client
	// sends no Last-Event-ID and gets nothing: it just loaded the page and holds
	// current content already.
	if sub.lastEventID <= 0 {
		return
	}
	var missed [][]byte
	for _, rf := range h.replay[replayKey(sub.key, sub.lane)] {
		if rf.seq > sub.lastEventID {
			missed = append(missed, rf.frame)
		}
	}
	if len(missed) > 0 && !offer(sub, missed) {
		delete(set, sub)
		if len(set) == 0 {
			delete(h.subs, sub.key)
		}
		sub.stop()
	}
}

func replayKey(key, lane string) string { return key + "\x00" + lane }

// retain records a frame for Last-Event-ID replay, bounded by both count and
// total bytes so whole-document frames cannot grow memory without limit. A client
// disconnected for longer than the buffer holds still loses events; that residual
// is bounded and visible here rather than silent.
//
// Caller must hold h.mu.
func (h *hub) retain(key, lane string, seq int64, f []byte) {
	rk := replayKey(key, lane)
	buf := append(h.replay[rk], retainedFrame{seq: seq, frame: f})
	total := h.replayBytes[rk] + len(f)
	for len(buf) > replayDepth || (total > replayMaxBytes && len(buf) > 1) {
		total -= len(buf[0].frame)
		buf = buf[1:]
	}
	h.replay[rk] = buf
	h.replayBytes[rk] = total
}

// remove drops a subscriber.
func (h *hub) remove(sub *subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set, ok := h.subs[sub.key]
	if !ok {
		return
	}
	delete(set, sub)
	if len(set) == 0 {
		delete(h.subs, sub.key)
	}
}

func (h *hub) subscriberCount(key string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs[key])
}

// enqueue posts one pre-serialized frame to every subscriber on lane, and retains
// it for replay. Frames for one subscriber keep their insertion order. A
// subscriber whose queue is full is evicted and unblocked: dropping it from the
// map alone would not free a goroutine already stuck inside Write, which is what
// the rolling write deadline on the connection handles. The evicted client
// reconnects with Last-Event-ID and is replayed from the buffer, so eviction
// costs it a reconnect rather than the events themselves.
//
// Caller must hold h.mu.
func (h *hub) enqueue(key, lane string, seq int64, f []byte) {
	h.retain(key, lane, seq, f)

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

// relay broadcasts a peer snapshot to the live lane. It never persists, backs up,
// or advances either per-file record.
func (h *hub) relay(key, html, sender string, identityMap json.RawMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	seq := h.nextSeq()
	f := frame(seq, livePayload{HTML: html, Sender: sender, Seq: seq, IdentityMap: identityMap})
	if f == nil {
		return
	}
	h.enqueue(key, laneLive, seq, f)
}

// broadcastSaved sends post-strip on-disk HTML to the saved lane. Used by disk
// saves and restores.
func (h *hub) broadcastSaved(key, html, sender string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	seq := h.nextSeq()
	f := frame(seq, livePayload{HTML: html, Sender: sender, Seq: seq})
	if f == nil {
		return
	}
	h.enqueue(key, laneSaved, seq, f)
}

// publishExternalChange is what an external edit produces: a notification on the
// live lane, and the stable on-disk HTML on the saved lane.
//
// The live lane gets a notice and not content on purpose. After B0 every htmlclay
// tab is an edit-mode tab holding unsaved DOM state, and pushing content there
// would silently discard it.
//
// Sequence allocation and enqueue happen together under one lock so the watcher
// and the relay leg share a single ordering.
func (h *hub) publishExternalChange(key, msg, html string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	nSeq := h.nextSeq()
	if n := frame(nSeq, notifyPayload{Type: "notification", MsgType: "warning", Msg: msg}); n != nil {
		h.enqueue(key, laneLive, nSeq, n)
	}
	bSeq := h.nextSeq()
	if b := frame(bSeq, livePayload{HTML: html, Sender: "file-system", Seq: bSeq}); b != nil {
		h.enqueue(key, laneSaved, bSeq, b)
	}
}

// notifyWarning sends a warning notification to the live lane.
func (h *hub) notifyWarning(key, msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	seq := h.nextSeq()
	if n := frame(seq, notifyPayload{Type: "notification", MsgType: "warning", Msg: msg}); n != nil {
		h.enqueue(key, laneLive, seq, n)
	}
}

// shutdown closes every stream. Called before http.Server.Shutdown, because
// active streams otherwise hold graceful shutdown open until its timeout and are
// then force-closed.
func (h *hub) shutdown() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	close(h.closing)
	for key, set := range h.subs {
		for sub := range set {
			sub.stop()
		}
		delete(h.subs, key)
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
	}

	// The subscriber is registered BEFORE the headers are flushed, so nothing that
	// happens during setup has no recipient. Flushing first left a window in which
	// a relay found an empty subscriber set and the event was simply gone. Frames
	// arriving before the flush sit in the bounded queue and go out right after it.
	//
	// One watch and one unwatch per connection. The watcher refcounts, so it polls
	// only currently-subscribed files and stops when the last stream goes away.
	s.hub.add(sub)
	s.watcher.watch(f)
	defer func() {
		// unwatch runs before hub.remove so a poll that is mid-flight cannot publish
		// into a set this connection has already left: unwatch is what tells the
		// watcher to stop, and it only returns once no orphaned check can record.
		s.watcher.unwatch(f)
		s.hub.remove(sub)
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		s.logger.Printf("live-sync: cannot flush stream: %v", err)
		return
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

	s.hub.relay(f.AbsPath, payload.HTML, payload.Sender, identityMap)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(`{"ok":true}`))
}

// broadcastDiskHTML publishes bytes that just landed on disk to the saved lane.
func (s *Server) broadcastDiskHTML(f *session.File, data []byte) {
	s.hub.broadcastSaved(f.AbsPath, string(htmlutil.StripToken(data)), "file-system")
}
