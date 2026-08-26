package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/panphora/htmlclay/config"
	"github.com/panphora/htmlclay/internal/testutil"
)

// safeBuffer is a buffer a test reads while a subcommand goroutine writes it.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type wireHarness struct {
	env    *wireEnv
	out    *safeBuffer
	errs   *safeBuffer
	cancel context.CancelFunc
}

// newWireHarness points a CLI at a config directory. Every long-running
// subcommand ends through the harness context, because the production one is
// cancelled by a signal and signalling the test binary would end the suite.
func newWireHarness(t *testing.T, cfgBase string) *wireHarness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := &wireHarness{out: &safeBuffer{}, errs: &safeBuffer{}, cancel: cancel}
	h.env = &wireEnv{
		configPath: filepath.Join(config.DirFrom(cfgBase), "config.json"),
		stdin:      bytes.NewReader(nil),
		stdout:     h.out,
		stderr:     h.errs,
		ctx:        ctx,
	}
	return h
}

// run drives a subcommand that returns on its own.
func (h *wireHarness) run(args ...string) int {
	return runWire(h.env, args)
}

// background drives one that does not, and reports its exit code once it stops.
func (h *wireHarness) background(args ...string) <-chan int {
	done := make(chan int, 1)
	go func() { done <- runWire(h.env, args) }()
	return done
}

func waitForText(t *testing.T, b *safeBuffer, want string) {
	t.Helper()
	testutil.Eventually(t, 15*time.Second, testutil.Lazy(func() string {
		return fmt.Sprintf("%q in output; saw:\n%s", want, b.String())
	}), func() bool {
		return strings.Contains(b.String(), want)
	})
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	testutil.Eventually(t, 15*time.Second, path, func() bool {
		_, err := os.Stat(path)
		return err == nil
	})
}

// fakeOrigin binds a loopback server the CLI can be pointed at, and returns its
// port. It is how a stranger holding a remembered port is tested: a real site
// cannot be made to answer 500 or to redirect.
func fakeOrigin(t *testing.T, h http.Handler) int {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	_, portStr, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("cannot read the test server's port from %s: %v", srv.URL, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func waitForExit(t *testing.T, done <-chan int) int {
	t.Helper()
	// 15s rather than the in-process 10s: this one waits on process startup and
	// signal delivery.
	return testutil.Receive(t, 15*time.Second, "the subcommand to exit", done)
}

// wireFrames parses a listener's stdout, which is one JSON envelope per line.
func wireFrames(t *testing.T, b *safeBuffer) []wireFrame {
	t.Helper()
	var out []wireFrame
	for _, line := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		if line == "" {
			continue
		}
		var f wireFrame
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			t.Fatalf("stdout line is not an envelope: %q (%v)", line, err)
		}
		out = append(out, f)
	}
	return out
}

func hasFrame(frames []wireFrame, typ, id string) bool {
	for _, f := range frames {
		if f.Type == typ && f.ID == id {
			return true
		}
	}
	return false
}

// openTestSite brings up a real app with one file open and returns the file's
// resolved path, its site, and the config base the CLI must read.
func openTestSite(t *testing.T) (file string, s *site, cfgBase string) {
	t.Helper()
	home, _ := filepath.EvalSymlinks(t.TempDir())
	file = filepath.Join(home, "proj", "index.htmlclay")
	writeTestFile(t, file, "<html><body>wire</body></html>")
	cfgBase = t.TempDir()
	a := newTestAppWithConfigDir(t, home, cfgBase)
	s, _ = a.openForTest(t, file)
	return file, s, cfgBase
}

// The probe order mirrors the anchor rule (broadest ancestor first) without
// reimplementing it, and every other remembered port still gets asked, because a
// stale hint must not make a live origin unreachable.
func TestWireCandidatesProbeOrder(t *testing.T) {
	base := filepath.Join(string(filepath.Separator), "home", "u")
	ports := map[string]int{
		filepath.Join(base, "proj"):         1001,
		filepath.Join(base, "proj", "deep"): 1002,
		filepath.Join(base, "other"):        1003,
		filepath.Join(base, "aaa"):          1004,
		filepath.Join(base, "bad"):          0,
	}
	got := wireCandidates(ports, filepath.Join(base, "proj", "deep", "index.htmlclay"))
	want := []int{1001, 1002, 1004, 1003}
	if len(got) != len(want) {
		t.Fatalf("got %d candidates, want %d: %+v", len(got), len(want), got)
	}
	for i, c := range got {
		if c.port != want[i] {
			t.Fatalf("candidate %d is port %d, want %d (%+v)", i, c.port, want[i], got)
		}
	}
}

// The CLI must never repair a config it cannot parse. config.Load renames a
// corrupt file aside, and a wire invocation racing the app doing that would
// quarantine the live config.
func TestWireSitePortsLeavesACorruptConfigAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	garbage := []byte("{not json at all")
	if err := os.WriteFile(path, garbage, 0600); err != nil {
		t.Fatal(err)
	}
	if ports := wireSitePorts(path); ports != nil {
		t.Fatalf("a corrupt config must yield no hint, got %+v", ports)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the config was moved or removed: %v", err)
	}
	if !bytes.Equal(after, garbage) {
		t.Fatalf("the config was rewritten: %q", after)
	}
}

func TestWireWhereFindsTheServingOrigin(t *testing.T) {
	file, s, cfgBase := openTestSite(t)
	h := newWireHarness(t, cfgBase)

	if code := h.run("where", file); code != wireExitOK {
		t.Fatalf("where exited %d; stderr:\n%s", code, h.errs.String())
	}
	var got struct {
		File   string `json:"file"`
		Origin string `json:"origin"`
		Anchor string `json:"anchor"`
		State  string `json:"state"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(h.out.String())), &got); err != nil {
		t.Fatalf("where printed %q, which is not JSON: %v", h.out.String(), err)
	}
	if want := fmt.Sprintf("http://127.0.0.1:%d", s.port); got.Origin != want {
		t.Errorf("origin = %q, want %q", got.Origin, want)
	}
	if got.File != file {
		t.Errorf("file = %q, want %q", got.File, file)
	}
	if got.Anchor != filepath.Dir(file) {
		t.Errorf("anchor = %q, want %q", got.Anchor, filepath.Dir(file))
	}
	if got.State != "live" {
		t.Errorf("state = %q, want live", got.State)
	}
}

// A live site that does not know the file is a different answer from no app at
// all, and a script has to be able to tell them apart.
func TestWireWhereSeparatesUnknownFileFromNoApp(t *testing.T) {
	file, _, cfgBase := openTestSite(t)
	other := filepath.Join(filepath.Dir(filepath.Dir(file)), "elsewhere", "other.htmlclay")
	writeTestFile(t, other, "<html><body>other</body></html>")

	h := newWireHarness(t, cfgBase)
	if code := h.run("where", other); code != wireExitNoFile {
		t.Fatalf("where on an unopened file exited %d, want %d; stderr:\n%s",
			code, wireExitNoFile, h.errs.String())
	}

	empty := newWireHarness(t, t.TempDir())
	if code := empty.run("where", other); code != wireExitAppDown {
		t.Fatalf("where with no app exited %d, want %d", code, wireExitAppDown)
	}
}

// The parked recovery listener answers 404 on every path, exactly as a live site
// does for a file it does not hold. Content type is what separates them, and the
// CLI must report "nothing is open here" rather than "no such file".
func TestWireWhereReportsTheRecoveryPage(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	anchor := filepath.Join(home, "proj")
	file := filepath.Join(anchor, "index.htmlclay")
	writeTestFile(t, file, "<html><body>parked</body></html>")

	cfgBase := t.TempDir()
	a := newTestAppWithConfigDir(t, home, cfgBase)

	port := parkTestPort(t, a, anchor)
	a.rt.cfg.RememberSitePort(anchor, port)
	if err := a.rt.cfg.Save(); err != nil {
		t.Fatal(err)
	}

	h := newWireHarness(t, cfgBase)
	if code := h.run("where", file); code != wireExitRecovery {
		t.Fatalf("where exited %d, want %d; stderr:\n%s", code, wireExitRecovery, h.errs.String())
	}
}

// A send nobody took is not a success. The reply still prints, so a caller that
// reads it sees delivered: 0 either way.
func TestWireSendWithNoHandlerAttached(t *testing.T) {
	file, _, cfgBase := openTestSite(t)
	h := newWireHarness(t, cfgBase)

	code := h.run("send", "--type", "wire/request", "--id", "req-1", file)
	if code != wireExitNotDelivered {
		t.Fatalf("send exited %d, want %d; stderr:\n%s", code, wireExitNotDelivered, h.errs.String())
	}
	if !strings.Contains(h.out.String(), `"delivered":0`) {
		t.Fatalf("send should print the reply, got %q", h.out.String())
	}
}

func TestWireSendRejectsANonWireType(t *testing.T) {
	file, _, cfgBase := openTestSite(t)
	h := newWireHarness(t, cfgBase)
	if code := h.run("send", "--type", "rewrite", file); code != wireExitUsage {
		t.Fatalf("send exited %d, want %d", code, wireExitUsage)
	}
}

// The handler slot is exclusive, and the second process must learn that from an
// exit code rather than a hang.
func TestWireListenRefusesASecondHandler(t *testing.T) {
	file, _, cfgBase := openTestSite(t)

	first := newWireHarness(t, cfgBase)
	done := first.background("listen", "--handler", file)
	waitForText(t, first.errs, "attached as handler")

	second := newWireHarness(t, cfgBase)
	if code := second.run("listen", "--handler", file); code != wireExitHandlerTaken {
		t.Fatalf("the second handler exited %d, want %d; stderr:\n%s",
			code, wireExitHandlerTaken, second.errs.String())
	}

	first.cancel()
	waitForExit(t, done)
}

// The acceptance test for the CLI: a request reaches a real command, its output
// becomes the request's status frames, and its exit becomes the terminal frame.
func TestWireServeRunsTheHandlerCommand(t *testing.T) {
	file, _, cfgBase := openTestSite(t)
	envelopePath := filepath.Join(t.TempDir(), "envelope.json")
	t.Setenv("HTMLCLAY_WIRE_HELPER", "1")
	t.Setenv("HTMLCLAY_WIRE_HELPER_MODE", "echo")
	t.Setenv("HTMLCLAY_WIRE_HELPER_OUT", envelopePath)

	server := newWireHarness(t, cfgBase)
	serving := server.background(append([]string{"serve", file, "--"}, wireHelperCommand()...)...)
	waitForText(t, server.errs, "attached as handler")

	watcher := newWireHarness(t, cfgBase)
	watching := watcher.background("listen", file)
	waitForText(t, watcher.errs, "attached as observer")

	sender := newWireHarness(t, cfgBase)
	sender.env.stdin = strings.NewReader(`{"kind":"rewrite"}`)
	if code := sender.run("send", "--type", "wire/request", "--id", "req-42", file); code != wireExitOK {
		t.Fatalf("send exited %d; stderr:\n%s", code, sender.errs.String())
	}

	waitForText(t, watcher.out, `"wire/done"`)
	frames := wireFrames(t, watcher.out)
	if !hasFrame(frames, "wire/ack", "req-42") {
		t.Errorf("no ack for req-42:\n%s", watcher.out.String())
	}
	if !hasFrame(frames, "wire/done", "req-42") {
		t.Errorf("no done for req-42:\n%s", watcher.out.String())
	}
	var sawStatus bool
	for _, f := range frames {
		if f.Type == "wire/status" && f.ID == "req-42" && strings.Contains(f.Text, "working") {
			sawStatus = true
		}
		if f.From == "" {
			t.Errorf("the server must stamp from on every frame: %+v", f)
		}
		if f.File != file {
			t.Errorf("frame carries file %q, want %q", f.File, file)
		}
	}
	if !sawStatus {
		t.Errorf("a line the command printed did not become a status frame:\n%s", watcher.out.String())
	}

	// The command is handed the whole request, payload included, so a handler
	// that needs to know what was asked can read it from stdin alone.
	got, err := os.ReadFile(envelopePath)
	if err != nil {
		t.Fatalf("the command did not receive the envelope on stdin: %v", err)
	}
	var received wireFrame
	if err := json.Unmarshal(got, &received); err != nil {
		t.Fatalf("stdin was not an envelope: %q", got)
	}
	if received.ID != "req-42" || string(received.Payload) != `{"kind":"rewrite"}` {
		t.Errorf("the command received %+v, want req-42 with the sent payload", received)
	}

	server.cancel()
	watcher.cancel()
	waitForExit(t, serving)
	waitForExit(t, watching)
}

func TestWireServeReportsANonZeroExitAsError(t *testing.T) {
	file, _, cfgBase := openTestSite(t)
	t.Setenv("HTMLCLAY_WIRE_HELPER", "1")
	t.Setenv("HTMLCLAY_WIRE_HELPER_MODE", "fail")

	server := newWireHarness(t, cfgBase)
	serving := server.background(append([]string{"serve", file, "--"}, wireHelperCommand()...)...)
	waitForText(t, server.errs, "attached as handler")

	watcher := newWireHarness(t, cfgBase)
	watching := watcher.background("listen", file)
	waitForText(t, watcher.errs, "attached as observer")

	sender := newWireHarness(t, cfgBase)
	if code := sender.run("send", "--type", "wire/request", "--id", "req-fail", file); code != wireExitOK {
		t.Fatalf("send exited %d; stderr:\n%s", code, sender.errs.String())
	}

	waitForText(t, watcher.out, `"wire/error"`)
	if hasFrame(wireFrames(t, watcher.out), "wire/done", "req-fail") {
		t.Error("a command that exited non-zero must not report done")
	}
	// The command's own stderr reaches the terminal the handler runs in, tagged
	// with the request it belongs to.
	waitForText(t, server.errs, "handler blew up")

	server.cancel()
	watcher.cancel()
	waitForExit(t, serving)
	waitForExit(t, watching)
}

// Cancel has to reach the process, not just stop reporting about it.
func TestWireServeCancelStopsTheChild(t *testing.T) {
	file, _, cfgBase := openTestSite(t)
	started := filepath.Join(t.TempDir(), "started")
	t.Setenv("HTMLCLAY_WIRE_HELPER", "1")
	t.Setenv("HTMLCLAY_WIRE_HELPER_MODE", "sleep")
	t.Setenv("HTMLCLAY_WIRE_HELPER_OUT", started)

	server := newWireHarness(t, cfgBase)
	serving := server.background(append([]string{"serve", file, "--"}, wireHelperCommand()...)...)
	waitForText(t, server.errs, "attached as handler")

	watcher := newWireHarness(t, cfgBase)
	watching := watcher.background("listen", file)
	waitForText(t, watcher.errs, "attached as observer")

	sender := newWireHarness(t, cfgBase)
	if code := sender.run("send", "--type", "wire/request", "--id", "req-slow", file); code != wireExitOK {
		t.Fatalf("send exited %d; stderr:\n%s", code, sender.errs.String())
	}
	waitForText(t, watcher.out, `"wire/ack"`)

	waitForFile(t, started)

	canceller := newWireHarness(t, cfgBase)
	// The cancel is delivered to the handler, so it counts as delivered.
	if code := canceller.run("send", "--type", "wire/cancel", "--id", "req-slow", file); code != wireExitOK {
		t.Fatalf("cancel exited %d; stderr:\n%s", code, canceller.errs.String())
	}

	waitForText(t, watcher.out, `"wire/error"`)
	var cancelled bool
	for _, f := range wireFrames(t, watcher.out) {
		if f.Type == "wire/error" && f.ID == "req-slow" && strings.Contains(f.Text, "cancelled") {
			cancelled = true
		}
	}
	if !cancelled {
		t.Errorf("cancel should end the request as cancelled:\n%s", watcher.out.String())
	}

	// The frame alone proves nothing: it is written from ctx.Err() and would
	// appear just the same if the signal never reached the process. The child
	// records the signal itself, so this is what separates "cancel works" from
	// "cancel gave up waiting".
	if runtime.GOOS != "windows" {
		waitForFile(t, started+".stopped")
	}

	server.cancel()
	watcher.cancel()
	waitForExit(t, serving)
	waitForExit(t, watching)
}

func TestWireServeNeedsACommand(t *testing.T) {
	file, _, cfgBase := openTestSite(t)
	h := newWireHarness(t, cfgBase)
	if code := h.run("serve", file); code != wireExitUsage {
		t.Fatalf("serve with no command exited %d, want %d", code, wireExitUsage)
	}
	if code := h.run("serve", file, "--"); code != wireExitUsage {
		t.Fatalf("serve with an empty command exited %d, want %d", code, wireExitUsage)
	}
}

// Dispatch must claim the wire subcommand and nothing else: every other argv is
// still a file to open.
func TestWireDispatchOnlyClaimsWireArgv(t *testing.T) {
	if _, handled := wireDispatch([]string{"htmlclay", "/tmp/some.htmlclay"}); handled {
		t.Error("an ordinary file argument must not be claimed by the wire CLI")
	}
	if _, handled := wireDispatch([]string{"htmlclay"}); handled {
		t.Error("a bare launch must not be claimed by the wire CLI")
	}
}

func TestWireReadsAFrameOffTheStream(t *testing.T) {
	stream := "id: 7\ndata: {\"type\":\"wire/status\",\"id\":\"a\",\"text\":\"one\"}\n\n" +
		": keepalive\n\n" +
		"id: 8\ndata: {\"type\":\"wire/done\",\"id\":\"a\"}\n\n"
	var got []wireFrame
	last := wireReadStream(strings.NewReader(stream), "", func(_ []byte, f wireFrame) {
		got = append(got, f)
	})
	if len(got) != 2 || got[0].Text != "one" || got[1].Type != "wire/done" {
		t.Fatalf("parsed %+v, want a status then a done", got)
	}
	if last != "8" {
		t.Fatalf("resume id = %q, want 8", last)
	}
}

// parkTestPort binds a free port with the real recovery listener. Choosing a
// port and binding it are two steps, so another process can take it in between;
// this verifies the listener actually answers and retries rather than leaving a
// race in the suite.
func parkTestPort(t *testing.T, a *app, anchor string) int {
	t.Helper()
	for attempt := 0; attempt < 5; attempt++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		ln.Close()

		a.parkPort(anchor, port)
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/_/wire/subscribe", port))
		if err == nil {
			ct := resp.Header.Get("Content-Type")
			resp.Body.Close()
			if resp.StatusCode == http.StatusNotFound && strings.Contains(ct, "text/html") {
				return port
			}
		}
		a.unpark(anchor)
	}
	t.Fatal("could not bind a recovery listener on a free port")
	return 0
}

// Flags on either side of the file must both work. The documented form puts the
// file first, and flag.Parse stops at the first non-flag argument, so this is
// the form that silently did not work while every test used the other one.
func TestWireAcceptsFlagsOnEitherSideOfTheFile(t *testing.T) {
	file, s, cfgBase := openTestSite(t)

	fileFirst := newWireHarness(t, cfgBase)
	if code := fileFirst.run("where", file, "--port", strconv.Itoa(s.port)); code != wireExitOK {
		t.Fatalf("`where <file> --port` exited %d; stderr:\n%s", code, fileFirst.errs.String())
	}
	flagsFirst := newWireHarness(t, cfgBase)
	if code := flagsFirst.run("where", "--port", strconv.Itoa(s.port), file); code != wireExitOK {
		t.Fatalf("`where --port <file>` exited %d; stderr:\n%s", code, flagsFirst.errs.String())
	}

	sender := newWireHarness(t, cfgBase)
	// wire/request with no handler attached still proves the frame was parsed and
	// posted, which is what the flag order decides.
	if code := sender.run("send", file, "--type", "wire/request", "--id", "flag-order"); code != wireExitNotDelivered {
		t.Fatalf("`send <file> --type ...` exited %d, want %d; stderr:\n%s",
			code, wireExitNotDelivered, sender.errs.String())
	}
}

// --port is the way to reach a file whose folder HTML Clay remembers no port
// for, which is every file loose in the home directory.
func TestWirePortFlagReachesAnUnrememberedOrigin(t *testing.T) {
	file, s, _ := openTestSite(t)
	empty := newWireHarness(t, t.TempDir()) // no config at all
	if code := empty.run("where", file, "--port", strconv.Itoa(s.port)); code != wireExitOK {
		t.Fatalf("where with --port and no config exited %d; stderr:\n%s", code, empty.errs.String())
	}
	if !strings.Contains(empty.out.String(), fmt.Sprintf("127.0.0.1:%d", s.port)) {
		t.Errorf("where printed %q, which does not name the port it was given", empty.out.String())
	}
}

// A remembered port can be held by something that is not HTML Clay. Following
// its redirect would repost the envelope, with the absolute path of a local file
// and whatever the page sent, to wherever it points.
func TestWireNeverFollowsARedirect(t *testing.T) {
	var collected atomic.Int32
	collector := fakeOrigin(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		collected.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"delivered":1,"observers":0}`))
	}))
	redirector := fakeOrigin(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, fmt.Sprintf("http://127.0.0.1:%d/collect", collector), http.StatusTemporaryRedirect)
	}))

	home, _ := filepath.EvalSymlinks(t.TempDir())
	file := filepath.Join(home, "proj", "index.htmlclay")
	writeTestFile(t, file, "<html><body>x</body></html>")

	h := newWireHarness(t, t.TempDir())
	code := h.run("send", file, "--port", strconv.Itoa(redirector), "--type", "wire/request", "--id", "redir")
	if code == wireExitOK || code == wireExitNotDelivered {
		t.Fatalf("a redirect must not be treated as a delivery, got exit %d", code)
	}
	if n := collected.Load(); n != 0 {
		t.Fatalf("the envelope was reposted off-origin %d times", n)
	}
}

// One stranger on a remembered port must not hide the live origin behind it. A
// broader ancestor is probed first, so a 500 there used to end the walk.
func TestWireWalksPastAStrangerOnARememberedPort(t *testing.T) {
	file, s, cfgBase := openTestSite(t)
	home := filepath.Dir(filepath.Dir(file))

	stranger := fakeOrigin(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not me", http.StatusInternalServerError)
	}))
	// Remembered under the home directory, which contains the file, so it sorts
	// ahead of the file's own anchor.
	a := newTestAppWithConfigDir(t, home, cfgBase)
	a.rt.cfg.RememberSitePort(home, stranger)
	if err := a.rt.cfg.Save(); err != nil {
		t.Fatal(err)
	}

	h := newWireHarness(t, cfgBase)
	if code := h.run("where", file); code != wireExitOK {
		t.Fatalf("where exited %d, want 0; a stranger on a broader anchor hid the origin\nstderr:\n%s",
			code, h.errs.String())
	}
	if !strings.Contains(h.out.String(), fmt.Sprintf("127.0.0.1:%d", s.port)) {
		t.Errorf("where printed %q, want the real site on port %d", h.out.String(), s.port)
	}
}

// The CLI must attest nothing: no Origin, no Sec-Fetch-Site. Those two headers
// are exactly what the server's guard reads to tell a browser from a local
// process, and a stream that starts sending one gets a 403 with no other symptom.
func TestWireSendsNoBrowserHeaders(t *testing.T) {
	var mu sync.Mutex
	var seen http.Header
	origin := fakeOrigin(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = r.Header.Clone()
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))

	home, _ := filepath.EvalSymlinks(t.TempDir())
	file := filepath.Join(home, "proj", "index.htmlclay")
	writeTestFile(t, file, "<html><body>x</body></html>")

	h := newWireHarness(t, t.TempDir())
	done := h.background("listen", file, "--port", strconv.Itoa(origin))
	waitForText(t, h.errs, "attached as observer")

	mu.Lock()
	got := seen
	mu.Unlock()
	for _, header := range []string{"Origin", "Sec-Fetch-Site", "Sec-Fetch-Mode"} {
		if v := got.Get(header); v != "" {
			t.Errorf("the CLI sent %s: %q; the guard reads that as a browser", header, v)
		}
	}

	h.cancel()
	waitForExit(t, done)
}

// A handler printing one very long line is ordinary. The line is truncated; the
// request still finishes.
func TestWireServeSurvivesAnOversizedHandlerLine(t *testing.T) {
	file, _, cfgBase := openTestSite(t)
	t.Setenv("HTMLCLAY_WIRE_HELPER", "1")
	t.Setenv("HTMLCLAY_WIRE_HELPER_MODE", "long")

	server := newWireHarness(t, cfgBase)
	serving := server.background(append([]string{"serve", file, "--"}, wireHelperCommand()...)...)
	waitForText(t, server.errs, "attached as handler")

	watcher := newWireHarness(t, cfgBase)
	watching := watcher.background("listen", file)
	waitForText(t, watcher.errs, "attached as observer")

	sender := newWireHarness(t, cfgBase)
	if code := sender.run("send", file, "--type", "wire/request", "--id", "req-long"); code != wireExitOK {
		t.Fatalf("send exited %d; stderr:\n%s", code, sender.errs.String())
	}

	waitForText(t, watcher.out, `"wire/done"`)
	for _, f := range wireFrames(t, watcher.out) {
		if f.Type == "wire/status" && len(f.Text) > wireMaxStatus {
			t.Errorf("a status frame carried %d bytes, past the %d cap", len(f.Text), wireMaxStatus)
		}
	}

	server.cancel()
	watcher.cancel()
	waitForExit(t, serving)
	waitForExit(t, watching)
}

// reapOrphanDescendant makes the test responsible for the descendant the orphan
// fixtures leave running.
//
// Those descendants are deliberately unreachable from the CLI, which is the whole
// behaviour under test. Nothing made them reachable from the TEST either, so they
// sat out their own 20s timer holding this test binary open. On Windows that is
// fatal to the run rather than untidy: `go test` cannot unlink htmlclay.test.exe
// while a copy of it is still running, so the job fails at cleanup after every
// package has passed. It stayed hidden for as long as the rest of the suite
// happened to take longer than the timer, and surfaced the moment it got faster.
// Reaping is bounded by the test rather than by a race between two durations, so
// making the suite faster again cannot bring it back.
func reapOrphanDescendant(t *testing.T) {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "orphan.pid")
	t.Setenv("HTMLCLAY_WIRE_HELPER_PIDFILE", pidFile)
	t.Cleanup(func() {
		raw, err := os.ReadFile(pidFile)
		if err != nil {
			// The fixture never got as far as spawning one, which several failure
			// paths through these tests reach legitimately.
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil {
			t.Errorf("orphan pid file holds %q, want a pid", raw)
			return
		}
		proc, err := os.FindProcess(pid)
		if err != nil {
			return
		}
		// No Wait: the descendant is a grandchild, reparented when the handler
		// exited, so this process cannot reap it and does not need to.
		_ = proc.Kill()
	})
}

// A handler that leaves something behind holding stdout must not hold the
// request open forever with it. This is the case where draining to EOF before
// waiting never returns.
func TestWireServeSurvivesADescendantHoldingStdout(t *testing.T) {
	file, _, cfgBase := openTestSite(t)
	t.Setenv("HTMLCLAY_WIRE_HELPER", "1")
	t.Setenv("HTMLCLAY_WIRE_HELPER_MODE", "orphan")
	reapOrphanDescendant(t)

	server := newWireHarness(t, cfgBase)
	serving := server.background(append([]string{"serve", file, "--"}, wireHelperCommand()...)...)
	waitForText(t, server.errs, "attached as handler")

	watcher := newWireHarness(t, cfgBase)
	watching := watcher.background("listen", file)
	waitForText(t, watcher.errs, "attached as observer")

	sender := newWireHarness(t, cfgBase)
	if code := sender.run("send", file, "--type", "wire/request", "--id", "req-orphan"); code != wireExitOK {
		t.Fatalf("send exited %d; stderr:\n%s", code, sender.errs.String())
	}

	// The handler exits 0 almost immediately; the grandchild holds the pipe. The
	// terminal frame must arrive on the drain window, not on the grandchild.
	waitForText(t, watcher.out, `"wire/done"`)

	server.cancel()
	watcher.cancel()
	waitForExit(t, serving)
	waitForExit(t, watching)
}

// The same wedge on the other pipe. stdout is an os.Pipe this process owns and
// can take away; stderr is an ordinary writer, so exec copies it on a goroutine
// that Wait waits for, and a descendant holding that write end is the one delay
// only WaitDelay can end. A handler that answered and exited 0 must still reach
// wire/done through it.
func TestWireServeSurvivesADescendantHoldingStderr(t *testing.T) {
	file, _, cfgBase := openTestSite(t)
	t.Setenv("HTMLCLAY_WIRE_HELPER", "1")
	t.Setenv("HTMLCLAY_WIRE_HELPER_MODE", "orphan-stderr")
	reapOrphanDescendant(t)

	server := newWireHarness(t, cfgBase)
	serving := server.background(append([]string{"serve", file, "--"}, wireHelperCommand()...)...)
	waitForText(t, server.errs, "attached as handler")

	watcher := newWireHarness(t, cfgBase)
	watching := watcher.background("listen", file)
	waitForText(t, watcher.errs, "attached as observer")

	sender := newWireHarness(t, cfgBase)
	if code := sender.run("send", file, "--type", "wire/request", "--id", "req-orphan-stderr"); code != wireExitOK {
		t.Fatalf("send exited %d; stderr:\n%s", code, sender.errs.String())
	}

	waitForText(t, watcher.out, `"wire/done"`)
	// The done had to come from the WaitDelay arm, not the ordinary one: an
	// assertion on `wire/done` alone would still pass if the descendant stopped
	// holding the pipe on its own and nothing was ever waited out.
	waitForText(t, server.errs, "done, after waiting out something the handler left holding a pipe")

	server.cancel()
	watcher.cancel()
	waitForExit(t, serving)
	waitForExit(t, watching)
}

func TestWireReadLineTruncatesInsteadOfStopping(t *testing.T) {
	long := strings.Repeat("a", 200<<10)
	r := bufio.NewReaderSize(strings.NewReader(long+"\nshort\n"), 4<<10)

	first, err := wireReadLine(r, 64)
	if err != nil {
		t.Fatalf("first line: %v", err)
	}
	if len(first) != 64 {
		t.Fatalf("first line is %d bytes, want it truncated to 64", len(first))
	}
	second, _ := wireReadLine(r, 64)
	if second != "short" {
		t.Fatalf("second line = %q, want the line after the long one", second)
	}
}

// The resume cursor may only advance past a frame that was delivered. The id
// line arrives before its data, so a stream cut between them must not move it.
func TestWireResumeIDWaitsForTheFrame(t *testing.T) {
	stream := "id: 7\ndata: {\"type\":\"wire/status\",\"id\":\"a\"}\n\nid: 8\n"
	var got int
	last := wireReadStream(strings.NewReader(stream), "", func([]byte, wireFrame) { got++ })
	if got != 1 {
		t.Fatalf("delivered %d frames, want 1", got)
	}
	if last != "7" {
		t.Fatalf("resume id = %q, want 7: frame 8 was never delivered", last)
	}
}

func TestWireOneLineKeepsStdoutParsable(t *testing.T) {
	raw := []byte("{\n  \"type\": \"wire/done\",\n  \"id\": \"a\"\n}")
	got := wireOneLine(raw)
	if bytes.ContainsAny(got, "\n\r") {
		t.Fatalf("a frame reached stdout across lines: %q", got)
	}
	var f wireFrame
	if err := json.Unmarshal(got, &f); err != nil || f.Type != "wire/done" {
		t.Fatalf("compaction broke the frame: %q (%v)", got, err)
	}
}

// wireHelperCommand runs this test binary as the handler, so `wire serve` covers
// the real spawn path on every platform instead of shelling out to sh.
func wireHelperCommand() []string {
	return []string{os.Args[0], "-test.run=TestWireHelperProcess"}
}

// noteOrphanPID records a spawned descendant's pid where the test that caused it
// can find it. The descendant deliberately outlives this handler, so the test is
// the only thing left that can end it.
func noteOrphanPID(child *exec.Cmd) {
	pidFile := os.Getenv("HTMLCLAY_WIRE_HELPER_PIDFILE")
	if pidFile == "" || child.Process == nil {
		return
	}
	os.WriteFile(pidFile, []byte(strconv.Itoa(child.Process.Pid)), 0644)
}

// TestWireHelperProcess is the handler command, not a test. It exits before the
// testing framework can print anything, so its stdout is exactly what it prints.
func TestWireHelperProcess(t *testing.T) {
	if os.Getenv("HTMLCLAY_WIRE_HELPER") != "1" {
		return
	}
	out := os.Getenv("HTMLCLAY_WIRE_HELPER_OUT")
	code := 0
	switch os.Getenv("HTMLCLAY_WIRE_HELPER_MODE") {
	case "echo":
		if out != "" {
			data, _ := io.ReadAll(os.Stdin)
			os.WriteFile(out, data, 0644)
		}
		fmt.Println("working")
	case "fail":
		fmt.Fprintln(os.Stderr, "handler blew up")
		code = 3
	case "long":
		// One line far past any reader's buffer, then a normal one. A handler
		// printing a whole JSON result on one line is ordinary.
		fmt.Println(strings.Repeat("x", 300<<10))
		fmt.Println("working")
	case "orphan":
		// A descendant that inherits stdout and outlives its parent. Nothing
		// closes the write end when this process exits, so the reader on the
		// other side only ever sees EOF if the CLI takes the pipe away.
		child := exec.Command(os.Args[0], "-test.run=TestWireHelperProcess")
		child.Env = append(os.Environ(), "HTMLCLAY_WIRE_HELPER_MODE=sleep",
			"HTMLCLAY_WIRE_HELPER_OUT=", "HTMLCLAY_WIRE_HELPER_PIDFILE=")
		child.Stdout = os.Stdout
		child.Start()
		noteOrphanPID(child)
		fmt.Println("working")
	case "orphan-stderr":
		// A descendant that inherits STDERR and outlives its parent. Nothing
		// closes that write end when this process exits, so exec's own copy
		// goroutine never sees EOF and only WaitDelay can end the wait.
		child := exec.Command(os.Args[0], "-test.run=TestWireHelperProcess")
		child.Env = append(os.Environ(), "HTMLCLAY_WIRE_HELPER_MODE=sleep",
			"HTMLCLAY_WIRE_HELPER_OUT=", "HTMLCLAY_WIRE_HELPER_PIDFILE=")
		child.Stderr = os.Stderr
		child.Start()
		noteOrphanPID(child)
		fmt.Println("answered")
	case "sleep":
		// SIGTERM has to be observable, or a test cannot tell "the signal
		// arrived" from "the process was still running when we gave up".
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, wireStopSignals...)
		if out != "" {
			os.WriteFile(out, []byte("started"), 0644)
		}
		// The orphan fixture is deliberately unreachable, so nothing signals it
		// and it always rides out this timer. Keep it well past any drain or
		// cancel window. It no longer has to be short enough to expire before the
		// suite ends: reapOrphanDescendant kills it in cleanup, which is what
		// stops it holding this binary open on Windows.
		select {
		case <-stop:
			if out != "" {
				os.WriteFile(out+".stopped", []byte("stopped"), 0644)
			}
		case <-time.After(20 * time.Second):
		}
	}
	os.Exit(code)
}
