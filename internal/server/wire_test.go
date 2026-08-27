package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/panphora/htmlclay/internal/session"
	"github.com/panphora/htmlclay/internal/versions"
)

func wireSendReq(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/_/wire/send", strings.NewReader(body))
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.wireMux().ServeHTTP(w, req)
	return w
}

// A local process attests no browser headers at all. That is the whole basis for
// admitting it, so it is the first thing worth pinning.
func TestWireAdmitsHeaderFreeProcess(t *testing.T) {
	srv, f := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })

	body := fmt.Sprintf(`{"type":"wire/request","id":"a1","file":%q}`, f.AbsPath)
	w := wireSendReq(t, srv, body)
	if w.Code != 200 {
		t.Fatalf("header-free process rejected: %d %s", w.Code, w.Body.String())
	}
}

func TestWireGuardRejectsCrossOriginAndSameSite(t *testing.T) {
	srv, f := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })
	body := fmt.Sprintf(`{"type":"wire/request","id":"a1","file":%q}`, f.AbsPath)

	cases := []struct {
		name string
		site string
		orig string
	}{
		// Every loopback origin is same-site with every other, so admitting
		// same-site would let one project's page drive another project's wire.
		{"same-site", "same-site", ""},
		{"cross-site", "cross-site", ""},
		{"foreign origin", "same-origin", "http://evil.example"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/_/wire/send", strings.NewReader(body))
			req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
			req.Header.Set("Content-Type", "application/json")
			if tc.site != "" {
				req.Header.Set("Sec-Fetch-Site", tc.site)
			}
			if tc.orig != "" {
				req.Header.Set("Origin", tc.orig)
			}
			w := httptest.NewRecorder()
			srv.wireMux().ServeHTTP(w, req)
			if w.Code != 403 {
				t.Fatalf("expected 403, got %d", w.Code)
			}
		})
	}
}

// Origin must never be REQUIRED: Chrome omits it on same-origin GETs, including
// EventSource's stream GET. Requiring it is what 403'd every live-sync stream
// from v1.3.0 to v1.5.0, and the subscribe route is the same kind of GET.
func TestWireAdmitsSameOriginWithoutOriginHeader(t *testing.T) {
	req := httptest.NewRequest("GET", "/_/wire/subscribe", nil)
	req.Host = "127.0.0.1:4321"
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	isBrowser, ok := wireCaller(req)
	if !ok || !isBrowser {
		t.Fatalf("same-origin GET without Origin must be admitted as a browser; got ok=%v browser=%v", ok, isBrowser)
	}
}

// Pages send, processes handle. A page holding the exclusive slot could take it
// from the user's agent forever, see every request on the file including other
// tabs', and answer with fabricated terminal frames.
func TestWirePageCannotTakeHandlerRole(t *testing.T) {
	srv, f := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })

	req := httptest.NewRequest("GET", "/_/wire/subscribe?role=handler&file="+f.AbsPath, nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req = sameOriginHeaders(req)
	w := httptest.NewRecorder()
	srv.wireMux().ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("a page took the handler role: %d", w.Code)
	}
}

func TestWireHandlerSlotIsExclusive(t *testing.T) {
	wh := newWireHub()
	t.Cleanup(wh.shutdown)

	first := &wireSub{key: "/f.htmlclay", handler: true, ch: make(chan []byte, 4), done: make(chan struct{})}
	if _, err := wh.add(first, 0); err != nil {
		t.Fatalf("first handler refused: %v", err)
	}
	second := &wireSub{key: "/f.htmlclay", handler: true, ch: make(chan []byte, 4), done: make(chan struct{})}
	if _, err := wh.add(second, 0); err != errWireHandlerTaken {
		t.Fatalf("second handler admitted: %v", err)
	}
	// An observer alongside a handler is fine; tailing must not need the slot.
	obs := &wireSub{key: "/f.htmlclay", ch: make(chan []byte, 4), done: make(chan struct{})}
	if _, err := wh.add(obs, 0); err != nil {
		t.Fatalf("observer refused: %v", err)
	}
	// Releasing the slot lets the next handler in, so a reconnect is not
	// permanently locked out by its own predecessor.
	wh.remove(first)
	third := &wireSub{key: "/f.htmlclay", handler: true, ch: make(chan []byte, 4), done: make(chan struct{})}
	if _, err := wh.add(third, 0); err != nil {
		t.Fatalf("handler slot not released: %v", err)
	}
}

// "delivered" answers "is an agent there". Counting subscriber writes would
// answer yes because the page's own observer stream takes a copy of the page's
// own request.
func TestWireDeliveredCountsHandlersOnly(t *testing.T) {
	wh := newWireHub()
	t.Cleanup(wh.shutdown)
	key := "/f.htmlclay"

	page := &wireSub{key: key, ch: make(chan []byte, 4), done: make(chan struct{})}
	if _, err := wh.add(page, 0); err != nil {
		t.Fatal(err)
	}
	handlers, observers := wh.publish(key, wireEnvelope{Type: "wire/request", ID: "r1"})
	if handlers != 0 {
		t.Fatalf("no agent attached but delivered=%d", handlers)
	}
	if observers != 1 {
		t.Fatalf("page observer did not receive its own request: %d", observers)
	}

	agent := &wireSub{key: key, handler: true, ch: make(chan []byte, 4), done: make(chan struct{})}
	if _, err := wh.add(agent, 0); err != nil {
		t.Fatal(err)
	}
	handlers, observers = wh.publish(key, wireEnvelope{Type: "wire/request", ID: "r2"})
	if handlers != 1 || observers != 1 {
		t.Fatalf("want handlers=1 observers=1, got %d/%d", handlers, observers)
	}
}

// A fresh subscription replays nothing. A page reloaded after cancelling a
// request has no memory of the cancel, so a replayed terminal frame would
// resurrect it and "stop completely" would not survive a reload.
func TestWireReplayOnlyAboveLastEventID(t *testing.T) {
	wh := newWireHub()
	t.Cleanup(wh.shutdown)
	key := "/f.htmlclay"

	seed := &wireSub{key: key, ch: make(chan []byte, 8), done: make(chan struct{})}
	if _, err := wh.add(seed, 0); err != nil {
		t.Fatal(err)
	}
	wh.publish(key, wireEnvelope{Type: "wire/status", ID: "r1", Text: "working"})
	wh.publish(key, wireEnvelope{Type: "wire/done", ID: "r1"})

	fresh := &wireSub{key: key, ch: make(chan []byte, 8), done: make(chan struct{})}
	replay, err := wh.add(fresh, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(replay) != 0 {
		t.Fatalf("fresh subscription replayed %d frames", len(replay))
	}

	resumed := &wireSub{key: key, ch: make(chan []byte, 8), done: make(chan struct{})}
	replay, err = wh.add(resumed, 1)
	if err != nil {
		t.Fatal(err)
	}
	// Only the terminal frame is retained; a lost status is repaired by the next
	// frame and a lost terminal is repaired by nothing.
	if len(replay) != 1 {
		t.Fatalf("want 1 retained terminal, got %d", len(replay))
	}
	if !strings.Contains(string(replay[0]), "wire/done") {
		t.Fatalf("retained the wrong frame: %s", replay[0])
	}
}

func TestWireFirstTerminalWins(t *testing.T) {
	wh := newWireHub()
	t.Cleanup(wh.shutdown)
	key := "/f.htmlclay"
	sub := &wireSub{key: key, ch: make(chan []byte, 8), done: make(chan struct{})}
	if _, err := wh.add(sub, 0); err != nil {
		t.Fatal(err)
	}
	wh.publish(key, wireEnvelope{Type: "wire/done", ID: "r1"})
	wh.publish(key, wireEnvelope{Type: "wire/error", ID: "r1", Text: "too late"})

	resumed := &wireSub{key: key, ch: make(chan []byte, 8), done: make(chan struct{})}
	replay, _ := wh.add(resumed, 1)
	if len(replay) != 1 || !strings.Contains(string(replay[0]), "wire/done") {
		t.Fatalf("a handler rewrote an outcome a subscriber already saw: %q", replay)
	}
}

// The page must never author the path: a page naming ~/.zshrc would launder a
// write through the agent's OS authority into a file the page can never touch.
func TestWireSendStampsCanonicalFile(t *testing.T) {
	srv, f := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })

	tap := &wireSub{key: f.AbsPath, ch: make(chan []byte, 4), done: make(chan struct{})}
	if _, err := srv.wire.add(tap, 0); err != nil {
		t.Fatal(err)
	}

	// A process names its own file; the server still stamps the canonical value.
	body := fmt.Sprintf(`{"type":"wire/request","id":"a1","file":%q,"from":"spoofed"}`, f.AbsPath)
	if w := wireSendReq(t, srv, body); w.Code != 200 {
		t.Fatalf("send failed: %d %s", w.Code, w.Body.String())
	}

	select {
	case raw := <-tap.ch:
		payload := raw[strings.Index(string(raw), "data: ")+6:]
		var env wireEnvelope
		if err := json.Unmarshal(payload, &env); err != nil {
			t.Fatalf("unparseable frame %q: %v", raw, err)
		}
		if env.File != f.AbsPath {
			t.Fatalf("file not stamped: %q", env.File)
		}
		if env.From == "spoofed" {
			t.Fatal("client-supplied from survived")
		}
	default:
		t.Fatal("subscriber received nothing")
	}
}

func TestWireSendRejectsBadFrames(t *testing.T) {
	srv, f := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })

	cases := []struct {
		name string
		body string
		want int
	}{
		{"non-wire type", fmt.Sprintf(`{"type":"evil/x","id":"a","file":%q}`, f.AbsPath), 400},
		{"missing id", fmt.Sprintf(`{"type":"wire/request","file":%q}`, f.AbsPath), 400},
		{"unknown file", `{"type":"wire/request","id":"a","file":"/nope/missing.htmlclay"}`, 404},
		{"relative path", `{"type":"wire/request","id":"a","file":"page.htmlclay"}`, 404},
		{"invalid json", `{`, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if w := wireSendReq(t, srv, tc.body); w.Code != tc.want {
				t.Fatalf("want %d, got %d (%s)", tc.want, w.Code, w.Body.String())
			}
		})
	}
}

// application/json is not a CORS-simple content type, so requiring it forces a
// cross-origin POST into a preflight this mux never answers.
func TestWireSendRequiresJSONContentType(t *testing.T) {
	srv, f := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })

	body := fmt.Sprintf(`{"type":"wire/request","id":"a","file":%q}`, f.AbsPath)
	req := httptest.NewRequest("POST", "/_/wire/send", strings.NewReader(body))
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	srv.wireMux().ServeHTTP(w, req)
	if w.Code != 415 {
		t.Fatalf("want 415, got %d", w.Code)
	}
}

// A GET to the send route must not reach the wire at all. With method patterns it
// falls through the subtree mux to a 405, which is the correct answer and keeps an
// <img src> probe from a foreign page away from the handler.
func TestWireSendRejectsGET(t *testing.T) {
	srv, _ := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })

	req := httptest.NewRequest("GET", "/_/wire/send", nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	w := httptest.NewRecorder()
	srv.wireMux().ServeHTTP(w, req)
	if w.Code == 200 {
		t.Fatal("GET reached the send handler")
	}
}

// Bounded queues evict rather than grow, matching the live-sync hub. A subscriber
// that cannot keep up loses its stream instead of the server losing its memory.
func TestWireEvictsSlowSubscriber(t *testing.T) {
	wh := newWireHub()
	t.Cleanup(wh.shutdown)
	key := "/f.htmlclay"

	slow := &wireSub{key: key, ch: make(chan []byte, 1), done: make(chan struct{})}
	if _, err := wh.add(slow, 0); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		wh.publish(key, wireEnvelope{Type: "wire/status", ID: "r1", Text: "x"})
	}
	select {
	case <-slow.done:
	default:
		t.Fatal("slow subscriber was not evicted")
	}
}

func TestWireSubscriberCapIsEnforced(t *testing.T) {
	wh := newWireHub()
	t.Cleanup(wh.shutdown)
	key := "/f.htmlclay"

	for i := 0; i < maxWireSubs; i++ {
		sub := &wireSub{key: key, ch: make(chan []byte, 2), done: make(chan struct{})}
		if _, err := wh.add(sub, 0); err != nil {
			t.Fatalf("subscriber %d refused early: %v", i, err)
		}
	}
	over := &wireSub{key: key, ch: make(chan []byte, 2), done: make(chan struct{})}
	if _, err := wh.add(over, 0); err != errWireBusy {
		t.Fatalf("cap not enforced: %v", err)
	}
}

// The lease is what makes an attached agent useful at all: it edits the FILE, and
// the edit reaches a page through the ordinary external-change path, which only
// runs for files the watcher polls.
func TestWireHandlerLeaseWatchesAndVersionsAnUnopenedFile(t *testing.T) {
	srv, f := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })

	f.Lock()
	before := f.HistoryKey()
	f.Unlock()
	if before != "" {
		t.Fatalf("fixture was already served: key %q", before)
	}

	if err := srv.openHistoryForHandler(f); err != nil {
		t.Fatal(err)
	}
	srv.coord.lease(f)

	if !watchedPath(srv.watcher, f.AbsPath) {
		t.Fatal("the handler lease did not make the file watched")
	}

	f.Lock()
	key, stable, observed := f.HistoryKey(), f.LastStableObservation(), f.Observed()
	f.Unlock()

	if key == "" {
		t.Fatal("no history key was resolved, so an external change cannot be versioned")
	}
	disk, err := os.ReadFile(f.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	if stable != versions.Hash(disk) {
		t.Fatal("lastStableObservation was not seeded, so attaching would look like a change")
	}
	// Marking the file observed here would let a wire subscription do what the
	// watcher is structurally forbidden from doing, and the first real GET would
	// then skip both its first-open snapshot and its seeding.
	if observed {
		t.Fatal("the lease marked the file observed")
	}

	entries, err := srv.versions.List(key, f.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("want one baseline backup, got %d", len(entries))
	}
	baseline, err := srv.versions.Read(key, f.AbsPath, entries[0].Name)
	if err != nil {
		t.Fatal(err)
	}
	if string(baseline) != string(disk) {
		t.Fatal("the baseline backup is not what was on disk when the agent attached")
	}

	srv.coord.unlease(f)
	if watchedPath(srv.watcher, f.AbsPath) {
		t.Fatal("the lease outlived the handler")
	}
}

// Refcounted through the same watch/unwatch pair as a stream, so a handler
// reconnecting while the previous stream is still tearing down does not drop the
// entry out from under itself.
func TestWireHandlerLeaseIsRefcounted(t *testing.T) {
	srv, f := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })

	srv.coord.lease(f)
	srv.coord.lease(f)
	srv.coord.unlease(f)
	if !watchedPath(srv.watcher, f.AbsPath) {
		t.Fatal("one release dropped a doubly-held lease")
	}
	srv.coord.unlease(f)
	if watchedPath(srv.watcher, f.AbsPath) {
		t.Fatal("the last release did not drop the lease")
	}
}

// waitWatched polls because a cancelled SSE handler unwinds on its own goroutine.
func waitWatched(t *testing.T, wt *watcher, path string, want bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if watchedPath(wt, path) == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("watch lease never became %v", want)
}

// The subscribe route needs a real connection: it clears the write deadline for
// its own stream, which a ResponseRecorder cannot do, and answers 501 rather than
// serving an SSE stream it cannot bound.
func wireSubscribeURL(ts *httptest.Server, f *session.File, role string) string {
	q := url.Values{"file": {f.AbsPath}}
	if role != "" {
		q.Set("role", role)
	}
	return ts.URL + "/_/wire/subscribe?" + q.Encode()
}

// A handler subscription raises the lease and its teardown releases it, over the
// real route rather than the helper.
func TestWireSubscribeRaisesAndReleasesTheLease(t *testing.T) {
	srv, f := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })
	ts := httptest.NewServer(srv.wireMux())
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", wireSubscribeURL(ts, f, "handler"), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		cancel()
		t.Fatalf("handler subscription refused: %d", resp.StatusCode)
	}
	waitWatched(t, srv.watcher, f.AbsPath, true)

	cancel()
	resp.Body.Close()
	waitWatched(t, srv.watcher, f.AbsPath, false)
}

// A refused subscription must leave nothing behind. The second handler never
// holds the slot, so it must not hold a watch either.
func TestWireRefusedHandlerLeavesNoLease(t *testing.T) {
	srv, f := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })
	ts := httptest.NewServer(srv.wireMux())
	t.Cleanup(ts.Close)

	held := &wireSub{key: f.AbsPath, handler: true, ch: make(chan []byte, 4), done: make(chan struct{})}
	if _, err := srv.wire.add(held, 0); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(wireSubscribeURL(ts, f, "handler"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 409 {
		t.Fatalf("want 409 for a taken slot, got %d", resp.StatusCode)
	}
	if watchedPath(srv.watcher, f.AbsPath) {
		t.Fatal("a refused handler left a watch lease behind")
	}
}

// The exclusive slot is reserved, not competed for: tails on a file must not be
// able to lock the user's own agent out of a slot that is free.
func TestWireObserverCapDoesNotLockOutTheHandler(t *testing.T) {
	wh := newWireHub()
	t.Cleanup(wh.shutdown)
	key := "/f.htmlclay"

	for i := 0; i < maxWireSubs; i++ {
		sub := &wireSub{key: key, ch: make(chan []byte, 2), done: make(chan struct{})}
		if _, err := wh.add(sub, 0); err != nil {
			t.Fatalf("observer %d refused early: %v", i, err)
		}
	}
	agent := &wireSub{key: key, handler: true, ch: make(chan []byte, 2), done: make(chan struct{})}
	if _, err := wh.add(agent, 0); err != nil {
		t.Fatalf("observers locked the agent out of a free slot: %v", err)
	}
}

// A channel keeps itself alive while it holds a retained outcome, and nothing
// revisits a file whose handler has gone. Without a sweep that is one channel per
// file ever touched, for the life of the site.
func TestWireChannelIsDroppedOnceItsTerminalsExpire(t *testing.T) {
	wh := newWireHub()
	t.Cleanup(wh.shutdown)
	key := "/f.htmlclay"

	sub := &wireSub{key: key, ch: make(chan []byte, 4), done: make(chan struct{})}
	if _, err := wh.add(sub, 0); err != nil {
		t.Fatal(err)
	}
	wh.publish(key, wireEnvelope{Type: "wire/done", ID: "r1"})
	wh.remove(sub)

	wh.mu.Lock()
	c, held := wh.chans[key]
	wh.mu.Unlock()
	if !held {
		t.Fatal("the outcome was dropped while a reconnect could still ask for it")
	}

	wh.mu.Lock()
	for id, term := range c.terminal {
		term.at = term.at.Add(-2 * wireTerminalTTL)
		c.terminal[id] = term
	}
	wh.mu.Unlock()

	// Any later hub operation sweeps, so the map is bounded by live use.
	other := &wireSub{key: "/g.htmlclay", ch: make(chan []byte, 4), done: make(chan struct{})}
	if _, err := wh.add(other, 0); err != nil {
		t.Fatal(err)
	}
	wh.remove(other)

	wh.mu.Lock()
	_, still := wh.chans[key]
	wh.mu.Unlock()
	if still {
		t.Fatal("a channel holding only expired terminals was never dropped")
	}
}

// A registered file can be gone by the time an agent attaches, and the agent may
// be about to put it back. Its identity resolves from the path alone, so the very
// first thing the agent writes is versioned rather than being the earliest state
// on record.
func TestWireHandlerLeaseResolvesHistoryForAMissingFile(t *testing.T) {
	srv, existing := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })

	gone := filepath.Join(filepath.Dir(existing.AbsPath), "gone.htmlclay")
	if err := os.WriteFile(gone, []byte("<html><body>here for now</body></html>"), 0644); err != nil {
		t.Fatal(err)
	}
	f, err := srv.sessions.Register(gone, session.ViaOsOpen)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}

	if err := srv.openHistoryForHandler(f); err != nil {
		t.Fatal(err)
	}

	f.Lock()
	key, stable := f.HistoryKey(), f.LastStableObservation()
	f.Unlock()
	if _, ok := versions.IDFromKey(key); !ok {
		t.Fatalf("no durable identity for a file the agent is about to create: %q", key)
	}
	if stable != "" {
		t.Fatal("a file with no bytes was given a stable observation")
	}
}

// The per-file caps bound one channel; a page may send on any registered path on
// its origin, so a loop over a tree would pin the per-file maximum for every file
// at once and no per-file limit would ever fire.
func TestWireTerminalRetentionIsBoundedAcrossChannels(t *testing.T) {
	wh := newWireHub()
	t.Cleanup(wh.shutdown)

	big := strings.Repeat("x", 64<<10)
	for file := 0; file < 40; file++ {
		key := fmt.Sprintf("/f%d.htmlclay", file)
		sub := &wireSub{key: key, ch: make(chan []byte, 64), done: make(chan struct{})}
		if _, err := wh.add(sub, 0); err != nil {
			t.Fatal(err)
		}
		for req := 0; req < maxWireTerminals; req++ {
			wh.publish(key, wireEnvelope{Type: "wire/done", ID: fmt.Sprintf("r%d", req), Text: big})
		}
		wh.remove(sub)
	}

	wh.mu.Lock()
	total, counted := wh.terminalBytes, 0
	for _, c := range wh.chans {
		for _, term := range c.terminal {
			counted += len(term.frame)
		}
	}
	wh.mu.Unlock()

	if total > wireGlobalMaxTerminalBytes {
		t.Fatalf("retained %d bytes, over the %d ceiling", total, wireGlobalMaxTerminalBytes)
	}
	if counted != total {
		t.Fatalf("running total %d disagrees with what is actually held, %d", total, counted)
	}
}

// The poke, end to end over the route: a process's terminal frame publishes the
// change it just wrote without waiting out the quiet interval, and a page's
// identical frame does not. The interval here is long enough that nothing below
// can publish by waiting.
func TestWireTerminalFrameFromAProcessPokesTheWatcher(t *testing.T) {
	srv, f := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })
	srv.watcher.poll = 10 * time.Millisecond
	srv.watcher.quiet = 30 * time.Second

	disk, err := os.ReadFile(f.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	f.Lock()
	f.RecordServerWrite(versions.Hash(disk))
	f.Unlock()

	sub := newSubscriber(f.AbsPath, laneSaved)
	srv.hub.add(sub)
	srv.coord.lease(f) // the shape a wire handler holds: watched, no tab open
	t.Cleanup(func() { srv.coord.unlease(f) })

	changed := "<!DOCTYPE html>\n<html><body>the agent wrote this</body></html>"
	if err := os.WriteFile(f.AbsPath, []byte(changed), 0644); err != nil {
		t.Fatal(err)
	}
	expectNoFrame(t, sub, 200*time.Millisecond)

	// A page cannot claim a write finished, so its terminal frame changes nothing.
	req := httptest.NewRequest("POST", "/_/wire/send", strings.NewReader(`{"type":"wire/done","id":"a1"}`))
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Page-URL", fmt.Sprintf("http://127.0.0.1:%d/%s", srv.port, filepath.Base(f.AbsPath)))
	w := httptest.NewRecorder()
	srv.wireMux().ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("page send failed: %d %s", w.Code, w.Body.String())
	}
	expectNoFrame(t, sub, 200*time.Millisecond)

	body := fmt.Sprintf(`{"type":"wire/done","id":"a2","file":%q}`, f.AbsPath)
	if w := wireSendReq(t, srv, body); w.Code != 200 {
		t.Fatalf("process send failed: %d %s", w.Code, w.Body.String())
	}

	frame := waitFrame(t, sub, 2*time.Second)
	if frame["html"] != changed {
		t.Fatalf("saved lane html = %v", frame["html"])
	}
}
