package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/panphora/htmlclay/internal/logging"
	"github.com/panphora/htmlclay/internal/session"
	"github.com/panphora/htmlclay/internal/versions"
)

type sseEvent struct {
	name string
	id   string
	data string
}

// readSSE parses a stream into events, one goroutine per body. It stops at the
// first read error, which is how a closed stream is observed.
func readSSE(body io.Reader) <-chan sseEvent {
	out := make(chan sseEvent, 64)
	go func() {
		defer close(out)
		reader := bufio.NewReader(body)
		var ev sseEvent
		var data []string
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\n")
			switch {
			case line == "":
				if len(data) > 0 {
					ev.data = strings.Join(data, "\n")
					out <- ev
				}
				ev = sseEvent{}
				data = data[:0]
			case strings.HasPrefix(line, ":"):
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "id: "):
				ev.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "data: "):
				data = append(data, strings.TrimPrefix(line, "data: "))
			}
		}
	}()
	return out
}

func nextEvent(t *testing.T, events <-chan sseEvent, what string) sseEvent {
	t.Helper()
	select {
	case ev, ok := <-events:
		if !ok {
			t.Fatalf("stream closed while waiting for %s", what)
		}
		return ev
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
	return sseEvent{}
}

type sharedCursor struct {
	Sub    int    `json:"sub"`
	Seq    int64  `json:"seq"`
	Resync bool   `json:"resync"`
	Error  string `json:"error"`
}

func parseCursor(t *testing.T, ev sseEvent) sharedCursor {
	t.Helper()
	if ev.name != "cursor" {
		t.Fatalf("expected a cursor event, got %+v", ev)
	}
	if ev.id != "" {
		t.Fatalf("a shared cursor must carry no id, got %+v", ev)
	}
	var c sharedCursor
	if err := json.Unmarshal([]byte(ev.data), &c); err != nil {
		t.Fatalf("cursor data %q: %v", ev.data, err)
	}
	return c
}

// startSharedHost is a real listener, because the stream clears its write
// deadline and flushes through the logging writer, which a recorder cannot do.
func startSharedHost(t *testing.T) (*Server, string, func(name, content string) *session.File) {
	t.Helper()
	homeDir, _ := filepath.EvalSymlinks(t.TempDir())
	mgr := newTestManager(t, homeDir)
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
	register := func(name, content string) *session.File {
		t.Helper()
		p := filepath.Join(homeDir, name)
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		f, err := mgr.Register(p, session.ViaOsOpen)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	return srv, base, register
}

func entry(lane string, since int64, href string) string {
	return fmt.Sprintf("%s:%d:%s", lane, since, href)
}

func sharedURL(base string, entries ...string) string {
	q := make([]string, 0, len(entries))
	for _, e := range entries {
		q = append(q, "s="+url.QueryEscape(e))
	}
	return base + "/_/sync?" + strings.Join(q, "&")
}

func openStream(t *testing.T, target string, headers func(*http.Request) *http.Request) (*http.Response, <-chan sseEvent) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", target, nil)
	if headers != nil {
		headers(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		resp.Body.Close()
	})
	return resp, readSSE(resp.Body)
}

const pageA = "<!DOCTYPE html>\n<html><body>a</body></html>"
const pageB = "<!DOCTYPE html>\n<html><body>b</body></html>"

// Two documents on one connection: a cursor per entry in query order, and every
// frame named after the entry it belongs to, while a one-document stream on the
// same file still receives unnamed frames.
func TestSharedStreamNamesEachSubscription(t *testing.T) {
	srv, base, register := startSharedHost(t)
	a := register("a.htmlclay", pageA)
	b := register("b.htmlclay", pageB)

	resp, events := openStream(t, sharedURL(base, entry(laneLive, 0, base+"/a.htmlclay"), entry(laneSaved, 0, base+"/b.htmlclay")), sameOriginHeaders)
	if resp.StatusCode != 200 {
		t.Fatalf("shared stream returned %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	for i := 0; i < 2; i++ {
		c := parseCursor(t, nextEvent(t, events, "cursor"))
		if c.Sub != i || c.Resync || c.Error != "" {
			t.Fatalf("cursor %d = %+v", i, c)
		}
	}

	single, singleEvents := openStream(t, base+"/_/sync?document-url="+url.QueryEscape(base+"/a.htmlclay")+"&lane=live", sameOriginHeaders)
	if single.StatusCode != 200 {
		t.Fatalf("one-document stream returned %d", single.StatusCode)
	}
	if ev := nextEvent(t, singleEvents, "the one-document cursor"); ev.name != "cursor" || ev.id == "" {
		t.Fatalf("one-document cursor = %+v, want a cursor carrying an id", ev)
	}

	waitFor(t, 10*time.Second, "all three subscriptions to register", func() bool {
		return srv.hub.subscriberCount(a.AbsPath) == 2 && srv.hub.subscriberCount(b.AbsPath) == 1
	})

	srv.coord.relay(a, "<html>peer</html>", "c1", "", nil)
	ev := nextEvent(t, events, "the relay on a")
	if ev.name != "s0" || ev.id == "" || !strings.Contains(ev.data, "peer") {
		t.Fatalf("relay on a arrived on the shared stream as %+v, want event s0 with an id", ev)
	}
	if ev := nextEvent(t, singleEvents, "the relay on the one-document stream"); ev.name != "" || !strings.Contains(ev.data, "peer") {
		t.Fatalf("relay on a arrived on the one-document stream as %+v, want an unnamed event", ev)
	}

	srv.coord.broadcastSaved(b, "<html>saved</html>", "c2")
	ev = nextEvent(t, events, "the save on b")
	if ev.name != "s1" || !strings.Contains(ev.data, "saved") {
		t.Fatalf("save on b arrived as %+v, want event s1", ev)
	}
}

// Each entry resumes from its own position: one that presents the last id it
// saw gets the frames after it, tagged, with no resync; one presenting a position
// the host cannot vouch for is told to resync; a fresh one gets neither.
func TestSharedStreamResumesEachEntryFromItsOwnPosition(t *testing.T) {
	srv, base, register := startSharedHost(t)
	a := register("a.htmlclay", pageA)
	register("b.htmlclay", pageB)

	first, firstEvents := openStream(t, sharedURL(base, entry(laneLive, 0, base+"/a.htmlclay")), sameOriginHeaders)
	if first.StatusCode != 200 {
		t.Fatalf("first stream returned %d", first.StatusCode)
	}
	start := parseCursor(t, nextEvent(t, firstEvents, "the first cursor"))
	waitFor(t, 10*time.Second, "the first stream to register", func() bool {
		return srv.hub.subscriberCount(a.AbsPath) == 1
	})
	srv.coord.relay(a, "<html>one</html>", "c1", "", nil)
	srv.coord.relay(a, "<html>two</html>", "c1", "", nil)
	one := nextEvent(t, firstEvents, "frame one")
	two := nextEvent(t, firstEvents, "frame two")
	if one.name != "s0" || two.name != "s0" || one.id == "" || two.id == "" {
		t.Fatalf("frames = %+v %+v", one, two)
	}

	// Resume from the cursor: both frames come back, tagged, and no resync. A
	// second entry with a position on a file nothing was ever retained for is
	// told to resync, and a third fresh entry is simply current.
	resp, events := openStream(t, sharedURL(base,
		entry(laneLive, start.Seq, base+"/a.htmlclay"),
		entry(laneLive, 5, base+"/b.htmlclay"),
		entry(laneSaved, 0, base+"/b.htmlclay"),
	), sameOriginHeaders)
	if resp.StatusCode != 200 {
		t.Fatalf("resumed stream returned %d", resp.StatusCode)
	}
	c0 := parseCursor(t, nextEvent(t, events, "cursor 0"))
	if c0.Sub != 0 || c0.Resync {
		t.Fatalf("cursor 0 = %+v, want no resync", c0)
	}
	for _, want := range []sseEvent{one, two} {
		got := nextEvent(t, events, "replay of "+want.id)
		if got.name != "s0" || got.id != want.id || got.data != want.data {
			t.Fatalf("replayed %+v, want %+v", got, want)
		}
	}
	c1 := parseCursor(t, nextEvent(t, events, "cursor 1"))
	if c1.Sub != 1 || !c1.Resync {
		t.Fatalf("cursor 1 = %+v, want resync for a position the host cannot vouch for", c1)
	}
	c2 := parseCursor(t, nextEvent(t, events, "cursor 2"))
	if c2.Sub != 2 || c2.Resync || c2.Error != "" {
		t.Fatalf("cursor 2 = %+v, want a plain fresh cursor", c2)
	}

	// A position above the head is a fresh subscription, not a resync.
	resp3, events3 := openStream(t, sharedURL(base, entry(laneLive, start.Seq+1_000_000, base+"/a.htmlclay")), sameOriginHeaders)
	if resp3.StatusCode != 200 {
		t.Fatalf("future stream returned %d", resp3.StatusCode)
	}
	if c := parseCursor(t, nextEvent(t, events3, "the future cursor")); c.Resync || c.Error != "" {
		t.Fatalf("future cursor = %+v, want fresh", c)
	}
}

// A document the caller may not follow is answered inside the stream, and the
// other entries are served. A stream of nothing but such entries still opens.
func TestSharedStreamAnswersNotFoundInsideTheStream(t *testing.T) {
	srv, base, register := startSharedHost(t)
	a := register("a.htmlclay", pageA)

	resp, events := openStream(t, sharedURL(base, entry(laneLive, 0, base+"/a.htmlclay"), entry(laneLive, 0, base+"/nope.htmlclay")), sameOriginHeaders)
	if resp.StatusCode != 200 {
		t.Fatalf("stream returned %d", resp.StatusCode)
	}
	if c := parseCursor(t, nextEvent(t, events, "cursor 0")); c.Sub != 0 || c.Error != "" {
		t.Fatalf("cursor 0 = %+v", c)
	}
	if c := parseCursor(t, nextEvent(t, events, "cursor 1")); c.Sub != 1 || c.Error != "not-found" {
		t.Fatalf("cursor 1 = %+v, want not-found", c)
	}
	waitFor(t, 10*time.Second, "the resolved entry to register", func() bool {
		return srv.hub.subscriberCount(a.AbsPath) == 1
	})
	srv.coord.relay(a, "<html>still here</html>", "c1", "", nil)
	if ev := nextEvent(t, events, "the relay"); ev.name != "s0" {
		t.Fatalf("relay arrived as %+v", ev)
	}

	only, onlyEvents := openStream(t, sharedURL(base, entry(laneSaved, 0, base+"/nope.htmlclay")), sameOriginHeaders)
	if only.StatusCode != 200 {
		t.Fatalf("a stream of unresolvable entries returned %d, want 200 with not-found cursors", only.StatusCode)
	}
	if c := parseCursor(t, nextEvent(t, onlyEvents, "the only cursor")); c.Error != "not-found" {
		t.Fatalf("cursor = %+v", c)
	}
}

// The list is refused whole when it is not a list: above the cap, or any entry
// the host cannot parse.
func TestSharedStreamRefusesMalformedLists(t *testing.T) {
	srv, _, register := startSharedHost(t)
	register("a.htmlclay", pageA)
	base := fmt.Sprintf("http://127.0.0.1:%d", srv.port)

	cases := map[string]string{
		"no subscriptions": base + "/_/sync",
		"empty value":      base + "/_/sync?s=",
		"two fields":       sharedURL(base, "live:"+base+"/a.htmlclay"),
		"bad since":        sharedURL(base, "live:x:"+base+"/a.htmlclay"),
		"negative":         sharedURL(base, "live:-1:"+base+"/a.htmlclay"),
		"unknown lane":     sharedURL(base, "both:0:"+base+"/a.htmlclay"),
		"one too many":     sharedURL(base, repeated(entry(laneLive, 0, base+"/a.htmlclay"), maxSharedSubs+1)...),
	}
	for name, target := range cases {
		req, _ := http.NewRequest("GET", target, nil)
		sameOriginHeaders(req)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", name, resp.StatusCode)
		}
	}

	// The cap itself is served.
	req, _ := http.NewRequest("GET", sharedURL(base, repeated(entry(laneLive, 0, base+"/a.htmlclay"), maxSharedSubs)...), nil)
	sameOriginHeaders(req)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("a list at the cap returned %d, want 200", resp.StatusCode)
	}
}

func repeated(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}

// One connection, N records, one closer. Evicting any record ends the stream
// once, and stopping the others afterwards is a no-op rather than a panic.
func TestSharedStreamRecordsShareOneEnd(t *testing.T) {
	h := newHub("")
	t.Cleanup(h.shutdown)
	ch := make(chan queued, 1)
	end := newStreamEnd()
	a := &subscriber{key: "/a", lane: laneLive, ch: ch, end: end, tag: []byte("event: s0\n")}
	b := &subscriber{key: "/b", lane: laneLive, ch: ch, end: end, tag: []byte("event: s1\n")}
	h.add(a)
	h.add(b)

	h.relay("/a", "<html>1</html>", "c", "", nil)
	evicted := h.relay("/b", "<html>2</html>", "c", "", nil)
	if len(evicted) != 1 || evicted[0] != b {
		t.Fatalf("evicted = %v, want the record whose offer overflowed the shared queue", evicted)
	}
	select {
	case <-end.done:
	default:
		t.Fatal("evicting one record did not end the shared stream")
	}
	a.stop()
	b.stop()

	// What did land is tagged for its own entry.
	q := <-ch
	if string(q.tag) != "event: s0\n" || !strings.HasPrefix(string(q.frame), "id: ") {
		t.Fatalf("queued entry = %q + %q, want it tagged s0", q.tag, q.frame)
	}
}

// Closing the connection tears down every record, so the watcher and the hub
// forget both files, not just the last one registered.
func TestSharedStreamTeardownRemovesEveryRecord(t *testing.T) {
	srv, base, register := startSharedHost(t)
	a := register("a.htmlclay", pageA)
	b := register("b.htmlclay", pageB)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", sharedURL(base, entry(laneLive, 0, base+"/a.htmlclay"), entry(laneSaved, 0, base+"/b.htmlclay")), nil)
	sameOriginHeaders(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "both records to register", func() bool {
		return srv.hub.subscriberCount(a.AbsPath) == 1 && srv.hub.subscriberCount(b.AbsPath) == 1
	})
	if !watchedPath(srv.watcher, a.AbsPath) || !watchedPath(srv.watcher, b.AbsPath) {
		t.Fatal("both files should be watched while the shared stream is open")
	}

	cancel()
	resp.Body.Close()
	waitFor(t, 10*time.Second, "both records to be removed", func() bool {
		return srv.hub.subscriberCount(a.AbsPath) == 0 && srv.hub.subscriberCount(b.AbsPath) == 0
	})
	waitWatched(t, srv.watcher, a.AbsPath, false)
	waitWatched(t, srv.watcher, b.AbsPath, false)
}

// The list form joins the same gate as the one-document form.
func TestSharedStreamIsSameOriginGated(t *testing.T) {
	_, base, register := startSharedHost(t)
	register("a.htmlclay", pageA)

	req, _ := http.NewRequest("GET", sharedURL(base, entry(laneLive, 0, base+"/a.htmlclay")), nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site shared stream: got %d, want 403", resp.StatusCode)
	}
}

// A record queues the hub's frame, never a copy of it. A list may name one
// document many times, and a copy per record would turn one relay into that
// many copies, allocated under the hub lock.
func TestSharedRecordsQueueTheFrameNotACopy(t *testing.T) {
	ch := make(chan queued, 4)
	end := newStreamEnd()
	a := &subscriber{key: "/k", lane: laneLive, ch: ch, end: end, tag: []byte("event: s0\n")}
	b := &subscriber{key: "/k", lane: laneLive, ch: ch, end: end, tag: []byte("event: s1\n")}
	frame := []byte("id: 1\ndata: {}\n\n")
	if !offer(a, [][]byte{frame}) || !offer(b, [][]byte{frame}) {
		t.Fatal("offer refused with room in the queue")
	}
	for _, want := range []string{"event: s0\n", "event: s1\n"} {
		q := <-ch
		if string(q.tag) != want {
			t.Fatalf("tag = %q, want %q", q.tag, want)
		}
		if &q.frame[0] != &frame[0] {
			t.Fatal("the queue holds a copy of the frame; every record must share the hub's slice")
		}
	}
}

// Eviction through the handler: overflowing the shared queue on one file ends
// the writer, and the handler's deferred removals unwind every record, so the
// other file is forgotten too and neither is watched any more.
func TestSharedStreamEvictionRemovesEveryRecord(t *testing.T) {
	srv, base, register := startSharedHost(t)
	a := register("a.htmlclay", pageA)
	b := register("b.htmlclay", pageB)

	resp, _ := openStream(t, sharedURL(base, entry(laneLive, 0, base+"/a.htmlclay"), entry(laneSaved, 0, base+"/b.htmlclay")), sameOriginHeaders)
	if resp.StatusCode != 200 {
		t.Fatalf("shared stream returned %d", resp.StatusCode)
	}
	waitFor(t, 10*time.Second, "both records to register", func() bool {
		return srv.hub.subscriberCount(a.AbsPath) == 1 && srv.hub.subscriberCount(b.AbsPath) == 1
	})

	// The reader stops after a few frames. Frames large enough to fill the socket
	// behind it leave the writer stuck and the queue full, and the next offer
	// evicts.
	big := "<html>" + strings.Repeat("x", 16*1024) + "</html>"
	for i := 0; i < 2*sharedQueueSize; i++ {
		srv.hub.relay(a.AbsPath, big, "c1", "", nil)
	}
	waitFor(t, 10*time.Second, "every record to be removed", func() bool {
		return srv.hub.subscriberCount(a.AbsPath) == 0 && srv.hub.subscriberCount(b.AbsPath) == 0 &&
			!watchedPath(srv.watcher, a.AbsPath) && !watchedPath(srv.watcher, b.AbsPath)
	})
}
