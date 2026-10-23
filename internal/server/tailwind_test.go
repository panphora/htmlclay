package server

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/panphora/htmlclay/internal/logging"
	"github.com/panphora/htmlclay/internal/tailwind"
)

// fakeTailwind stands in for the compiler: it records every document it was
// handed and answers with a marker derived from that document, so a test can say
// which document produced the stylesheet it got.
type fakeTailwind struct {
	mu   sync.Mutex
	got  [][]byte
	err  error
	fail bool
}

func (f *fakeTailwind) compile(_ context.Context, html []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, append([]byte(nil), html...))
	if f.fail {
		return nil, f.err
	}
	return []byte("/*css*/" + string(html)), nil
}

func (f *fakeTailwind) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.got)
}

func (f *fakeTailwind) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.got) == 0 {
		return ""
	}
	return string(f.got[len(f.got)-1])
}

func tailwindGet(t *testing.T, srv *Server, target string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", target, nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)
	return w
}

func TestTailwindByName(t *testing.T) {
	body := page(`<div class="p-4"></div>`)
	fx := setupFileTest(t, "site.htmlclay", body)
	fake := &fakeTailwind{}
	fx.srv.setTailwindCompiler(fake.compile)

	w := tailwindGet(t, fx.srv, "/tailwindcss/site.css", nil)
	if w.Code != 200 {
		t.Fatalf("got %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Errorf("Content-Type = %q, want text/css", ct)
	}
	if got, want := w.Body.String(), "/*css*/"+body; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", cc)
	}
	if w.Header().Get("ETag") == "" {
		t.Error("no ETag")
	}
	if got := w.Header().Get("Vary"); got != "Referer" {
		t.Errorf("Vary = %q, want Referer", got)
	}
	if got := fake.last(); got != body {
		t.Errorf("compiled %q, want the document %q", got, body)
	}
}

func TestTailwindPrefersHTMLOverHTMLClay(t *testing.T) {
	htmlBody := page(`<div class="html-only"></div>`)
	clayBody := page(`<div class="clay-only"></div>`)
	fx := setupFileTest(t, "a.html", htmlBody)
	registerPageWithContent(t, fx.srv, "a.htmlclay", clayBody)
	fake := &fakeTailwind{}
	fx.srv.setTailwindCompiler(fake.compile)

	w := tailwindGet(t, fx.srv, "/tailwindcss/a.css", nil)
	if w.Code != 200 {
		t.Fatalf("got %d, want 200", w.Code)
	}
	if got, want := w.Body.String(), "/*css*/"+htmlBody; got != want {
		t.Errorf("body = %q, want a.html's %q", got, want)
	}
}

func TestTailwindRefererWins(t *testing.T) {
	realBody := page(`<div class="real"></div>`)
	otherBody := page(`<div class="other"></div>`)
	fx := setupFileTest(t, "real.htmlclay", realBody)
	registerPageWithContent(t, fx.srv, "other.htmlclay", otherBody)
	fake := &fakeTailwind{}
	fx.srv.setTailwindCompiler(fake.compile)

	host := fmt.Sprintf("127.0.0.1:%d", fx.srv.port)
	w := tailwindGet(t, fx.srv, "/tailwindcss/other.css", map[string]string{
		"Referer": "http://" + host + "/real.htmlclay",
	})
	if got, want := w.Body.String(), "/*css*/"+realBody; got != want {
		t.Errorf("same-host Referer: body = %q, want real.htmlclay's %q", got, want)
	}

	w = tailwindGet(t, fx.srv, "/tailwindcss/other.css", map[string]string{
		"Referer": "http://evil.example/real.htmlclay",
	})
	if got, want := w.Body.String(), "/*css*/"+otherBody; got != want {
		t.Errorf("cross-host Referer: body = %q, want other.htmlclay's %q", got, want)
	}
}

func TestTailwindCacheHitAndMiss(t *testing.T) {
	first := page(`<div class="first"></div>`)
	fx := setupFileTest(t, "site.htmlclay", first)
	internalDir := t.TempDir()
	fx.srv.SetInternalDir(internalDir)
	fake := &fakeTailwind{}
	fx.srv.setTailwindCompiler(fake.compile)

	w1 := tailwindGet(t, fx.srv, "/tailwindcss/site.css", nil)
	w2 := tailwindGet(t, fx.srv, "/tailwindcss/site.css", nil)
	if fake.calls() != 1 {
		t.Fatalf("compile calls = %d, want 1 (second request served from cache)", fake.calls())
	}
	if w1.Body.String() != w2.Body.String() {
		t.Errorf("bodies differ: %q and %q", w1.Body.String(), w2.Body.String())
	}
	if got, want := w1.Body.String(), "/*css*/"+first; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}

	second := page(`<div class="second"></div>`)
	if err := os.WriteFile(fx.file.AbsPath, []byte(second), 0644); err != nil {
		t.Fatal(err)
	}
	w3 := tailwindGet(t, fx.srv, "/tailwindcss/site.css", nil)
	if fake.calls() != 2 {
		t.Fatalf("compile calls = %d after the edit, want 2", fake.calls())
	}
	if got, want := w3.Body.String(), "/*css*/"+second; got != want {
		t.Errorf("after the edit: body = %q, want %q", got, want)
	}

	// A text-only edit leaves the candidate list alone, so the stylesheet is the
	// one already cached and nothing is compiled again.
	textOnly := page(`<div class="second">edited text</div>`)
	if err := os.WriteFile(fx.file.AbsPath, []byte(textOnly), 0644); err != nil {
		t.Fatal(err)
	}
	w4 := tailwindGet(t, fx.srv, "/tailwindcss/site.css", nil)
	if fake.calls() != 2 {
		t.Errorf("compile calls = %d after a text-only edit, want 2 (served from cache)", fake.calls())
	}
	if got, want := w4.Body.String(), "/*css*/"+second; got != want {
		t.Errorf("after a text-only edit: body = %q, want the cached %q", got, want)
	}

	entries, err := os.ReadDir(filepath.Join(internalDir, "tailwind", "cache"))
	if err != nil {
		t.Fatalf("cache dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("cache entries = %d, want 1", len(entries))
	}
	data, err := os.ReadFile(filepath.Join(internalDir, "tailwind", "cache", entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	nl := strings.IndexByte(string(data), '\n')
	if nl < 0 {
		t.Fatalf("cache file has no fingerprint line: %q", string(data))
	}
	if got, want := string(data[:nl]), tailwind.Fingerprint([]byte(second)); got != want {
		t.Errorf("cache fingerprint = %q, want %q", got, want)
	}
}

func TestTailwindNotModified(t *testing.T) {
	body := page(`<div class="p-4"></div>`)
	fx := setupFileTest(t, "site.htmlclay", body)
	fake := &fakeTailwind{}
	fx.srv.setTailwindCompiler(fake.compile)

	w := tailwindGet(t, fx.srv, "/tailwindcss/site.css", nil)
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}
	if fake.calls() != 1 {
		t.Fatalf("compile calls = %d, want 1", fake.calls())
	}

	for _, inm := range []string{etag, "W/" + etag, `"x", ` + etag} {
		w = tailwindGet(t, fx.srv, "/tailwindcss/site.css", map[string]string{"If-None-Match": inm})
		if w.Code != 304 {
			t.Fatalf("If-None-Match %q: got %d, want 304", inm, w.Code)
		}
		if w.Body.Len() != 0 {
			t.Errorf("If-None-Match %q: 304 carried a body: %q", inm, w.Body.String())
		}
	}
	if fake.calls() != 1 {
		t.Errorf("compile calls = %d after three 304s, want 1", fake.calls())
	}
}

func TestTailwindMissingDocumentIsEmpty(t *testing.T) {
	fx := setupFileTest(t, "site.htmlclay", page(`<div class="p-4"></div>`))
	fake := &fakeTailwind{}
	fx.srv.setTailwindCompiler(fake.compile)

	w := tailwindGet(t, fx.srv, "/tailwindcss/nope.css", nil)
	if w.Code != 200 {
		t.Fatalf("got %d, want 200", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", w.Body.String())
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if fake.calls() != 0 {
		t.Errorf("compiled %d times, want 0", fake.calls())
	}
}

func TestTailwindCompileFailureIsEmptyAndLoggedOnce(t *testing.T) {
	fx := setupFileTest(t, "site.htmlclay", page(`<div class="p-4"></div>`))
	logPath := filepath.Join(t.TempDir(), "test.log")
	logger, err := logging.New(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logger.Close() })
	fx.srv.logger = logger

	fake := &fakeTailwind{fail: true, err: fmt.Errorf("compiler exploded")}
	fx.srv.setTailwindCompiler(fake.compile)

	for i := 0; i < 2; i++ {
		w := tailwindGet(t, fx.srv, "/tailwindcss/site.css", nil)
		if w.Code != 200 {
			t.Fatalf("request %d: got %d, want 200", i+1, w.Code)
		}
		if w.Body.Len() != 0 {
			t.Errorf("request %d: body = %q, want empty", i+1, w.Body.String())
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("request %d: Cache-Control = %q, want no-store", i+1, cc)
		}
	}

	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(logged), "Tailwind: "); n != 1 {
		t.Errorf("failure logged %d times, want 1: %q", n, string(logged))
	}
}

func TestTailwindRefusesOutOfScope(t *testing.T) {
	fx := setupFileTest(t, "site.htmlclay", page(`<div class="p-4"></div>`))
	outside := filepath.Join(fx.home, "outside")
	if err := os.MkdirAll(outside, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "x.htmlclay"), []byte(page(`<div class="x"></div>`)), 0644); err != nil {
		t.Fatal(err)
	}
	fake := &fakeTailwind{}
	fx.srv.setTailwindCompiler(fake.compile)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- tailwindGet(t, fx.srv, "/tailwindcss/outside/x.css", nil)
	}()

	select {
	case w := <-done:
		if w.Code != 200 {
			t.Fatalf("got %d, want 200", w.Code)
		}
		if w.Body.Len() != 0 {
			t.Errorf("body = %q, want empty", w.Body.String())
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", cc)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request parked instead of refusing an out-of-scope document")
	}

	if fake.calls() != 0 {
		t.Errorf("compiled %d times, want 0", fake.calls())
	}
}

func TestTailwindPercentNameNotDoubleDecoded(t *testing.T) {
	body := page(`<div class="pct"></div>`)
	fx := setupFileTest(t, "site.htmlclay", page(`<div class="p-4"></div>`))
	registerPageWithContent(t, fx.srv, "50% off.htmlclay", body)
	fake := &fakeTailwind{}
	fx.srv.setTailwindCompiler(fake.compile)

	w := tailwindGet(t, fx.srv, "/tailwindcss/50%25%20off.css", nil)
	if w.Code != 200 {
		t.Fatalf("got %d, want 200", w.Code)
	}
	if got, want := w.Body.String(), "/*css*/"+body; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestTailwindRefusesHiddenAndInternal(t *testing.T) {
	fx := setupFileTest(t, "site.htmlclay", page(`<div class="p-4"></div>`))
	hidden := filepath.Join(fx.home, ".secret")
	if err := os.MkdirAll(hidden, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hidden, "x.htmlclay"), []byte(page(`<div class="hidden"></div>`)), 0644); err != nil {
		t.Fatal(err)
	}
	fake := &fakeTailwind{}
	fx.srv.setTailwindCompiler(fake.compile)

	w := tailwindGet(t, fx.srv, "/tailwindcss/.secret/x.css", nil)
	if w.Code != 200 {
		t.Fatalf("got %d, want 200", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", w.Body.String())
	}
	if fake.calls() != 0 {
		t.Errorf("compiled %d times, want 0", fake.calls())
	}
}

func TestTailwindLeavesOtherFilesInTailwindFolder(t *testing.T) {
	body := page(`<div class="folder-page">folder page</div>`)
	fx := setupFileTest(t, "site.htmlclay", page(`<div class="p-4"></div>`))
	if err := os.MkdirAll(filepath.Join(fx.home, "tailwindcss"), 0755); err != nil {
		t.Fatal(err)
	}
	registerPageWithContent(t, fx.srv, "tailwindcss/page.htmlclay", body)
	fake := &fakeTailwind{}
	fx.srv.setTailwindCompiler(fake.compile)

	w := tailwindGet(t, fx.srv, "/tailwindcss/page.htmlclay", nil)
	if w.Code != 200 {
		t.Fatalf("got %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/css") {
		t.Errorf("Content-Type = %q, want the document's own type", ct)
	}
	if got := w.Body.String(); !strings.Contains(got, "folder page") {
		t.Errorf("body = %q, want the page's own text", got)
	}
	if fake.calls() != 0 {
		t.Errorf("compiled %d times, want 0", fake.calls())
	}
}

// A request that goes away does not abort the compile the next request waits
// on, and that compile is still cached for it.
func TestTailwindCompileSurvivesCancelledRequest(t *testing.T) {
	body := page(`<div class="p-4"></div>`)
	fx := setupFileTest(t, "site.htmlclay", body)
	fx.srv.SetInternalDir(t.TempDir())

	var mu sync.Mutex
	calls := 0
	var compileErr error
	fx.srv.setTailwindCompiler(func(ctx context.Context, html []byte) ([]byte, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		time.Sleep(200 * time.Millisecond)
		mu.Lock()
		compileErr = ctx.Err()
		mu.Unlock()
		return []byte("/*css*/" + string(html)), nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "/tailwindcss/site.css", nil).WithContext(ctx)
	req.Host = fmt.Sprintf("127.0.0.1:%d", fx.srv.port)
	go func() {
		fx.srv.httpServer.Handler.ServeHTTP(httptest.NewRecorder(), req)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	time.Sleep(350 * time.Millisecond)

	w := tailwindGet(t, fx.srv, "/tailwindcss/site.css", nil)
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("compile calls = %d, want 1", calls)
	}
	if compileErr != nil {
		t.Errorf("the compile saw ctx.Err() = %v, want nil", compileErr)
	}
	if got, want := w.Body.String(), "/*css*/"+body; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestTailwindUnavailableLoggedOnceAndCancelledQuiet(t *testing.T) {
	fx := setupFileTest(t, "a.htmlclay", page(`<div class="a"></div>`))
	registerPageWithContent(t, fx.srv, "b.htmlclay", page(`<div class="b"></div>`))
	logPath := filepath.Join(t.TempDir(), "test.log")
	logger, err := logging.New(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logger.Close() })
	fx.srv.logger = logger

	fake := &fakeTailwind{fail: true, err: tailwind.ErrUnavailable}
	fx.srv.setTailwindCompiler(fake.compile)

	for _, target := range []string{"/tailwindcss/a.css", "/tailwindcss/b.css"} {
		w := tailwindGet(t, fx.srv, target, nil)
		if w.Code != 200 {
			t.Fatalf("%s: got %d, want 200", target, w.Code)
		}
		if w.Body.Len() != 0 {
			t.Errorf("%s: body = %q, want empty", target, w.Body.String())
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want no-store", target, cc)
		}
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(logged), "Tailwind: "); n != 1 {
		t.Errorf("a build with no compiler logged %d lines for two documents, want 1: %q", n, string(logged))
	}

	// A request whose context is already cancelled is not a compile failure.
	cancelled := &fakeTailwind{fail: true, err: context.Canceled}
	fx.srv.setTailwindCompiler(cancelled.compile)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := fx.srv.tailwindStylesheet(ctx, fx.file.AbsPath, []byte(page(`<div class="c"></div>`))); err == nil {
		t.Fatal("a cancelled compile returned no error")
	}
	logged, err = os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(logged), "Tailwind: "); n != 1 {
		t.Errorf("a cancelled request logged %d lines, want the 1 already there: %q", n, string(logged))
	}
}

func TestTailwindRegisteredSizeLimit(t *testing.T) {
	fx := setupFileTest(t, "site.htmlclay", page(`<div class="p-4"></div>`))
	fake := &fakeTailwind{}
	fx.srv.setTailwindCompiler(fake.compile)

	if err := os.Truncate(fx.file.AbsPath, maxTailwindSource+1); err != nil {
		t.Fatal(err)
	}

	w := tailwindGet(t, fx.srv, "/tailwindcss/site.css", nil)
	if w.Code != 200 {
		t.Fatalf("got %d, want 200", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", w.Body.String())
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if fake.calls() != 0 {
		t.Errorf("compiled %d times, want 0", fake.calls())
	}
}

func TestTailwindRefusesSymlinkInsideHiddenFolder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks needs a privilege on Windows")
	}
	body := page(`<div class="p-4"></div>`)
	fx := setupFileTest(t, "doc.htmlclay", body)
	hidden := filepath.Join(fx.home, ".hidden")
	if err := os.MkdirAll(hidden, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "doc.htmlclay"), filepath.Join(hidden, "alias.htmlclay")); err != nil {
		t.Fatal(err)
	}
	fake := &fakeTailwind{}
	fx.srv.setTailwindCompiler(fake.compile)

	w := tailwindGet(t, fx.srv, "/tailwindcss/.hidden/alias.css", nil)
	if w.Code != 200 {
		t.Fatalf("got %d, want 200", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", w.Body.String())
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if fake.calls() != 0 {
		t.Errorf("compiled %d times, want 0", fake.calls())
	}
}

func TestTailwindCompilesOneAtATimeAcrossServers(t *testing.T) {
	var inFlight, maxInFlight int32
	compiler := func(tag string) func(context.Context, []byte) ([]byte, error) {
		return func(_ context.Context, html []byte) ([]byte, error) {
			n := atomic.AddInt32(&inFlight, 1)
			for {
				max := atomic.LoadInt32(&maxInFlight)
				if n <= max || atomic.CompareAndSwapInt32(&maxInFlight, max, n) {
					break
				}
			}
			time.Sleep(100 * time.Millisecond)
			atomic.AddInt32(&inFlight, -1)
			return []byte("/*" + tag + "*/" + string(html)), nil
		}
	}

	first := page(`<div class="first"></div>`)
	second := page(`<div class="second"></div>`)
	fx1 := setupFileTest(t, "site.htmlclay", first)
	fx2 := setupFileTest(t, "site.htmlclay", second)
	fx1.srv.setTailwindCompiler(compiler("one"))
	fx2.srv.setTailwindCompiler(compiler("two"))

	results := make([]*httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for i, fx := range []*fileFixture{fx1, fx2} {
		wg.Add(1)
		go func(i int, fx *fileFixture) {
			defer wg.Done()
			results[i] = tailwindGet(t, fx.srv, "/tailwindcss/site.css", nil)
		}(i, fx)
	}
	wg.Wait()

	if got := atomic.LoadInt32(&maxInFlight); got != 1 {
		t.Errorf("compiles in flight at once = %d, want 1", got)
	}
	if got, want := results[0].Body.String(), "/*one*/"+first; got != want {
		t.Errorf("first server: body = %q, want %q", got, want)
	}
	if got, want := results[1].Body.String(), "/*two*/"+second; got != want {
		t.Errorf("second server: body = %q, want %q", got, want)
	}
}
