package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/panphora/htmlclay/logging"
	"github.com/panphora/htmlclay/session"
	"github.com/panphora/htmlclay/versions"
)

func newSubscriber(key, lane string) *subscriber {
	return &subscriber{
		key:  key,
		lane: lane,
		ch:   make(chan []byte, subQueueSize),
		done: make(chan struct{}),
	}
}

func waitFrame(t *testing.T, sub *subscriber, within time.Duration) map[string]interface{} {
	t.Helper()
	select {
	case raw := <-sub.ch:
		payload, ok := strings.CutPrefix(string(raw), "data: ")
		if !ok {
			t.Fatalf("frame is not an SSE data line: %q", raw)
		}
		var out map[string]interface{}
		if err := json.Unmarshal([]byte(strings.TrimSpace(payload)), &out); err != nil {
			t.Fatalf("frame is not valid JSON: %v (%q)", err, payload)
		}
		return out
	case <-time.After(within):
		t.Fatal("timed out waiting for a frame")
		return nil
	}
}

func expectNoFrame(t *testing.T, sub *subscriber, within time.Duration) {
	t.Helper()
	select {
	case raw := <-sub.ch:
		t.Fatalf("unexpected frame: %q", raw)
	case <-time.After(within):
	}
}

// The counter is seeded from wall-clock milliseconds. A counter restarting at 1
// after a server restart is rejected by the client's retained high-water mark,
// and the stream silently stops updating.
func TestSequenceSeededFromWallClock(t *testing.T) {
	before := time.Now().UnixMilli()
	h := newHub()
	after := time.Now().UnixMilli()

	if h.seq < before || h.seq > after {
		t.Fatalf("seq %d not seeded from wall clock (%d..%d)", h.seq, before, after)
	}

	sub := newSubscriber("/tmp/a.html", laneLive)
	h.add(sub)
	h.relay("/tmp/a.html", "<html></html>", "c1", nil)
	msg := waitFrame(t, sub, time.Second)
	if seq, _ := msg["seq"].(float64); int64(seq) < before {
		t.Fatalf("broadcast seq %v is below the startup seed %d", msg["seq"], before)
	}
}

// B3 and B4 share one counter. Every allocation, from either leg, is strictly
// increasing.
func TestSequenceIsSharedAndMonotonic(t *testing.T) {
	h := newHub()
	sub := newSubscriber("/tmp/a.html", laneSaved)
	h.add(sub)

	var last int64
	for i := 0; i < 20; i++ {
		if i%2 == 0 {
			h.broadcastSaved("/tmp/a.html", fmt.Sprintf("<html>%d</html>", i), "file-system")
		} else {
			h.publishExternalChange("/tmp/a.html", "changed", fmt.Sprintf("<html>%d</html>", i))
		}
		msg := waitFrame(t, sub, time.Second)
		seq := int64(msg["seq"].(float64))
		if seq <= last {
			t.Fatalf("seq did not advance: %d after %d", seq, last)
		}
		last = seq
	}
}

func TestRelayGoesToLiveLaneOnly(t *testing.T) {
	h := newHub()
	live := newSubscriber("/tmp/a.html", laneLive)
	saved := newSubscriber("/tmp/a.html", laneSaved)
	h.add(live)
	h.add(saved)

	h.relay("/tmp/a.html", "<html>peer</html>", "c1", json.RawMessage(`{"0":"x"}`))

	msg := waitFrame(t, live, time.Second)
	if msg["html"] != "<html>peer</html>" || msg["sender"] != "c1" {
		t.Fatalf("unexpected live payload: %v", msg)
	}
	if _, ok := msg["identityMap"]; !ok {
		t.Fatal("identityMap was dropped")
	}
	expectNoFrame(t, saved, 100*time.Millisecond)
}

// An external change notifies the live lane and does not push content there,
// because after B0 every tab holds unsaved DOM state. The stable disk HTML goes
// to the saved lane.
func TestExternalChangeNotifiesLiveAndBroadcastsSaved(t *testing.T) {
	h := newHub()
	live := newSubscriber("/tmp/a.html", laneLive)
	saved := newSubscriber("/tmp/a.html", laneSaved)
	h.add(live)
	h.add(saved)

	h.publishExternalChange("/tmp/a.html", "notes.htmlclay changed on disk", "<html>disk</html>")

	notice := waitFrame(t, live, time.Second)
	if notice["type"] != "notification" || notice["msgType"] != "warning" {
		t.Fatalf("live lane did not receive a warning notification: %v", notice)
	}
	if _, ok := notice["html"]; ok {
		t.Fatal("live lane received content, which would discard unsaved DOM state")
	}
	expectNoFrame(t, live, 100*time.Millisecond)

	content := waitFrame(t, saved, time.Second)
	if content["html"] != "<html>disk</html>" || content["sender"] != "file-system" {
		t.Fatalf("saved lane payload wrong: %v", content)
	}
}

func TestSlowSubscriberIsEvictedAndUnblocked(t *testing.T) {
	h := newHub()
	sub := newSubscriber("/tmp/a.html", laneLive)
	h.add(sub)

	for i := 0; i < subQueueSize+5; i++ {
		h.relay("/tmp/a.html", fmt.Sprintf("<html>%d</html>", i), "c1", nil)
	}

	select {
	case <-sub.done:
	case <-time.After(time.Second):
		t.Fatal("a subscriber that overflowed its bounded queue was never unblocked")
	}
	if h.subscriberCount("/tmp/a.html") != 0 {
		t.Fatal("evicted subscriber is still registered")
	}
}

func TestHubShutdownClosesEveryStream(t *testing.T) {
	h := newHub()
	a := newSubscriber("/tmp/a.html", laneLive)
	b := newSubscriber("/tmp/b.html", laneSaved)
	h.add(a)
	h.add(b)

	h.shutdown()

	for name, sub := range map[string]*subscriber{"a": a, "b": b} {
		select {
		case <-sub.done:
		case <-time.After(time.Second):
			t.Fatalf("subscriber %s was not closed by shutdown", name)
		}
	}
	if h.subscriberCount("/tmp/a.html") != 0 || h.subscriberCount("/tmp/b.html") != 0 {
		t.Fatal("subscribers survived shutdown")
	}
}

func setupLiveSyncTest(t *testing.T) (*Server, *session.File) {
	t.Helper()
	homeDir, _ := filepath.EvalSymlinks(t.TempDir())
	filePath := filepath.Join(homeDir, "page.htmlclay")
	os.WriteFile(filePath, []byte("<!DOCTYPE html>\n<html><body>hi</body></html>"), 0644)

	mgr := session.NewManagerWithHome(homeDir)
	f, err := mgr.Register(filePath)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	srv := New(ln, mgr, logging.NewStdout(), versions.New(t.TempDir()))
	t.Cleanup(func() { srv.hub.shutdown(); srv.watcher.shutdown() })
	return srv, f
}

func postLiveSync(t *testing.T, srv *Server, pageURL, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/_/live-sync/save", strings.NewReader(body))
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.Header.Set("Content-Type", "application/json")
	if pageURL != "" {
		req.Header.Set("Page-URL", pageURL)
	}
	w := httptest.NewRecorder()
	srv.handleLiveSyncSave(w, req)
	return w
}

// The POST leg reads page identity from the Page-URL header, not the query.
func TestLiveSyncSaveReadsPageURLHeader(t *testing.T) {
	srv, f := setupLiveSyncTest(t)
	pageURL := fmt.Sprintf("http://127.0.0.1:%d/page.htmlclay", srv.port)

	sub := newSubscriber(f.AbsPath, laneLive)
	srv.hub.add(sub)

	w := postLiveSync(t, srv, pageURL, `{"html":"<html>peer</html>","sender":"c1"}`)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
	msg := waitFrame(t, sub, time.Second)
	if msg["html"] != "<html>peer</html>" {
		t.Fatalf("payload not relayed: %v", msg)
	}
}

// POST is relay-only: it never persists its payload, backs it up, writes it to
// disk, or advances either per-file record.
func TestLiveSyncSaveNeverPersists(t *testing.T) {
	srv, f := setupLiveSyncTest(t)
	pageURL := fmt.Sprintf("http://127.0.0.1:%d/page.htmlclay", srv.port)

	before, _ := os.ReadFile(f.AbsPath)

	f.Lock()
	beforeWrite, beforeStable := f.LastServerWrite(), f.LastStableObservation()
	f.Unlock()

	if w := postLiveSync(t, srv, pageURL, `{"html":"<html>ghost</html>","sender":"c1"}`); w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	after, _ := os.ReadFile(f.AbsPath)
	if string(after) != string(before) {
		t.Fatalf("relay wrote to disk: %q", after)
	}

	f.Lock()
	afterWrite, afterStable := f.LastServerWrite(), f.LastStableObservation()
	f.Unlock()
	if afterWrite != beforeWrite || afterStable != beforeStable {
		t.Fatal("relay advanced a per-file record")
	}

	key := versions.Key(f.AbsPath, before)
	if entries, _ := srv.versions.List(key, f.AbsPath); len(entries) != 0 {
		t.Fatalf("relay created %d backups", len(entries))
	}
}

func TestLiveSyncSaveValidatesPayload(t *testing.T) {
	srv, _ := setupLiveSyncTest(t)
	pageURL := fmt.Sprintf("http://127.0.0.1:%d/page.htmlclay", srv.port)

	cases := []struct {
		name, body string
		want       int
	}{
		{"missing sender", `{"html":"<html></html>"}`, 400},
		{"empty sender", `{"html":"<html></html>","sender":""}`, 400},
		{"oversized sender", `{"html":"<html></html>","sender":"` + strings.Repeat("s", maxSenderLen+1) + `"}`, 400},
		{"missing html", `{"sender":"c1"}`, 400},
		{"identityMap null", `{"html":"<html></html>","sender":"c1","identityMap":null}`, 400},
		{"identityMap array", `{"html":"<html></html>","sender":"c1","identityMap":[]}`, 400},
		{"identityMap string", `{"html":"<html></html>","sender":"c1","identityMap":"x"}`, 400},
		{"not json", `nope`, 400},
		{"identityMap object", `{"html":"<html></html>","sender":"c1","identityMap":{"0":"a"}}`, 200},
		{"no identityMap", `{"html":"<html></html>","sender":"c1"}`, 200},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if w := postLiveSync(t, srv, pageURL, c.body); w.Code != c.want {
				t.Fatalf("expected %d, got %d: %s", c.want, w.Code, w.Body.String())
			}
		})
	}
}

func TestLiveSyncSaveRejectsUnknownOrForeignPageURL(t *testing.T) {
	srv, _ := setupLiveSyncTest(t)
	body := `{"html":"<html></html>","sender":"c1"}`

	cases := []string{
		"",
		"http://evil.com/page.htmlclay",
		"https://127.0.0.1/page.htmlclay",
		fmt.Sprintf("http://127.0.0.1:%d/never-opened.htmlclay", srv.port),
		fmt.Sprintf("http://127.0.0.1:%d/../../etc/passwd", srv.port),
		fmt.Sprintf("http://127.0.0.1:%d/%%2e%%2e/%%2e%%2e/etc/passwd", srv.port),
	}
	for _, pageURL := range cases {
		if w := postLiveSync(t, srv, pageURL, body); w.Code != 404 {
			t.Errorf("page-url %q returned %d, want 404", pageURL, w.Code)
		}
	}
}

// The SPA suffix is stripped, so a client on a client-routed sub-path still
// resolves to its own file.
func TestResolvePageURLStripsSPASuffix(t *testing.T) {
	srv, f := setupLiveSyncTest(t)
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)

	pageURL := fmt.Sprintf("http://127.0.0.1:%d/page.htmlclay/settings/deep", srv.port)
	got, ok := srv.resolvePageURL(req, pageURL)
	if !ok || got != f {
		t.Fatalf("SPA sub-path did not resolve to the page: %v %v", got, ok)
	}
}

// End-to-end over a real connection, which is the only way to prove the logging
// responseWriter's Unwrap and Flush actually reach the underlying writer. Without
// them http.ResponseController cannot clear the write deadline or flush, and the
// stream never delivers a byte.
func TestSSEStreamFlushesOverARealConnection(t *testing.T) {
	homeDir, _ := filepath.EvalSymlinks(t.TempDir())
	filePath := filepath.Join(homeDir, "page.htmlclay")
	os.WriteFile(filePath, []byte("<!DOCTYPE html>\n<html><body>hi</body></html>"), 0644)

	mgr := session.NewManagerWithHome(homeDir)
	f, err := mgr.Register(filePath)
	if err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := New(ln, mgr, logging.NewStdout(), versions.New(t.TempDir()))
	go srv.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})

	base := fmt.Sprintf("http://127.0.0.1:%d", srv.port)
	pageURL := base + "/page.htmlclay"

	req, _ := http.NewRequest("GET", base+"/_/live-sync/stream?page-url="+url.QueryEscape(pageURL)+"&lane=live", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("stream returned %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q", cc)
	}

	// Wait for the subscriber to register before relaying.
	deadline := time.Now().Add(2 * time.Second)
	for srv.hub.subscriberCount(f.AbsPath) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if srv.hub.subscriberCount(f.AbsPath) == 0 {
		t.Fatal("stream never registered a subscriber")
	}

	srv.hub.relay(f.AbsPath, "<html>peer</html>", "c1", nil)

	type result struct {
		line string
		err  error
	}
	got := make(chan result, 1)
	go func() {
		reader := bufio.NewReader(resp.Body)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				got <- result{err: err}
				return
			}
			if strings.HasPrefix(line, "data: ") {
				got <- result{line: line}
				return
			}
		}
	}()

	select {
	case r := <-got:
		if r.err != nil {
			t.Fatalf("reading the stream failed: %v", r.err)
		}
		payload, _ := strings.CutPrefix(strings.TrimSpace(r.line), "data: ")
		var out map[string]interface{}
		if err := json.Unmarshal([]byte(payload), &out); err != nil {
			t.Fatalf("frame is not valid JSON: %v (%q)", err, payload)
		}
		if out["html"] != "<html>peer</html>" {
			t.Fatalf("unexpected frame: %q", r.line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no frame arrived; the stream is not being flushed")
	}
}

// Shutdown closes streams before handing off to http.Server.Shutdown, so an
// active stream does not hold graceful shutdown open until its timeout.
func TestShutdownClosesActiveStreamsPromptly(t *testing.T) {
	homeDir, _ := filepath.EvalSymlinks(t.TempDir())
	filePath := filepath.Join(homeDir, "page.htmlclay")
	os.WriteFile(filePath, []byte("<!DOCTYPE html>\n<html><body>hi</body></html>"), 0644)

	mgr := session.NewManagerWithHome(homeDir)
	f, _ := mgr.Register(filePath)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := New(ln, mgr, logging.NewStdout(), versions.New(t.TempDir()))
	go srv.Start()

	base := fmt.Sprintf("http://127.0.0.1:%d", srv.port)
	pageURL := base + "/page.htmlclay"
	resp, err := http.Get(base + "/_/live-sync/stream?page-url=" + url.QueryEscape(pageURL))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	deadline := time.Now().Add(2 * time.Second)
	for srv.hub.subscriberCount(f.AbsPath) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	start := time.Now()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("shutdown took %v; the active stream held it open", elapsed)
	}
}

// page-url resolution is routing, not authentication, but it still runs the full
// path pipeline: decode once, strip the SPA suffix, validate, resolve symlinks,
// then look up canonically. A symlink out of the home tree resolves to a path
// that was never registered, on both the GET and POST legs.
func TestLiveSyncRejectsSymlinkEscape(t *testing.T) {
	homeDir, _ := filepath.EvalSymlinks(t.TempDir())
	outside, _ := filepath.EvalSymlinks(t.TempDir())

	secret := filepath.Join(outside, "secret.htmlclay")
	os.WriteFile(secret, []byte("<!DOCTYPE html>\n<html><body>classified</body></html>"), 0644)

	pagePath := filepath.Join(homeDir, "page.htmlclay")
	os.WriteFile(pagePath, []byte("<!DOCTYPE html>\n<html><body>hi</body></html>"), 0644)

	link := filepath.Join(homeDir, "link.htmlclay")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	mgr := session.NewManagerWithHome(homeDir)
	if _, err := mgr.Register(pagePath); err != nil {
		t.Fatal(err)
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	t.Cleanup(func() { ln.Close() })
	srv := New(ln, mgr, logging.NewStdout(), versions.New(t.TempDir()))
	t.Cleanup(func() { srv.hub.shutdown(); srv.watcher.shutdown() })

	pageURL := fmt.Sprintf("http://127.0.0.1:%d/link.htmlclay", srv.port)

	// POST leg.
	if w := postLiveSync(t, srv, pageURL, `{"html":"<html></html>","sender":"c1"}`); w.Code != 404 {
		t.Errorf("POST leg accepted a symlink escape: %d", w.Code)
	}

	// GET leg, through resolvePageURL directly since a recorder cannot stream.
	req := httptest.NewRequest("GET", "/_/live-sync/stream", nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	if f, ok := srv.resolvePageURL(req, pageURL); ok {
		t.Errorf("GET leg resolved a symlink escape to %s", f.AbsPath)
	}
}

// The live-sync routes sit behind the same Host validation as everything else.
func TestLiveSyncRoutesRejectForeignHost(t *testing.T) {
	srv, _ := setupLiveSyncTest(t)

	for _, target := range []string{"/_/live-sync/stream", "/_/live-sync/save"} {
		req := httptest.NewRequest("GET", target, nil)
		req.Host = "evil.com:1234"
		w := httptest.NewRecorder()
		srv.httpServer.Handler.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s from a foreign host returned %d, want 403", target, w.Code)
		}
	}
}

// Watcher lifecycle is driven by the stream handler, one watch and one unwatch
// per connection, so an evicted subscriber cannot strand a polling goroutine on a
// file nobody is watching any more.
func TestClosedStreamStopsTheWatcher(t *testing.T) {
	homeDir, _ := filepath.EvalSymlinks(t.TempDir())
	filePath := filepath.Join(homeDir, "page.htmlclay")
	os.WriteFile(filePath, []byte("<!DOCTYPE html>\n<html><body>hi</body></html>"), 0644)

	mgr := session.NewManagerWithHome(homeDir)
	f, _ := mgr.Register(filePath)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := New(ln, mgr, logging.NewStdout(), versions.New(t.TempDir()))
	go srv.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})

	base := fmt.Sprintf("http://127.0.0.1:%d", srv.port)
	resp, err := http.Get(base + "/_/live-sync/stream?page-url=" + url.QueryEscape(base+"/page.htmlclay"))
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, 2*time.Second, "the watcher to start", func() bool {
		return watchEntries(srv) == 1
	})

	// Evict the subscriber by overflowing its bounded queue, the way a wedged
	// client would.
	for i := 0; i < subQueueSize+5; i++ {
		srv.hub.relay(f.AbsPath, fmt.Sprintf("<html>%d</html>", i), "c1", nil)
	}
	resp.Body.Close()

	waitFor(t, 3*time.Second, "the watcher to stop", func() bool {
		return watchEntries(srv) == 0 && !watcherRunning(srv)
	})
}

func watchEntries(srv *Server) int {
	srv.watcher.mu.Lock()
	defer srv.watcher.mu.Unlock()
	return len(srv.watcher.entries)
}

func watcherRunning(srv *Server) bool {
	srv.watcher.mu.Lock()
	defer srv.watcher.mu.Unlock()
	return srv.watcher.running
}

func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
