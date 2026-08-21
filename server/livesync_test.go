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
	"strconv"
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

// splitFrame pulls the SSE id and the JSON payload out of one frame. Every frame
// carries an id so a reconnecting EventSource can resume with Last-Event-ID.
func splitFrame(t *testing.T, raw []byte) (int64, map[string]interface{}) {
	t.Helper()
	rest, ok := strings.CutPrefix(string(raw), "id: ")
	if !ok {
		t.Fatalf("frame carries no SSE id: %q", raw)
	}
	idLine, body, ok := strings.Cut(rest, "\n")
	if !ok {
		t.Fatalf("frame has no line after its id: %q", raw)
	}
	id, err := strconv.ParseInt(idLine, 10, 64)
	if err != nil {
		t.Fatalf("frame id is not a number: %q", idLine)
	}
	payload, ok := strings.CutPrefix(body, "data: ")
	if !ok {
		t.Fatalf("frame is not an SSE data line: %q", raw)
	}
	var out map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(payload)), &out); err != nil {
		t.Fatalf("frame is not valid JSON: %v (%q)", err, payload)
	}
	return id, out
}

func waitFrame(t *testing.T, sub *subscriber, within time.Duration) map[string]interface{} {
	t.Helper()
	select {
	case raw := <-sub.ch:
		_, out := splitFrame(t, raw)
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
	h := newHub("")
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
	h := newHub("")
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
	h := newHub("")
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

// An external change notifies the live lane with the disk HTML riding the
// notification's data field — never a top-level livePayload, which an old
// client would full-morph over unsaved DOM state. New clients route the data
// through dirty-region protection; old clients forward `data` inertly and just
// toast. The stable disk HTML still goes to the saved lane.
func TestExternalChangeNotifiesLiveAndBroadcastsSaved(t *testing.T) {
	h := newHub("")
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
		t.Fatal("live lane received top-level content, which an old client would morph over unsaved DOM state")
	}
	data, ok := notice["data"].(map[string]interface{})
	if !ok {
		t.Fatalf("notification carries no data object: %v", notice)
	}
	if data["kind"] != "external-change" || data["html"] != "<html>disk</html>" || data["sender"] != "file-system" {
		t.Fatalf("external-change data wrong: %v", data)
	}
	expectNoFrame(t, live, 100*time.Millisecond)

	content := waitFrame(t, saved, time.Second)
	if content["html"] != "<html>disk</html>" || content["sender"] != "file-system" {
		t.Fatalf("saved lane payload wrong: %v", content)
	}
}

// The data payload is built with an escape-off encoder: json.Marshal would
// pre-escape every angle bracket into six bytes, tripling a document of tags,
// and the outer frame encoder cannot undo escaping baked into a RawMessage.
func TestExternalChangeEmbedsDiskHTMLUnescaped(t *testing.T) {
	h := newHub("")
	live := newSubscriber("/tmp/a.html", laneLive)
	h.add(live)

	h.publishExternalChange("/tmp/a.html", "changed", "<html><body>disk</body></html>")

	select {
	case raw := <-live.ch:
		if !strings.Contains(string(raw), "<html><body>disk</body></html>") {
			t.Fatalf("frame does not carry literal HTML (escaped?): %q", raw)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the notification frame")
	}
}

// HTML over maxLiveSyncSize falls back to a bare notification (no data field);
// the new client recovers the content with a token-free fetch of the served
// page. The saved lane still gets the full document either way.
func TestOversizedExternalChangeFallsBackToBareNotification(t *testing.T) {
	h := newHub("")
	live := newSubscriber("/tmp/a.html", laneLive)
	saved := newSubscriber("/tmp/a.html", laneSaved)
	h.add(live)
	h.add(saved)

	big := "<html>" + strings.Repeat("a", maxLiveSyncSize) + "</html>"
	h.publishExternalChange("/tmp/a.html", "changed", big)

	notice := waitFrame(t, live, time.Second)
	if notice["type"] != "notification" {
		t.Fatalf("live lane did not receive a notification: %v", notice)
	}
	if _, ok := notice["data"]; ok {
		t.Fatal("oversized external change still embedded content in the notification")
	}

	content := waitFrame(t, saved, 5*time.Second)
	if content["html"] != big {
		t.Fatal("saved lane did not receive the full oversized document")
	}
}

func TestSlowSubscriberIsEvictedAndUnblocked(t *testing.T) {
	h := newHub("")
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
	h := newHub("")
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

	mgr := newTestManager(t, homeDir)
	f, err := mgr.Register(filePath, session.ViaOsOpen)
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
	req := httptest.NewRequest("POST", "/_/sync", strings.NewReader(body))
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

	mgr := newTestManager(t, homeDir)
	f, err := mgr.Register(filePath, session.ViaOsOpen)
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

	req, _ := http.NewRequest("GET", base+"/_/sync?document-url="+url.QueryEscape(pageURL)+"&lane=live", nil)
	sameOriginHeaders(req)
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
		event := ""
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				got <- result{err: err}
				return
			}
			switch {
			case strings.TrimRight(line, "\n") == "":
				event = ""
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
			case strings.HasPrefix(line, "data: "):
				// The cursor frame is sent first so a native EventSource records an
				// id on connect; it carries no html, so skip it.
				if event == "cursor" {
					continue
				}
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

	mgr := newTestManager(t, homeDir)
	f, _ := mgr.Register(filePath, session.ViaOsOpen)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := New(ln, mgr, logging.NewStdout(), versions.New(t.TempDir()))
	go srv.Start()

	base := fmt.Sprintf("http://127.0.0.1:%d", srv.port)
	pageURL := base + "/page.htmlclay"
	streamReq, _ := http.NewRequest("GET", base+"/_/sync?document-url="+url.QueryEscape(pageURL), nil)
	resp, err := http.DefaultClient.Do(sameOriginHeaders(streamReq))
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

	mgr := newTestManager(t, homeDir)
	if _, err := mgr.Register(pagePath, session.ViaOsOpen); err != nil {
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
	req := httptest.NewRequest("GET", "/_/sync", nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	if f, ok := srv.resolvePageURL(req, pageURL); ok {
		t.Errorf("GET leg resolved a symlink escape to %s", f.AbsPath)
	}
}

// The live-sync routes sit behind the same Host validation as everything else.
func TestLiveSyncRoutesRejectForeignHost(t *testing.T) {
	srv, _ := setupLiveSyncTest(t)

	for _, target := range []string{"/_/sync", "/_/sync"} {
		req := httptest.NewRequest("GET", target, nil)
		req.Host = "evil.com:1234"
		w := httptest.NewRecorder()
		srv.httpServer.Handler.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s from a foreign host returned %d, want 403", target, w.Code)
		}
	}
}

// A tab watching a file must never make that file unsaveable. Windows refuses to
// rename over a file while any handle to it is open, whatever sharing mode that
// handle asked for (platform/openshared_windows_test.go pins the OS behaviour),
// so a descriptor parked on the incarnation turned every save into a write error
// for as long as the tab stayed connected. Both entrances are covered here: the
// subscriber one, and the server-write one, which needs no subscriber at all.
//
// On macOS and Linux this passes either way. Only the Windows leg of CI can fail
// it, which is exactly why the shape is also pinned below.
func TestSaveSucceedsWhileSubscribed(t *testing.T) {
	srv, f := setupLiveSyncTest(t)

	for _, lane := range []string{laneLive, laneSaved} {
		srv.hub.add(newSubscriber(f.AbsPath, lane))
	}
	if err := atomicWriteFile(f.AbsPath, []byte("<html>saved</html>")); err != nil {
		t.Fatalf("save while a tab is subscribed: %v", err)
	}

	srv.coord.acceptServerReplacement(f)
	if err := atomicWriteFile(f.AbsPath, []byte("<html>saved again</html>")); err != nil {
		t.Fatalf("save after re-anchoring: %v", err)
	}
}

// The server-write path reaches acceptServerReplacement through the incarnation
// broadcastDiskHTML creates, so save then restore then save breaks on Windows
// with no tab ever connected.
func TestSaveSucceedsAfterAServerWriteWithNoSubscriber(t *testing.T) {
	srv, f := setupLiveSyncTest(t)

	srv.broadcastDiskHTML(f, []byte("<html>one</html>"))
	srv.coord.acceptServerReplacement(f)

	if err := atomicWriteFile(f.AbsPath, []byte("<html>two</html>")); err != nil {
		t.Fatalf("save after a server write with no subscriber: %v", err)
	}
}

// Retained frames belong to a document, not to a pathname. When something outside
// htmlclay puts a different file at the path, a tab reconnecting with a
// Last-Event-ID from the old one must be told nothing about it: replaying those
// frames would paint a dead document's bytes into a page showing the new one.
func TestExternalReplacementDropsTheOldDocumentsReplay(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	path := filepath.Join(dir, "page.html")
	if err := os.WriteFile(path, []byte("<html>one</html>"), 0644); err != nil {
		t.Fatal(err)
	}

	h := newHub("")
	t.Cleanup(h.shutdown)

	sub := newSubscriber(path, laneSaved)
	sub.resumeID = "r1"
	h.add(sub)
	mark := h.seq
	h.broadcastSaved(path, "<html>one</html>", "file-system")
	h.remove(sub)

	// Reconnecting while the file is untouched replays that frame. This half is
	// here so the assertion below cannot pass for the boring reason.
	same := newSubscriber(path, laneSaved)
	same.resumeID = "r1"
	same.lastEventID = mark
	if _, replay, _ := h.add(same); len(replay) != 1 {
		t.Fatalf("an ordinary reconnect should replay the retained frame, got %d", len(replay))
	}
	h.remove(same)
	generation := h.incs[path].generation

	tmp := filepath.Join(dir, "page.html.tmp")
	if err := os.WriteFile(tmp, []byte("<html>two</html>"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}

	after := newSubscriber(path, laneSaved)
	after.resumeID = "r1"
	after.lastEventID = mark
	_, replay, _ := h.add(after)

	if h.incs[path].generation == generation {
		t.Fatal("a file replaced from outside did not roll the generation")
	}
	if len(replay) != 0 {
		t.Fatalf("replayed %d frame(s) from the document that was replaced", len(replay))
	}
}

// anchoredHub returns a hub watching one real file, with a subscriber connected
// and one frame retained, which is the state every identity question is asked
// from. mark is a Last-Event-ID that sits just below the retained frame.
func anchoredHub(t *testing.T) (h *hub, path string, mark int64) {
	t.Helper()
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	path = filepath.Join(dir, "page.html")
	if err := os.WriteFile(path, []byte("<html>one</html>"), 0644); err != nil {
		t.Fatal(err)
	}
	h = newHub("")
	t.Cleanup(h.shutdown)

	sub := newSubscriber(path, laneSaved)
	sub.resumeID = "r1"
	h.add(sub)
	mark = h.seq
	h.broadcastSaved(path, "<html>one</html>", "file-system")
	h.remove(sub)
	return h, path, mark
}

func resume(t *testing.T, h *hub, path string, mark int64) (generation int64, replayed int) {
	t.Helper()
	sub := newSubscriber(path, laneSaved)
	sub.resumeID = "r1"
	sub.lastEventID = mark
	_, replay, _ := h.add(sub)
	h.remove(sub)
	return h.incs[path].generation, len(replay)
}

// The other side of the roll, and the one with teeth: htmlclay replacing the file
// itself is the same document, so the tab that saved it must still be able to
// resume. Without this, a regression that stopped re-anchoring would nuke every
// connected tab's replay on every single save and no test would notice.
func TestServerReplacementKeepsTheGenerationAndTheReplay(t *testing.T) {
	h, path, mark := anchoredHub(t)
	before := h.incs[path].generation

	writeThroughATempFile(t, path, "<html>two</html>")
	h.acceptServerReplacement(path)

	generation, replayed := resume(t, h, path, mark)
	if generation != before {
		t.Fatalf("our own save rolled the generation from %d to %d", before, generation)
	}
	if replayed != 1 {
		t.Fatalf("our own save cost the tab its replay buffer: %d frames", replayed)
	}
}

// Reading identity back can fail on a file we just wrote, which on Windows is an
// indexer or sync client holding a delete-pending window open. That must not be
// mistaken for somebody else replacing the file: the anchor is dropped so the
// next observation adopts, rather than kept so the next observation rolls.
func TestServerReplacementSurvivesAFailedIdentityRead(t *testing.T) {
	h, path, mark := anchoredHub(t)
	before := h.incs[path].generation

	writeThroughATempFile(t, path, "<html>two</html>")
	moved := path + ".moved"
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	h.acceptServerReplacement(path)
	if err := os.Rename(moved, path); err != nil {
		t.Fatal(err)
	}

	generation, replayed := resume(t, h, path, mark)
	if generation != before {
		t.Fatalf("an unreadable file rolled the generation from %d to %d on our own save", before, generation)
	}
	if replayed != 1 {
		t.Fatalf("an unreadable file cost the tab its replay buffer: %d frames", replayed)
	}
}

// Fail closed. Once a file has been identified, being unable to read it back is
// not evidence that it is still the same file, and the retained frames are only
// worth anything if it is.
func TestAnUnreadableFileRollsRatherThanReplays(t *testing.T) {
	h, path, mark := anchoredHub(t)
	before := h.incs[path].generation

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	generation, replayed := resume(t, h, path, mark)
	if generation == before {
		t.Fatal("a file that could no longer be identified kept its generation")
	}
	if replayed != 0 {
		t.Fatalf("replayed %d frame(s) for a file we can no longer identify", replayed)
	}
}

// A path that has never been identified has nothing to protect, so observing it
// must not roll on every look. Rolling there would break resume outright on any
// filesystem that cannot answer an identity query.
func TestAnUnidentifiablePathDoesNotRollOnEveryLook(t *testing.T) {
	h := newHub("")
	t.Cleanup(h.shutdown)
	path := filepath.Join(t.TempDir(), "never-created.html")

	first := newSubscriber(path, laneSaved)
	h.add(first)
	before := h.incs[path].generation
	for i := 0; i < 3; i++ {
		h.add(newSubscriber(path, laneSaved))
	}

	if got := h.incs[path].generation; got != before {
		t.Fatalf("generation walked from %d to %d for a path with no identity", before, got)
	}
}

// The watcher is stopped after the hub is closed, so a poll already in flight can
// land here. It must not rebuild an incarnation nothing will ever reap, which on
// Unix would strand the anchor's descriptor for the life of the process.
func TestPublishAfterShutdownDoesNotRebuildAnIncarnation(t *testing.T) {
	h, path, _ := anchoredHub(t)
	h.shutdown()

	h.publishExternalChange(path, "changed on disk", "<html>two</html>")

	if len(h.incs) != 0 {
		t.Fatalf("a publish after shutdown left %d incarnation(s) behind", len(h.incs))
	}
}

// Where identity can prove nothing, a file vanishing is the only evidence that a
// document ended, so it has to count.
func TestVanishingRollsWhenNothingCanIdentifyTheFile(t *testing.T) {
	h := newHub("")
	t.Cleanup(h.shutdown)
	path := filepath.Join(t.TempDir(), "unidentifiable.html")

	h.broadcastSaved(path, "<html>one</html>", "file-system")
	before := h.incs[path].generation

	h.markAbsent(path)

	if h.incs[path].generation == before {
		t.Fatal("a file that vanished kept its generation, and nothing else can notice it ended")
	}
}

// The same signal must not fire on a healthy file, because the watcher records
// absence during the brief gap of an atomic replacement, our own saves included.
func TestVanishingDoesNotRollAnIdentifiedFile(t *testing.T) {
	h, path, _ := anchoredHub(t)
	before := h.incs[path].generation

	h.markAbsent(path)

	if h.incs[path].generation != before {
		t.Fatal("an atomic replacement window rolled the generation of a file we can identify")
	}
}

func writeThroughATempFile(t *testing.T, path, body string) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// Watcher lifecycle is driven by the stream handler, one watch and one unwatch
// per connection, so an evicted subscriber cannot strand a polling goroutine on a
// file nobody is watching any more.
func TestClosedStreamStopsTheWatcher(t *testing.T) {
	homeDir, _ := filepath.EvalSymlinks(t.TempDir())
	filePath := filepath.Join(homeDir, "page.htmlclay")
	os.WriteFile(filePath, []byte("<!DOCTYPE html>\n<html><body>hi</body></html>"), 0644)

	mgr := newTestManager(t, homeDir)
	f, _ := mgr.Register(filePath, session.ViaOsOpen)

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
	streamReq, _ := http.NewRequest("GET", base+"/_/sync?document-url="+url.QueryEscape(base+"/page.htmlclay"), nil)
	resp, err := http.DefaultClient.Do(sameOriginHeaders(streamReq))
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

// The cursor frame was a named event nobody listened for, and droppedThrough was
// written in two places and read nowhere. Together they answer the one question a
// reconnecting client cannot answer for itself: is what I am holding still
// repairable by replay.
func TestCursorFrameFlagsAnUnrecoverableGap(t *testing.T) {
	h := newHub("")
	t.Cleanup(h.shutdown)
	path := "/tmp/gap.htmlclay"

	seed := newSubscriber(path, laneSaved)
	h.add(seed)
	mark := h.seq
	// One more frame than the bucket retains, so the oldest is dropped and the
	// drop high-water rises above mark.
	for i := 0; i < perIncarnationMaxFrames+1; i++ {
		h.broadcastSaved(path, fmt.Sprintf("<html>%d</html>", i), "file-system")
	}
	h.remove(seed)

	stale := newSubscriber(path, laneSaved)
	stale.lastEventID = mark
	if _, _, resync := h.add(stale); !resync {
		t.Fatal("a resume below the drop high-water was not told to resync")
	}
	if !strings.Contains(string(cursorFrame(mark, true)), `"resync":true`) {
		t.Fatal("the resync flag never reaches the frame")
	}

	// A first connection resumes at the current sequence and has nothing to
	// recover, so it must not be sent chasing a fetch it does not need.
	fresh := newSubscriber(path, laneSaved)
	if _, _, resync := h.add(fresh); resync {
		t.Fatal("a first connection was told to resync")
	}
	if strings.Contains(string(cursorFrame(h.seq, false)), "resync") {
		t.Fatal("resync appears on a frame that does not need it")
	}
}

// Delivery means a subscriber took the frame. A resume cursor does not count: the
// argument for counting it, that its reconnect replays what was retained, holds
// only inside the frame and cursor TTLs, and past them the incarnation is reaped
// and the client comes back to an empty replay against a change already recorded
// as reported.
func TestExternalChangeDeliveryMeansSomeoneTookTheFrame(t *testing.T) {
	h := newHub("")
	t.Cleanup(h.shutdown)
	path := "/tmp/receipt.htmlclay"

	if delivered, _ := h.publishExternalChange(path, "changed", "<html>a</html>"); delivered {
		t.Fatal("an empty hub reported a delivery")
	}

	live := newSubscriber(path, laneLive)
	h.add(live)
	if delivered, _ := h.publishExternalChange(path, "changed", "<html>b</html>"); !delivered {
		t.Fatal("a live subscriber did not count as a delivery")
	}

	resuming := newSubscriber(path, laneLive)
	resuming.resumeID = "r1"
	h.add(resuming)
	h.remove(live)
	h.remove(resuming)
	if delivered, _ := h.publishExternalChange(path, "changed", "<html>c</html>"); delivered {
		t.Fatal("a disconnected resume cursor was counted as a delivery")
	}

	// A subscriber whose bounded queue is full took nothing either, whatever the
	// hub retained on its behalf.
	slow := &subscriber{key: path, lane: laneLive, ch: make(chan []byte, 1), done: make(chan struct{})}
	h.add(slow)
	h.publishExternalChange(path, "changed", "<html>d</html>")
	delivered, evicted := h.publishExternalChange(path, "changed", "<html>e</html>")
	if delivered || len(evicted) == 0 {
		t.Fatalf("a full queue counted as a delivery: delivered=%v evicted=%d", delivered, len(evicted))
	}
}

// The marker the flag reads lives ON the incarnation, and an idle incarnation is
// reaped once its frames and its cursor expire, taking the marker with it. Without
// noticing that the record itself is gone, the reconnect that most needs the flag
// is exactly the one that would not get it.
func TestCursorFrameFlagsAReapedIncarnation(t *testing.T) {
	h := newHub("")
	t.Cleanup(h.shutdown)
	path := "/tmp/reaped.htmlclay"

	now := time.Now()
	h.now = func() time.Time { return now }

	seed := newSubscriber(path, laneSaved)
	seed.resumeID = "r1"
	h.add(seed)
	mark := h.seq
	h.broadcastSaved(path, "<html>one</html>", "file-system")
	h.remove(seed)

	// Past both TTLs: the frames age out, the cursor expires, and the incarnation
	// itself is dropped.
	now = now.Add(2 * (replayFrameTTL + cursorTTL))

	back := newSubscriber(path, laneSaved)
	back.resumeID = "r1"
	back.lastEventID = mark
	from, replay, resync := h.add(back)
	if len(replay) != 0 {
		t.Fatalf("expected an empty replay after the reap, got %d frames", len(replay))
	}
	if !resync {
		t.Fatalf("a reconnect into a reaped incarnation was not told to resync (from=%d)", from)
	}
}
