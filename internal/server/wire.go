package server

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/panphora/htmlclay/internal/session"
	"github.com/panphora/htmlclay/internal/versions"
)

// The wire is a per-file control channel between a page and a local process. A
// page sends a request ("rewrite the section I circled"), a process answers with
// progress and a terminal frame, and the process edits the FILE. HTML never rides
// the wire: the edit reaches the page as an ordinary external change, through the
// watcher and the live-sync lane that already exist. That split is the whole
// design, and it is why this file has no morphing, no content lane, and no
// interest in what a payload contains.
//
// Everything here is bounded. The wire hub's lock is a LEAF: nothing is called
// while it is held except channel sends with a default case, so it joins the
// documented order (session.File, coordinator, watcher, hub) without adding a
// cycle. Network writes always happen on the connection's own goroutine, never
// under the hub lock, exactly as the live-sync hub does it.
const (
	// A control frame is not a document. Anything approaching a document belongs
	// in the file, which is the wire's premise; a cap this low makes "HTML never
	// rides the wire" fail loudly in development instead of drifting in quietly.
	maxWireBody = 1 << 20
	// Free-form progress text. Bounded separately so a chatty handler cannot
	// flood observers' queues into eviction.
	maxWireText = 4 << 10
	// Request ids are opaque to the router. It validates only that one exists and
	// is not unbounded; it never parses, namespaces or rewrites one.
	maxWireIDLen = 128

	// One handler, a page or two, and a couple of tails. Past this something is
	// wrong and growing memory is not the answer. The live-sync hub has no
	// equivalent cap to inherit: its bounds are per-stream queue size, the
	// subscriptions one shared stream may carry, and the replay caps.
	maxWireSubs = 8
	// Retained terminal frames per file. See wireChannel.terminal.
	maxWireTerminals = 32
	wireTerminalTTL  = 5 * time.Minute
	wireQueueSize    = 32

	// The per-file caps bound one channel; this bounds all of them together. A
	// page may send on any registered path on its origin, so without a global
	// ceiling a loop over a trusted tree pins maxWireTerminals frames per file for
	// the whole TTL, and the per-file limits never fire. Oldest-first eviction,
	// like the live-sync replay cache.
	wireGlobalMaxTerminalBytes = 8 << 20
)

var (
	errWireHandlerTaken = errors.New("a handler is already attached")
	errWireBusy         = errors.New("too many wire subscribers")
	errWireClosed       = errors.New("shutting down")
)

// wireEnvelope is the whole protocol. The router validates the routing fields
// and treats Payload as opaque JSON.
//
// File and From are stamped by the SERVER and any client-supplied value is
// discarded. File, because a page that could name its own path would launder a
// write through the agent's OS authority into a file the page can never touch.
// From, because it identifies a connection, and a value the sender chooses is a
// value a hostile sender can choose to be someone else's.
type wireEnvelope struct {
	V       int             `json:"v"`
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	From    string          `json:"from,omitempty"`
	File    string          `json:"file"`
	Text    string          `json:"text,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// isTerminal reports whether this frame ends a request. Only terminal frames are
// retained, because a lost status frame is repaired by the next one and a lost
// terminal frame is repaired by nothing.
func (e wireEnvelope) isTerminal() bool {
	return e.Type == "wire/done" || e.Type == "wire/error"
}

type wireSub struct {
	key     string
	handler bool
	ch      chan []byte
	done    chan struct{}
	once    sync.Once
	// removed makes removal idempotent, so an eviction and the handler's own
	// defer cannot both tear down one subscriber. Guarded by wireHub.mu.
	removed bool
}

func (sub *wireSub) stop() { sub.once.Do(func() { close(sub.done) }) }

type wireTerminal struct {
	seq   int64
	frame []byte
	at    time.Time
}

// wireChannel is one file's wire: at most one handler, capped observers, and the
// terminal frame of each recent request.
//
// Retention is keyed by request id rather than kept as an anonymous ring so that
// a reconnecting subscriber recovers the OUTCOME of its requests, which is the
// only question it ever asks, and so a request that terminates twice keeps the
// first answer rather than the last.
type wireChannel struct {
	subs     map[*wireSub]struct{}
	handler  *wireSub
	terminal map[string]wireTerminal
}

// wireHub lives on the Server, not on the process-wide runtime, because a site is
// the security and lifetime boundary for an instruction channel. untrustFolder
// already calls s.close() on the site and drops it (cmd/htmlclay/folders.go), so an untrusted
// folder cannot keep a live wire into itself, and graceful shutdown closes these
// streams before the HTTP drain instead of holding it to ShutdownBudget. A
// process-wide hub would need a new bookkeeping call at both sites, and the one
// that gets forgotten is the one that matters.
type wireHub struct {
	mu      sync.Mutex
	chans   map[string]*wireChannel
	seq     int64
	closing chan struct{}
	closed  bool

	// terminalBytes is the running total across every channel, so the global cap
	// costs no walk to check. Every path that adds or drops a retained terminal
	// goes through retainTerminalLocked / dropTerminalLocked to keep it honest.
	terminalBytes int
}

func newWireHub() *wireHub {
	return &wireHub{chans: make(map[string]*wireChannel), closing: make(chan struct{})}
}

// nextSeq hands out a monotonic id used as the SSE id, so a reconnecting
// EventSource can ask for "everything after X".
//
// It floors on the wall clock for the same reason the live-sync hub does: after a
// restart the counter must resume ABOVE anything already handed out, or a client
// reconnecting with an old Last-Event-ID discards every new frame as stale. Unlike
// live-sync this needs no persisted high-water mark, because the wire retains
// nothing across a restart to be resumed into: the floor alone is sufficient, and
// borrowing live-sync's counter would mean taking its lock from inside this one.
// Caller holds wireHub.mu.
func (wh *wireHub) nextSeqLocked() int64 {
	if now := time.Now().UnixMilli(); now > wh.seq {
		wh.seq = now
	} else {
		wh.seq++
	}
	return wh.seq
}

func (wh *wireHub) ensureLocked(key string) *wireChannel {
	c, ok := wh.chans[key]
	if !ok {
		c = &wireChannel{
			subs:     make(map[*wireSub]struct{}),
			terminal: make(map[string]wireTerminal),
		}
		wh.chans[key] = c
	}
	return c
}

// add installs a subscriber and returns the position it resumes from with the
// terminal frames retained past that position. The handler slot is decided under
// the SAME lock as membership, so two processes can never both believe they hold
// it. The position is chosen under that lock too, so it is never above a frame
// the subscriber is still owed: a page that reconnects from the cursor it was
// handed is replayed everything after it.
func (wh *wireHub) add(sub *wireSub, lastEventID int64) (int64, [][]byte, error) {
	wh.mu.Lock()
	defer wh.mu.Unlock()
	if wh.closed {
		return 0, nil, errWireClosed
	}
	wh.sweepLocked()
	c := wh.ensureLocked(sub.key)
	// The cap counts observers only, so the exclusive slot is reserved rather than
	// competed for: eight tails on a file must not be able to lock the user's own
	// agent out of a slot that is free.
	if !sub.handler && len(c.subs) >= maxWireSubs {
		return 0, nil, errWireBusy
	}
	if sub.handler {
		if c.handler != nil {
			return 0, nil, errWireHandlerTaken
		}
		c.handler = sub
	}
	c.subs[sub] = struct{}{}

	// A fresh subscription replays nothing. Replaying to a subscriber with no
	// cursor would hand a reloaded page the terminal frame of a request it
	// cancelled before reloading, and the page, having no memory of the cancel,
	// would act on it. "Stop completely" has to survive a reload. Its position is
	// a sequence number taken now, which every frame published from here on
	// sorts after.
	if lastEventID <= 0 {
		return wh.nextSeqLocked(), nil, nil
	}
	var replay [][]byte
	for _, t := range c.terminal {
		if t.seq > lastEventID {
			replay = append(replay, t.frame)
		}
	}
	return lastEventID, replay, nil
}

func (wh *wireHub) remove(sub *wireSub) {
	wh.mu.Lock()
	if sub.removed {
		wh.mu.Unlock()
		return
	}
	sub.removed = true
	if c, ok := wh.chans[sub.key]; ok {
		wh.removeFromChannelLocked(c, sub.key, sub)
	}
	wh.sweepLocked()
	wh.mu.Unlock()
	sub.stop()
}

func (wh *wireHub) removeFromChannelLocked(c *wireChannel, key string, sub *wireSub) {
	delete(c.subs, sub)
	if c.handler == sub {
		c.handler = nil
	}
	// A channel with no subscribers and no retained outcomes is not a channel.
	// Dropping it keeps the map bounded by live use rather than by history.
	if len(c.subs) == 0 && len(c.terminal) == 0 {
		delete(wh.chans, key)
	}
}

// retainTerminalLocked records one request's outcome and enforces both the
// channel's own count cap and the hub-wide byte cap.
func (wh *wireHub) retainTerminalLocked(c *wireChannel, id string, t wireTerminal) {
	c.terminal[id] = t
	wh.terminalBytes += len(t.frame)
	wh.expireLocked(c)
	wh.enforceTerminalBytesLocked()
}

func (wh *wireHub) dropTerminalLocked(c *wireChannel, id string) {
	if t, ok := c.terminal[id]; ok {
		wh.terminalBytes -= len(t.frame)
		delete(c.terminal, id)
	}
}

// enforceTerminalBytesLocked drops the oldest retained outcome anywhere in the hub
// until the total is back under the ceiling.
func (wh *wireHub) enforceTerminalBytesLocked() {
	for wh.terminalBytes > wireGlobalMaxTerminalBytes {
		var victim *wireChannel
		var victimKey, oldestID string
		var oldest time.Time
		for key, c := range wh.chans {
			for id, t := range c.terminal {
				if oldestID == "" || t.at.Before(oldest) {
					victim, victimKey, oldestID, oldest = c, key, id, t.at
				}
			}
		}
		if victim == nil {
			return
		}
		wh.dropTerminalLocked(victim, oldestID)
		if len(victim.subs) == 0 && len(victim.terminal) == 0 {
			delete(wh.chans, victimKey)
		}
	}
}

// sweepLocked expires every channel's retained terminals and drops the channels
// left with nothing.
//
// Per-channel expiry alone is not enough to bound the map: a channel keeps itself
// alive while it holds terminals, and nothing revisits a file whose handler has
// gone, so one channel per file that ever saw a request would live as long as the
// site. There is no janitor goroutine because there is nothing to do between
// operations: sweeping on subscribe and unsubscribe is O(channels), and the
// channel count is bounded by the files a user has actually pointed an agent at.
func (wh *wireHub) sweepLocked() {
	for key, c := range wh.chans {
		wh.expireLocked(c)
		if len(c.subs) == 0 && len(c.terminal) == 0 {
			delete(wh.chans, key)
		}
	}
	wh.enforceTerminalBytesLocked()
}

func (wh *wireHub) expireLocked(c *wireChannel) {
	cutoff := time.Now().Add(-wireTerminalTTL)
	for id, t := range c.terminal {
		if t.at.Before(cutoff) {
			wh.dropTerminalLocked(c, id)
		}
	}
	// TTL alone permits an unbounded burst inside one window, so the count cap is
	// enforced too, dropping the oldest first.
	for len(c.terminal) > maxWireTerminals {
		var oldestID string
		var oldest time.Time
		for id, t := range c.terminal {
			if oldestID == "" || t.at.Before(oldest) {
				oldestID, oldest = id, t.at
			}
		}
		wh.dropTerminalLocked(c, oldestID)
	}
}

// publish fans one envelope out and reports how many HANDLER queues took it.
//
// The handler count is what the page actually asked: "is an agent there". A count
// of successful subscriber writes would answer yes because the page's own
// observer stream took a copy of its own request, which is precisely the bug the
// Node router shipped.
func (wh *wireHub) publish(key string, env wireEnvelope) (handlers int, observers int) {
	wh.mu.Lock()
	c, ok := wh.chans[key]
	if !ok || wh.closed {
		wh.mu.Unlock()
		return 0, 0
	}
	seq := wh.nextSeqLocked()
	f := frame(seq, env)
	if f == nil {
		wh.mu.Unlock()
		return 0, 0
	}
	if env.isTerminal() && env.ID != "" {
		// First terminal wins: a handler that reports done and then errors for one
		// request does not get to rewrite the outcome a subscriber already saw.
		if _, seen := c.terminal[env.ID]; !seen {
			wh.retainTerminalLocked(c, env.ID, wireTerminal{seq: seq, frame: f, at: time.Now()})
		}
	}
	var evicted []*wireSub
	for sub := range c.subs {
		select {
		case sub.ch <- f:
			if sub.handler {
				handlers++
			} else {
				observers++
			}
		default:
			evicted = append(evicted, sub)
		}
	}
	for _, sub := range evicted {
		sub.removed = true
		wh.removeFromChannelLocked(c, key, sub)
	}
	wh.mu.Unlock()

	// Unblocking the writers happens outside the lock, so a stuck connection
	// cannot hold up the next publish.
	for _, sub := range evicted {
		sub.stop()
	}
	return handlers, observers
}

func (wh *wireHub) shutdown() {
	wh.mu.Lock()
	if wh.closed {
		wh.mu.Unlock()
		return
	}
	wh.closed = true
	close(wh.closing)
	subs := make([]*wireSub, 0, len(wh.chans))
	for _, c := range wh.chans {
		for sub := range c.subs {
			sub.removed = true
			subs = append(subs, sub)
		}
	}
	wh.chans = make(map[string]*wireChannel)
	wh.terminalBytes = 0
	wh.mu.Unlock()
	for _, sub := range subs {
		sub.stop()
	}
}

// wireCaller classifies who is asking and whether to admit them.
//
// A browser attests at least one of Sec-Fetch-Site or Origin on every fetch and
// every EventSource and cannot forge either; a local process attests neither.
// That is the whole classifier, and it is why the wire needs no secret and no
// custom header: a local process runs as the user and needs no confused deputy.
//
// Origin is checked only WHEN PRESENT, never required. Chrome omits it on
// same-origin GETs, including EventSource's stream GET, and requiring it is what
// silently 403'd every live-sync stream from v1.3.0 until v1.5.0. The subscribe
// route below is the same kind of GET and would break identically.
//
// same-site is rejected rather than admitted: htmlclay serves one loopback origin
// per project tree and every one of them is same-site with every other, so
// admitting it would let one project's page drive another project's wire.
//
// The isBrowser answer is RETURNED rather than discarded, because it is what
// makes the handler role safe. See handleWireSubscribe.
func wireCaller(r *http.Request) (isBrowser, ok bool) {
	site := r.Header.Get("Sec-Fetch-Site")
	origin := r.Header.Get("Origin")
	if site == "" && origin == "" {
		return false, true
	}
	if site != "" && site != "same-origin" {
		return true, false
	}
	if origin != "" && origin != "http://"+r.Host {
		return true, false
	}
	return true, true
}

func wireGuard(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := wireCaller(r); !ok {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// wireMux mounts both routes as one subtree behind one guard, so a route cannot
// be added to the wire without the guard in front of it. The alternative
// considered was giving every /_/ route a declared reach; that rewrites route
// registrations the wire does not touch, for a property only the wire needs.
func (s *Server) wireMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_/wire/subscribe", s.handleWireSubscribe)
	mux.HandleFunc("POST /_/wire/send", s.handleWireSend)
	return wireGuard(mux)
}

// wireTarget resolves which file this request is about.
//
// The two callers are not symmetric and must not be merged. A browser NEVER names
// a path: its file comes from the page's own URL, through the same funnel
// handleLiveSyncSave uses, and any supplied file field is discarded. A local
// process has no page, so it names an absolute path, which is then validated.
//
// Note that origin-wide trust is inherited here, not introduced: resolvePageURL
// already lets one served page name another registered path on the same origin
// (livesync.go). So a page can open a wire on a sibling file of its own project.
// That is the existing model; "one wire per file" is addressing, not isolation.
func (s *Server) wireTarget(r *http.Request, isBrowser bool, supplied string) (*session.File, bool) {
	if isBrowser {
		// Document-URL first, Page-URL after it, and the query last. §3 names the
		// first spelling and §11's channel addresses a document the same way the save
		// route does, so a client written against the spec sends only that one and was
		// getting no target at all here. The older spelling stays because a document
		// that opened a wire before the rename hardcoded it in its own inline script.
		pageURL := r.Header.Get("Document-URL")
		if pageURL == "" {
			pageURL = r.Header.Get("Page-URL")
		}
		if pageURL == "" {
			pageURL = r.URL.Query().Get("document-url")
		}
		if pageURL == "" {
			pageURL = r.URL.Query().Get("page-url")
		}
		return s.resolvePageURL(r, pageURL)
	}
	return s.resolveWireFile(supplied)
}

// resolveWireFile admits an absolute path from a local process.
//
// ValidatePath cannot be called on it directly: that helper takes a URL-relative
// path and rejects anything starting with "/", so the address is made relative to
// the home directory first and then runs the same containment it does.
//
// Ordering is the serve path's discipline: containment and internal/hidden
// refusal are string and memory work and happen FIRST, so an out-of-scope path is
// refused identically whether or not anything exists at it. Only a path already
// registered in this site is admitted. Admitting an unregistered path under a
// live trusted anchor is the subscribe-before-open case; it needs the site's own
// anchor to compare against (answering "is it under ANY trusted folder" would let
// one project's origin open a wire into another's) and is deliberately not built
// here yet.
func (s *Server) resolveWireFile(raw string) (*session.File, bool) {
	if raw == "" || !filepath.IsAbs(raw) || strings.Contains(raw, "\x00") {
		return nil, false
	}
	home := s.sessions.HomeDir()
	canonical, ok := session.ContainWithinHome(home, filepath.Clean(raw))
	if !ok || s.isInternal(canonical) || session.HasHiddenComponent(home, canonical) {
		return nil, false
	}
	return s.sessions.LookupByPath(canonical)
}

// openHistoryForHandler turns version history on for a file an agent is about to
// start editing, and tells the watcher what is already there.
//
// The history key is resolved HERE rather than in the watcher, through the same
// seam serving uses. versions.Backup needs a key, a key is only ever set by
// serving or saving, and the handler lease exists precisely to watch files no tab
// has served, where the key is still empty. Attaching an agent therefore becomes
// the act that turns version history on for a file nobody has opened, with a
// baseline backup of what was there first, so the agent's own first write is not
// the earliest state history remembers. A freshly minted identity is marked
// provisional exactly as a first-open snapshot is, so attaching an agent to a file
// that then never changes does not permanently claim an id for it.
//
// Only lastStableObservation is seeded, and only when nothing has observed the
// file yet. Marking the file OBSERVED here would let a wire subscription do what
// the watcher is structurally forbidden from doing (session.go derives Observed
// from lastServerWrite for that reason), and the file's first real GET would then
// skip both its first-open snapshot and the seeding that makes its first save
// comparable. Overwriting an existing observation would suppress a change already
// waiting to be published.
//
// It runs BEFORE the lease is raised, so the watcher's first poll never sees a
// file with no observation on record and reports its existing content as a change.
func (s *Server) openHistoryForHandler(f *session.File) error {
	// Bulk pruning takes the store lock, and the lock order is file lock before
	// store lock, so it runs only once the seeding has released f.Lock.
	key, err := s.seedHandlerHistory(f)
	if key != "" {
		s.versions.MaybePrune(key, f.AbsPath)
	}
	return err
}

func (s *Server) seedHandlerHistory(f *session.File) (string, error) {
	f.Lock()
	defer f.Unlock()

	data, err := os.ReadFile(f.AbsPath)
	if errors.Is(err, fs.ErrNotExist) {
		// A registered file can be gone by now, and the agent may be about to put it
		// back. There are no bytes to seed an observation from and no embedded id to
		// adopt, so the identity resolves from the path alone. Doing that here is
		// what lets the watcher version the agent's very first write.
		s.ensureHistoryKeyLocked(f, nil)
		return "", nil
	}
	if err != nil {
		// An existing file this server cannot read is a refusal, not a degraded
		// attach. Its bytes may carry an id a later serve should adopt, so minting
		// one now would fork its history, and leaving the key unresolved would mean
		// versioning nothing the agent writes.
		return "", err
	}
	key, provisional := s.ensureHistoryKeyLocked(f, data)
	if f.LastStableObservation() == "" {
		f.RecordStableObservation(versions.Hash(data))
	}
	if _, bErr := s.versions.Backup(key, f.AbsPath, data); bErr != nil {
		s.logger.Printf("Baseline backup for wire handler on %s failed: %v", f.RelPath, bErr)
	} else if provisional {
		if pErr := s.versions.SetProvisional(key, f.AbsPath, true); pErr != nil {
			s.logger.Printf("Could not mark provisional history for %s: %v", f.RelPath, pErr)
		}
	}
	return key, nil
}

func (s *Server) handleWireSubscribe(w http.ResponseWriter, r *http.Request) {
	isBrowser, _ := wireCaller(r)

	// Pages send; processes handle. A page allowed to hold the exclusive slot could
	// take it away from the user's agent, receive every request on this file
	// including other tabs', and answer them with fabricated terminal frames.
	//
	// KNOWN RESIDUAL, and the classifier cannot close it: a browser that predates
	// Sec-Fetch-Site attests nothing at all on a same-origin GET (Safari before
	// 16.4, Firefox before 90), so wireCaller reads such a page as a process. What
	// it buys is squatting the slot and lying to its own origin's tabs; it is
	// already allowed to send frames, it cannot read or write any file, and it
	// cannot reach another project's origin. No header fixes this, because a
	// same-origin fetch may set any header without a preflight; only a secret a
	// page cannot read would, and that is a bigger change than the residual is
	// worth. A cross-origin caller is never affected: every cross-origin request,
	// EventSource included, carries Origin.
	//
	// Refused before the file is resolved: the answer does not depend on which
	// file, so resolving first would tell a caller that is being refused anyway
	// whether a given path exists. A page therefore never sees the 409 below, which
	// is just as well, since it could do nothing with a conflict over a slot it may
	// never hold.
	wantHandler := r.URL.Query().Get("role") == "handler"
	if wantHandler && isBrowser {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	f, ok := s.wireTarget(r, isBrowser, r.URL.Query().Get("file"))
	if !ok {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}

	rc := http.NewResponseController(w)
	// Clear the write deadline for this connection only; zeroing the server-wide
	// WriteTimeout would remove the bound from every other request.
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		s.logger.Printf("wire: cannot clear write deadline: %v", err)
		http.Error(w, "Not Implemented", http.StatusNotImplemented)
		return
	}

	sub := &wireSub{
		key:     f.AbsPath,
		handler: wantHandler,
		ch:      make(chan []byte, wireQueueSize),
		done:    make(chan struct{}),
	}

	cursor, replay, err := s.wire.add(sub, parseLastEventID(r))
	if err != nil {
		switch {
		case errors.Is(err, errWireHandlerTaken):
			http.Error(w, "Conflict", http.StatusConflict)
		case errors.Is(err, errWireBusy):
			http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
		default:
			http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		}
		return
	}
	defer s.wire.remove(sub)

	// The watch is the whole point of the handler role. An agent edits the FILE, and
	// the edit reaches the page through the ordinary external-change path, which only
	// runs for files the watcher is polling; without the lease an agent would be
	// useful only while a tab happened to already be open on the file it was editing.
	//
	// Raised after the handler slot is won, so a refused subscription leaves no lease
	// behind, and released on the same teardown as the subscription itself. The raise
	// and its defer sit together with nothing between them, so no failure inside the
	// seeding above can leak a reference.
	if wantHandler {
		// Refused rather than degraded. Attaching without a resolved key would start
		// an agent whose every write goes unversioned until someone happens to open
		// the file in a browser, and the one state worth keeping is the one that
		// existed before the agent touched it.
		if err := s.openHistoryForHandler(f); err != nil {
			s.logger.Printf("wire: cannot open history for %s: %v", f.RelPath, err)
			http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
			return
		}
		s.coord.lease(f)
		defer s.coord.unlease(f)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		return
	}

	// A page's first frame is a cursor: a named event that never reaches
	// onmessage, carrying the position it resumes from. It is the floor of what
	// follows: the replay below and every live frame sort after it, so a stream
	// that drops between the cursor and a frame resumes with the frame still
	// owed. Pages only, because a process tailing the wire parses every data line
	// as a wire frame and has no use for a position it never presents.
	if isBrowser && !wantHandler {
		if !writeSSE(rc, w, cursorFrame(cursor, false)) {
			return
		}
	}

	for _, fr := range replay {
		if !writeSSE(rc, w, fr) {
			return
		}
	}

	ticker := time.NewTicker(keepaliveInterval)
	defer ticker.Stop()

	// This goroutine is the single writer for this connection, so keepalives and
	// broadcasts never write concurrently to one ResponseWriter and the hub lock
	// is never held during a network write. A wire is idle between requests by
	// design, so the keepalive is what stops IdleTimeout from closing it.
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.wire.closing:
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

func (s *Server) handleWireSend(w http.ResponseWriter, r *http.Request) {
	isBrowser, _ := wireCaller(r)

	// application/json is load-bearing rather than hygiene: it is not a
	// CORS-simple content type, so a cross-origin POST is forced into a preflight
	// that this mux answers with a bare 405 and no allow-origin header. The matcher
	// also accepts the `+json` suffix, which costs nothing here: the three simple
	// content types are form-urlencoded, multipart, and text/plain, so every type it
	// accepts is still preflighted.
	if !isJSONContentType(r.Header.Get("Content-Type")) {
		s.writeError(w, http.StatusUnsupportedMediaType, "expected application/json")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxWireBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			s.writeError(w, http.StatusRequestEntityTooLarge, "wire frame too large")
			return
		}
		s.writeError(w, http.StatusBadRequest, "could not read body")
		return
	}

	var env wireEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	// The router validates that a type is a wire type and that an id exists and is
	// bounded. It does not validate against a closed set of types: the parity
	// implementation already emits a batched delta type, and a router that never
	// reads payloads has no business rejecting a type it does not recognize.
	if !strings.HasPrefix(env.Type, "wire/") || len(env.Type) > 64 {
		s.writeError(w, http.StatusBadRequest, "invalid type")
		return
	}
	if env.ID == "" || len(env.ID) > maxWireIDLen {
		s.writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if len(env.Text) > maxWireText {
		env.Text = env.Text[:maxWireText]
	}

	f, ok := s.wireTarget(r, isBrowser, env.File)
	if !ok {
		s.writeError(w, http.StatusNotFound, "unknown file")
		return
	}

	// From is a coarse origin tag, not a connection identity. Correlation is by
	// the sender's own opaque id, and a subscriber drops ids it did not issue, so
	// nothing needs to tell two pages apart. Stamping it server-side keeps it
	// honest: a value the sender chose is a value a hostile sender can choose to
	// be someone else's.
	env.V = 1
	env.File = f.AbsPath
	env.From = "process"
	if isBrowser {
		env.From = "page"
	}

	// A handler that reports a request finished has finished writing the file, so
	// the change is already on disk and the quiet interval has nothing left to
	// wait for. Without the poke the page's spinner ends on this frame and the
	// text arrives up to a poll plus a quiet interval later.
	//
	// Only from a process: a page has no way to know a write finished, and a
	// terminal frame is the one thing it could send to claim so. An error is
	// terminal too, and a handler that gave up has also stopped writing.
	if env.isTerminal() && !isBrowser {
		s.coord.pokeWatcher(f)
	}

	handlers, observers := s.wire.publish(f.AbsPath, env)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok":        true,
		"delivered": handlers,
		"observers": observers,
	})
}
