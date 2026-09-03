package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"
)

// A page's wire stream opens with a cursor carrying a position, so a stream that
// drops before any frame can still resume with an id. A process tailing the same
// wire gets none: it parses every data line as a wire frame.
func TestWireStreamOpensWithACursorForPages(t *testing.T) {
	srv, f := setupLiveSyncTest(t)
	t.Cleanup(func() { srv.wire.shutdown() })
	ts := httptest.NewServer(srv.wireMux())
	t.Cleanup(ts.Close)

	pageURL := ts.URL + "/" + f.RelPath
	req, _ := http.NewRequest("GET", ts.URL+"/_/wire/subscribe?document-url="+url.QueryEscape(pageURL), nil)
	sameOriginHeaders(req)
	resp, events := openStream(t, req.URL.String(), func(r *http.Request) *http.Request {
		r.Header = req.Header.Clone()
		return r
	})
	if resp.StatusCode != 200 {
		t.Fatalf("page subscription refused: %d", resp.StatusCode)
	}
	ev := nextEvent(t, events, "the page's cursor")
	if ev.name != "cursor" {
		t.Fatalf("first frame to a page = %+v, want a cursor", ev)
	}
	if id, err := strconv.ParseInt(ev.id, 10, 64); err != nil || id <= 0 {
		t.Fatalf("cursor id = %q, want a positive position", ev.id)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	preq, _ := http.NewRequestWithContext(ctx, "GET", wireSubscribeURL(ts, f, ""), nil)
	presp, err := http.DefaultClient.Do(preq)
	if err != nil {
		t.Fatal(err)
	}
	defer presp.Body.Close()
	if presp.StatusCode != 200 {
		t.Fatalf("process subscription refused: %d", presp.StatusCode)
	}
	select {
	case ev, ok := <-readSSE(presp.Body):
		if ok {
			t.Fatalf("a process received %+v on an idle wire, want nothing", ev)
		}
	case <-time.After(500 * time.Millisecond):
	}
}
