package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"runtime"
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
	// inside Write fails on its own instead of pinning a goroutine forever.
	sseWriteDeadline = 10 * time.Second
	// keepaliveInterval keeps intermediaries and idle-timeout logic from closing
	// an otherwise silent stream.
	keepaliveInterval = 25 * time.Second
)

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
}

func (sub *subscriber) stop() {
	sub.once.Do(func() { close(sub.done) })
}

// hub owns every SSE subscriber and the single broadcast sequence counter shared
// by the relay leg (B3) and the watcher (B4). There is exactly one counter.
type hub struct {
	mu      sync.Mutex
	subs    map[string]map[*subscriber]struct{}
	seq     int64
	closing chan struct{}
	closed  bool
}

func newHub() *hub {
	return &hub{
		subs: make(map[string]map[*subscriber]struct{}),
		// Seed from wall-clock milliseconds, as the parity implementation does. A
		// counter restarting at 1 after a server restart is rejected by the
		// client's retained high-water mark and the stream silently stops updating.
		seq:     time.Now().UnixMilli(),
		closing: make(chan struct{}),
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
	return h.seq
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

// enqueue posts pre-serialized frames to every subscriber on lane. Frames for one
// subscriber keep their insertion order. A subscriber whose queue is full is
// evicted and unblocked: dropping it from the map alone would not free a
// goroutine already stuck inside Write, which is what the rolling write deadline
// on the connection handles.
//
// Caller must hold h.mu.
func (h *hub) enqueue(key, lane string, frames ...[]byte) {
	set := h.subs[key]
	var evicted []*subscriber
	for sub := range set {
		if sub.lane != lane {
			continue
		}
		if !offer(sub, frames) {
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

// frame serializes one SSE data frame. HTML escaping is off, matching the parity
// implementations' JSON.stringify: these payloads are whole HTML documents, and
// escaping every angle bracket would inflate them for no benefit. An SSE frame is
// never embedded in an HTML context.
func frame(v interface{}) []byte {
	var buf bytes.Buffer
	buf.WriteString("data: ")
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
	f := frame(livePayload{HTML: html, Sender: sender, Seq: h.nextSeq(), IdentityMap: identityMap})
	if f == nil {
		return
	}
	h.enqueue(key, laneLive, f)
}

// broadcastSaved sends post-strip on-disk HTML to the saved lane. Used by disk
// saves and restores.
func (h *hub) broadcastSaved(key, html, sender string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f := frame(livePayload{HTML: html, Sender: sender, Seq: h.nextSeq()})
	if f == nil {
		return
	}
	h.enqueue(key, laneSaved, f)
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

	if n := frame(notifyPayload{Type: "notification", MsgType: "warning", Msg: msg}); n != nil {
		h.enqueue(key, laneLive, n)
	}
	if b := frame(livePayload{HTML: html, Sender: "file-system", Seq: h.nextSeq()}); b != nil {
		h.enqueue(key, laneSaved, b)
	}
}

// notifyWarning sends a warning notification to the live lane.
func (h *hub) notifyWarning(key, msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if n := frame(notifyPayload{Type: "notification", MsgType: "warning", Msg: msg}); n != nil {
		h.enqueue(key, laneLive, n)
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

// samePath compares two absolute paths, folding case on the platforms whose
// default filesystem ignores it.
func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.EqualFold(a, b)
	}
	return a == b
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

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		s.logger.Printf("live-sync: cannot flush stream: %v", err)
		return
	}

	sub := &subscriber{
		key:  f.AbsPath,
		lane: lane,
		ch:   make(chan []byte, subQueueSize),
		done: make(chan struct{}),
	}

	// One watch and one unwatch per connection. The watcher refcounts, so it polls
	// only currently-subscribed files and stops when the last stream goes away.
	s.hub.add(sub)
	s.watcher.watch(f)
	defer func() {
		s.hub.remove(sub)
		s.watcher.unwatch(f)
	}()

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
