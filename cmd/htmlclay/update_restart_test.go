package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/panphora/htmlclay/internal/platform"
	"github.com/panphora/htmlclay/internal/update"
)

// writeServer serves countWrites around handler on a fresh loopback port and
// returns the base URL.
func writeServer(t *testing.T, a *app, handler http.HandlerFunc) string {
	t.Helper()
	b, err := bindLoopback(0)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: a.countWrites(handler)}
	go srv.Serve(b.v4)
	t.Cleanup(func() { srv.Close(); b.close() })
	return "http://" + b.v4.Addr().String()
}

func postStatus(t *testing.T, url string) (int, http.Header, string) {
	t.Helper()
	resp, err := http.Post(url, "text/plain", strings.NewReader("scratch"))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(body)
}

// A write that arrives after the drain has closed admission is refused with a
// retriable 503 and never reaches a handler, so nothing is left in flight for
// the shutdown to cut off while the handoff is written and the update commits.
func TestUpdateRestartRefusesWritesAfterTheDrain(t *testing.T) {
	a, _ := updateTestApp(t)
	entered := make(chan struct{}, 1)
	base := writeServer(t, a, func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		w.WriteHeader(http.StatusOK)
	})

	var code int
	var hdr http.Header
	var body string
	var inFlight int64
	err := a.restartForUpdate(version, func(string) error {
		code, hdr, body = postStatus(t, base+"/_/save/tok")
		inFlight = a.rt.writesInFlight.Load()
		return nil
	})
	if err != nil {
		t.Fatalf("restartForUpdate = %v", err)
	}
	if code != http.StatusServiceUnavailable || hdr.Get("Retry-After") == "" || !strings.Contains(body, `"restarting"`) {
		t.Fatalf("a write after the drain got %d (Retry-After %q): %s", code, hdr.Get("Retry-After"), body)
	}
	select {
	case <-entered:
		t.Fatal("a write after the drain reached the handler")
	default:
	}
	if inFlight != 0 {
		t.Fatalf("writesInFlight = %d while the update committed", inFlight)
	}
	if code, _, _ := postStatus(t, base+"/_/save/tok"); code != http.StatusServiceUnavailable {
		t.Fatalf("writes reopened after a committed restart: %d", code)
	}
}

// A failed restart reopens writes, so the version that keeps running saves as
// it did before.
func TestUpdateRestartFailureReopensWrites(t *testing.T) {
	a, _ := updateTestApp(t)
	base := writeServer(t, a, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	err := a.restartForUpdate(version, func(string) error { return errors.New("install failed") })
	if err == nil {
		t.Fatal("restartForUpdate swallowed the install failure")
	}
	if code, _, body := postStatus(t, base+"/_/save/tok"); code != http.StatusOK {
		t.Fatalf("a save after a failed restart got %d: %s", code, body)
	}
}

// drainWrites waits for a write it already admitted, refusing new ones while
// it does; when the admitted write outlasts the limit it gives up and reopens.
func TestDrainWritesWaitsForAdmittedWritesAndReopensOnTimeout(t *testing.T) {
	a, _ := updateTestApp(t)
	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	base := writeServer(t, a, func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		if r.URL.Path == "/slow" {
			<-release
		}
		w.WriteHeader(http.StatusOK)
	})

	slow := make(chan int, 1)
	go func() {
		resp, err := http.Post(base+"/slow", "text/plain", strings.NewReader("x"))
		if err != nil {
			slow <- 0
			return
		}
		resp.Body.Close()
		slow <- resp.StatusCode
	}()
	<-entered

	if a.drainWrites(100 * time.Millisecond) {
		t.Fatal("drainWrites reported drained with a write still running")
	}
	if code, _, _ := postStatus(t, base+"/fast"); code != http.StatusOK {
		t.Fatalf("writes stayed closed after the drain gave up: %d", code)
	}
	<-entered

	drained := make(chan bool, 1)
	go func() { drained <- a.drainWrites(5 * time.Second) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, _, _ := postStatus(t, base+"/fast")
		if code == http.StatusServiceUnavailable {
			break
		}
		<-entered
		if time.Now().After(deadline) {
			t.Fatal("drainWrites never closed admission")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-drained:
		t.Fatal("drainWrites returned before the admitted write finished")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if !<-drained {
		t.Fatal("drainWrites gave up although the admitted write finished in time")
	}
	if code := <-slow; code != http.StatusOK {
		t.Fatalf("the admitted write was not allowed to finish: %d", code)
	}
}

// A request that reaches a restarted app before the handoff is restored waits
// until it is, so it can neither register the file under a fresh token nor
// make the restore refuse the old one.
func TestRequestsWaitForTheHandoffRestore(t *testing.T) {
	fx := newHandoffFixture(t, version)
	second := buildTestApp(t, fx.home, fx.cfgBase)
	second.holdRequests()
	if !second.startListener() {
		t.Fatal("startListener failed")
	}
	second.startSites()
	rel, err := filepath.Rel(fx.home, fx.page)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan int, 1)
	go func() {
		code, _ := fetchNav(t, "http://"+fx.trustedHost+"/"+rel)
		got <- code
	}()
	select {
	case code := <-got:
		t.Fatalf("a navigation was served (%d) before the handoff was restored", code)
	case <-time.After(300 * time.Millisecond):
	}
	restored, err := second.restoreUpdateHandoff(fx.handoff)
	if err != nil {
		t.Fatal(err)
	}
	second.releaseRequests()
	if code := <-got; code != 200 {
		t.Fatalf("the held navigation got %d after the restore", code)
	}
	if restored != 3 {
		t.Fatalf("restored %d registrations, want 3", restored)
	}
	if code, body := saveWith(t, fx.trustedHost, fx.pageTok, "<html><body>old tab save</body></html>"); code != 200 {
		t.Fatalf("the old tab's token stopped working: %d %s", code, body)
	}
	second.shutdown()
}

// markApplying records an update to this version as applying, the state the
// new version starts under.
func markApplying(t *testing.T, configDir string) {
	t.Helper()
	dir := update.UpdatesDir(configDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	st := update.State{Schema: update.StateSchema, Phase: update.PhaseApplying, From: "0.0.1", To: version, OldPID: 1, Txid: "t", Time: time.Now().UTC()}
	data, _ := json.Marshal(st)
	if err := os.WriteFile(filepath.Join(dir, "state.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The new version of an update never asks about a taken port and never moves:
// it retries the handoff's port quietly, then gives up without serving, so the
// supervisor rolls back to the version whose tabs are on that port.
func TestUpdateRestartNeverMovesThePort(t *testing.T) {
	defer func(d time.Duration) { updatePortWait = d }(updatePortWait)
	updatePortWait = 300 * time.Millisecond
	fx := newHandoffFixture(t, version)
	holder, err := bindLoopback(fx.port)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.close()
	second := buildTestApp(t, fx.home, fx.cfgBase)
	markApplying(t, second.rt.configDir)
	if err := os.Rename(fx.handoff, filepath.Join(second.rt.configDir, updateHandoffFile)); err != nil {
		t.Fatal(err)
	}
	prompted := 0
	second.rt.confirmPort = func(string, string, platform.ConfirmLabels) (platform.ConfirmChoice, error) {
		prompted++
		return platform.ConfirmAllowOnce, nil
	}
	start := time.Now()
	if second.startListener() {
		t.Fatalf("startListener bound port %d while %d was taken", second.rt.port, fx.port)
	}
	if prompted != 0 {
		t.Fatalf("the port dialog was shown %d time(s) during an update restart", prompted)
	}
	if !second.restartingForUpdate {
		t.Fatal("the launch was not recognised as an update restart")
	}
	if took := time.Since(start); took < updatePortWait {
		t.Fatalf("gave up after %s, before the %s retry window", took, updatePortWait)
	}
	if second.rt.cfg.Port() != fx.port {
		t.Fatalf("the remembered port changed to %d", second.rt.cfg.Port())
	}
	if _, err := os.Stat(filepath.Join(second.rt.configDir, updateHandoffFile)); err != nil {
		t.Fatalf("the handoff was consumed by a launch that never served: %v", err)
	}
}

// The port coming free inside the retry window is the ordinary case: the old
// process has just exited. The new version takes the same port and every
// session comes back.
func TestUpdateRestartWaitsForThePortToComeFree(t *testing.T) {
	fx := newHandoffFixture(t, version)
	holder, err := bindLoopback(fx.port)
	if err != nil {
		t.Fatal(err)
	}
	second := buildTestApp(t, fx.home, fx.cfgBase)
	markApplying(t, second.rt.configDir)
	handoff := filepath.Join(second.rt.configDir, updateHandoffFile)
	if err := os.Rename(fx.handoff, handoff); err != nil {
		t.Fatal(err)
	}
	second.rt.confirmPort = func(string, string, platform.ConfirmLabels) (platform.ConfirmChoice, error) {
		t.Error("the port dialog was shown during an update restart")
		return platform.ConfirmDeny, nil
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		holder.close()
	}()
	if !second.startListener() {
		t.Fatal("startListener gave up although the port came free")
	}
	defer second.shutdown()
	if second.rt.port != fx.port {
		t.Fatalf("bound %d, want the handoff's port %d", second.rt.port, fx.port)
	}
	second.startSites()
	if n, err := second.restoreUpdateHandoff(handoff); err != nil || n != 3 {
		t.Fatalf("restore = (%d, %v), want 3", n, err)
	}
}

// A file replaced at the same path after the handoff was written is not the
// file the tab opened, so its old token must not come back.
func TestHandoffRefusesAReplacedFile(t *testing.T) {
	fx := newHandoffFixture(t, version)
	replacement := fx.loose + ".replacement"
	writeTestFile(t, replacement, "<html><body>different document</body></html>")
	if err := os.Rename(replacement, fx.loose); err != nil {
		t.Fatal(err)
	}
	_, restored, err := fx.restart(t)
	if err != nil {
		t.Fatal(err)
	}
	if restored != 2 {
		t.Fatalf("restored %d registrations, want the two unchanged trusted files", restored)
	}
	if code, _ := saveWith(t, fx.looseHost, fx.looseTok, "<html><body>old tab overwrites replacement</body></html>"); code == 200 {
		t.Fatal("the old token saved over the replacement file")
	}
	data, _ := os.ReadFile(fx.loose)
	if !strings.Contains(string(data), "different document") {
		t.Fatal("the replacement file was overwritten")
	}
}

// A granted folder replaced by another at the same path loses the grant, and
// the file beside it keeps its token.
func TestHandoffRefusesAReplacedGrantFolder(t *testing.T) {
	fx := newHandoffFixture(t, version)
	if err := os.Rename(fx.assets, fx.assets+".old"); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(fx.assets, "logo.txt"), "other logo")
	second, restored, err := fx.restart(t)
	if err != nil || restored != 3 {
		t.Fatalf("restore = (%d, %v), want 3", restored, err)
	}
	s := liveSiteWithHost(t, second, fx.looseHost)
	if _, _, ok := s.sessions.AssetRoot(filepath.Join(fx.assets, "logo.txt")); ok {
		t.Fatal("the grant came back for a folder that replaced the approved one")
	}
}

// An entry written without identities, as by a build that did not record
// them, is refused rather than trusted on its path alone.
func TestHandoffRefusesEntriesWithoutIdentity(t *testing.T) {
	fx := newHandoffFixture(t, version)
	data, err := os.ReadFile(fx.handoff)
	if err != nil {
		t.Fatal(err)
	}
	var h updateHandoff
	if err := json.Unmarshal(data, &h); err != nil {
		t.Fatal(err)
	}
	for i := range h.Sites {
		h.Sites[i].GrantIdentities = nil
		for j := range h.Sites[i].Files {
			h.Sites[i].Files[j].Identity = ""
		}
	}
	out, _ := json.Marshal(h)
	if err := os.WriteFile(fx.handoff, out, 0o600); err != nil {
		t.Fatal(err)
	}
	_, restored, err := fx.restart(t)
	if err != nil || restored != 0 {
		t.Fatalf("restore = (%d, %v), want every entry refused", restored, err)
	}
}
