package server

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/panphora/htmlclay/htmlutil"
	"github.com/panphora/htmlclay/logging"
	"github.com/panphora/htmlclay/platform"
	"github.com/panphora/htmlclay/session"
	"github.com/panphora/htmlclay/versions"
)

// A banner that reached a token-holding tab must never be saved to disk,
// whatever path it took to get there.
func TestSaveStripsBanner(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)

	banner := string(htmlutil.WrapBanner([]byte(`<div>open?</div>`)))
	body := `<!DOCTYPE html><html><body>edited` + banner + `</body></html>`
	req := httptest.NewRequest("POST", "/_/save/"+f.Token, strings.NewReader(body))
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.SetPathValue("token", f.Token)
	w := httptest.NewRecorder()
	srv.handleSave(w, req)

	if w.Code != 200 {
		t.Fatalf("save returned %d: %s", w.Code, w.Body.String())
	}
	saved, _ := os.ReadFile(f.AbsPath)
	if strings.Contains(string(saved), "htmlclay-banner") || strings.Contains(string(saved), "open?") {
		t.Fatalf("banner reached disk: %q", saved)
	}
	if !strings.Contains(string(saved), "edited") {
		t.Fatalf("save lost its real content: %q", saved)
	}
}

// A trusted-folder registration's write capability dies with its trusted root,
// even for a token a page is still holding: the save-time recheck refuses the
// write after the root is revoked. A file the user OS-opened is untouched.
func TestWorkspaceSaveRecheckAfterRootRevoked(t *testing.T) {
	homeDir, _ := filepath.EvalSymlinks(t.TempDir())
	ws := filepath.Join(homeDir, "ws")
	wsFile := filepath.Join(ws, "auto.htmlclay")
	openedFile := filepath.Join(ws, "opened.htmlclay")
	for _, p := range []string{wsFile, openedFile} {
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("<html><body>v1</body></html>"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	mgr := newTestManager(t, homeDir)
	if err := mgr.InstallTrustedRoot(ws); err != nil {
		t.Fatalf("install trusted root: %v", err)
	}
	wsReg, err := mgr.Register(wsFile, session.ViaTrusted)
	if err != nil {
		t.Fatalf("register trusted-folder file: %v", err)
	}
	openedReg, err := mgr.Register(openedFile, session.ViaOsOpen)
	if err != nil {
		t.Fatalf("register opened file: %v", err)
	}

	srv := New(listen(t), mgr, logging.NewStdout(), versions.New(t.TempDir()))

	save := func(f *session.File) int {
		req := httptest.NewRequest("POST", "/_/save/"+f.Token, strings.NewReader("<html><body>v2</body></html>"))
		req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
		req.SetPathValue("token", f.Token)
		w := httptest.NewRecorder()
		srv.handleSave(w, req)
		return w.Code
	}

	if code := save(wsReg); code != 200 {
		t.Fatalf("trusted-folder save before revoke = %d", code)
	}

	mgr.RevokeTrustedRoot(ws)

	if code := save(wsReg); code != 401 {
		t.Fatalf("trusted-folder save after revoke = %d, want 401", code)
	}
	if code := save(openedReg); code != 200 {
		t.Fatalf("OS-opened save after trusted revoke = %d, want 200", code)
	}
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

// One native dialog at a time: a runPrompt dialog must wait for a grant prompt
// in flight, and vice versa.
func TestRunPromptSerializesWithGrantPrompt(t *testing.T) {
	homeDir, _ := filepath.EvalSymlinks(t.TempDir())
	page := filepath.Join(homeDir, "site", "page.htmlclay")
	asset := filepath.Join(homeDir, "elsewhere", "a.js")
	mustWrite(t, page)
	mustWrite(t, asset)

	mgr := newTestManager(t, homeDir)
	if _, err := mgr.Register(page, session.ViaOsOpen); err != nil {
		t.Fatal(err)
	}

	var inPrompt atomic.Int32
	var overlapped atomic.Bool
	enter := func() {
		if inPrompt.Add(1) > 1 {
			overlapped.Store(true)
		}
		time.Sleep(50 * time.Millisecond)
		inPrompt.Add(-1)
	}

	b := newBroker(mgr, logging.NewStdout(), func(string, string, bool) (platform.ConfirmChoice, error) {
		enter()
		return platform.ConfirmDeny, nil
	})

	grantDone := make(chan struct{})
	go func() {
		defer close(grantDone)
		b.await(t.Context(), asset)
	}()

	// Give the grant prompt time to arm and start.
	time.Sleep(300 * time.Millisecond)

	promptDone := make(chan struct{})
	go func() {
		defer close(promptDone)
		b.runPrompt(enter)
	}()

	<-grantDone
	<-promptDone
	if overlapped.Load() {
		t.Fatal("two native prompts were on screen at once")
	}
}

// The endpoint-level Feature A flow: a navigation serve carries a nonce-bearing
// banner and no token; the nonce redeems exactly once against the served file;
// a deny suppresses the whole directory for the session.
func TestOpenRequestEndpointFlow(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	dir := registerSubdirPage(t, srv, "site")
	sibling := filepath.Join(dir, "week.htmlclay")
	if err := os.WriteFile(sibling, []byte("<html><body>week</body></html>"), 0644); err != nil {
		t.Fatal(err)
	}

	var opened []string
	allow := true
	srv.SetHooks(Hooks{
		TrustRequest: func(absPath string, _ bool) (string, bool) {
			opened = append(opened, absPath)
			return "http://127.0.0.1:1/opened", allow
		},
	})

	host := fmt.Sprintf("127.0.0.1:%d", srv.port)
	serveNav := func(rel string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/"+rel, nil)
		req.Host = host
		req.Header.Set("Sec-Fetch-Dest", "document")
		req.Header.Set("Sec-Fetch-User", "?1")
		w := httptest.NewRecorder()
		srv.httpServer.Handler.ServeHTTP(w, req)
		return w
	}
	postNonce := func(nonce string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/_/open-request", strings.NewReader(`{"nonce":"`+nonce+`"}`))
		req.Host = host
		req.Header.Set("Content-Type", "application/json")
		sameOriginHeaders(req)
		w := httptest.NewRecorder()
		srv.httpServer.Handler.ServeHTTP(w, req)
		return w
	}
	relSibling := "site/week.htmlclay"

	// A plain GET (no Sec-Fetch headers, e.g. a fetch() or an old client) gets
	// no banner and no nonce; a fetch()-shaped one (dest empty) too.
	if w := serveNav(relSibling); w.Code != 200 || !strings.Contains(w.Body.String(), "htmlclay-banner") {
		t.Fatalf("navigation should carry the banner: %d %q", w.Code, w.Body.String())
	}
	plainReq := httptest.NewRequest("GET", "/"+relSibling, nil)
	plainReq.Host = host
	plainW := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(plainW, plainReq)
	if strings.Contains(plainW.Body.String(), "htmlclay-banner") {
		t.Fatal("a headerless GET must not carry the banner")
	}

	nav := serveNav(relSibling)
	if cc := nav.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("bannered serve must be no-store, got %q", cc)
	}
	if strings.Contains(nav.Body.String(), "htmlclaytoken") {
		t.Fatal("a read-only bannered serve must not carry a save token")
	}
	nonceRe := regexp.MustCompile(`\{nonce:'([A-Za-z0-9_-]+)'\}`)
	m := nonceRe.FindStringSubmatch(nav.Body.String())
	if m == nil {
		t.Fatalf("no nonce in banner: %q", nav.Body.String())
	}
	nonce := m[1]

	// A foreign nonce never reaches the dialog.
	if w := postNonce("A_completely_invented_nonce_value_123456789"); w.Code != 403 {
		t.Fatalf("foreign nonce = %d, want 403", w.Code)
	}
	if len(opened) != 0 {
		t.Fatal("foreign nonce raised a dialog")
	}

	// The real nonce resolves to the served file and redeems exactly once.
	w := postNonce(nonce)
	if w.Code != 200 {
		t.Fatalf("open-request = %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		OK  bool   `json:"ok"`
		URL string `json:"url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || !resp.OK || resp.URL == "" {
		t.Fatalf("bad open-request response: %s", w.Body.String())
	}
	if len(opened) != 1 || opened[0] != sibling {
		t.Fatalf("dialog saw %v, want exactly [%s]", opened, sibling)
	}
	if w := postNonce(nonce); w.Code != 403 {
		t.Fatalf("nonce redeemed twice: %d", w.Code)
	}

	// Deny burns the nonce and suppresses the directory: no more banners, and
	// no more dialogs, for any file in it this session.
	allow = false
	m = nonceRe.FindStringSubmatch(serveNav(relSibling).Body.String())
	if m == nil {
		t.Fatal("expected a fresh banner before the deny")
	}
	if w := postNonce(m[1]); w.Code != 403 {
		t.Fatalf("denied open-request = %d, want 403", w.Code)
	}
	if strings.Contains(serveNav(relSibling).Body.String(), "htmlclay-banner") {
		t.Fatal("banner still offered after a deny in this directory")
	}
	if len(opened) != 2 {
		t.Fatalf("dialog count = %d, want 2", len(opened))
	}
}

// The endpoint-level Feature B gate: only a file the user opened themselves may
// ask; the folder is derived server-side; the dialog is told which way the file
// was reached; and a deny suppresses the folder.
func TestWorkspaceRequestEndpointGates(t *testing.T) {
	srv, osOpened, _ := setupHandlerTest(t)
	dir := registerSubdirPage(t, srv, "proj")
	pageOpened := filepath.Join(dir, "linked.htmlclay")
	if err := os.WriteFile(pageOpened, []byte("<html></html>"), 0644); err != nil {
		t.Fatal(err)
	}
	linkedReg, err := srv.sessions.Register(pageOpened, session.ViaTrusted)
	if err != nil {
		t.Fatal(err)
	}

	var asked []string
	var openedFlags []bool
	allow := true
	srv.SetHooks(Hooks{
		TrustRequest: func(requestingFile string, openedByUser bool) (string, bool) {
			asked = append(asked, requestingFile)
			openedFlags = append(openedFlags, openedByUser)
			return "", allow
		},
	})

	host := fmt.Sprintf("127.0.0.1:%d", srv.port)
	post := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/_/workspace-request/"+token, nil)
		req.Host = host
		sameOriginHeaders(req)
		w := httptest.NewRecorder()
		srv.httpServer.Handler.ServeHTTP(w, req)
		return w
	}

	if w := post("not-a-token"); w.Code != 401 {
		t.Fatalf("bad token = %d, want 401", w.Code)
	}
	// A file the user did NOT open themselves cannot declare trust. A token that
	// was minted for any other reason must never reach the dialog, or a page
	// could bootstrap a durable write grant out of a registration it did not ask
	// a human for.
	if w := post(linkedReg.Token); w.Code != 403 {
		t.Fatalf("token for a file the user never opened = %d, want 403", w.Code)
	}
	if len(asked) != 0 {
		t.Fatalf("a file the user never opened reached the dialog: %v", asked)
	}
	asked = nil
	openedFlags = nil

	if w := post(osOpened.Token); w.Code != 200 {
		t.Fatalf("OS-opened token = %d", w.Code)
	}
	if len(asked) != 1 || asked[0] != osOpened.AbsPath {
		t.Fatalf("dialog saw %v, want [%s]", asked, osOpened.AbsPath)
	}
	if len(openedFlags) != 1 || !openedFlags[0] {
		t.Fatalf("OS-opened ask reported openedByUser = %v, want [true]", openedFlags)
	}

	// After a deny, the folder stops asking for the session.
	allow = false
	if w := post(osOpened.Token); w.Code != 403 {
		t.Fatalf("denied request = %d, want 403", w.Code)
	}
	if w := post(osOpened.Token); w.Code != 403 {
		t.Fatalf("suppressed request = %d, want 403", w.Code)
	}
	if len(asked) != 2 {
		t.Fatalf("dialog count = %d, want 2 (suppression must skip the third)", len(asked))
	}
}

// A deny must answer the asks already queued behind the dialog. Selecting a
// folder of files and opening them at once queues one workspace ask per file;
// without the second refusal check inside the prompt lock, every ask that
// cleared the outer check before the user said No goes on to raise its own
// dialog.
func TestWorkspaceRequestDenySuppressesQueuedAsks(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)
	dir := registerSubdirPage(t, srv, "proj")

	var tokens []string
	for _, name := range []string{"a.htmlclay", "b.htmlclay", "c.htmlclay"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("<html><body>doc</body></html>"), 0644); err != nil {
			t.Fatal(err)
		}
		f, err := srv.sessions.Register(p, session.ViaOsOpen)
		if err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, f.Token)
	}

	var asks atomic.Int32
	release := make(chan struct{})
	srv.SetHooks(Hooks{
		TrustRequest: func(requestingFile string, openedByUser bool) (string, bool) {
			if asks.Add(1) == 1 {
				<-release
				// The refusal the handler records a few instructions after this
				// returns, recorded here instead so the interleaving under test is
				// pinned rather than raced: the other asks are parked inside
				// runPrompt right now, and what they do when they wake is the whole
				// question.
				srv.suppressTrustDenied(filepath.Dir(requestingFile))
			}
			return "", false
		},
	})

	host := fmt.Sprintf("127.0.0.1:%d", srv.port)
	codes := make(chan int, len(tokens))
	post := func(token string) {
		req := httptest.NewRequest("POST", "/_/workspace-request/"+token, nil)
		req.Host = host
		sameOriginHeaders(req)
		w := httptest.NewRecorder()
		srv.httpServer.Handler.ServeHTTP(w, req)
		codes <- w.Code
	}

	go post(tokens[0])
	deadline := time.Now().Add(2 * time.Second)
	for asks.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if asks.Load() != 1 {
		t.Fatal("the first ask never reached the dialog")
	}
	for _, tok := range tokens[1:] {
		go post(tok)
	}
	// Generous margin for the queued asks to clear the outer refusal check and
	// park inside runPrompt before the first one is answered.
	time.Sleep(300 * time.Millisecond)
	close(release)

	for range tokens {
		if code := <-codes; code != 403 {
			t.Fatalf("queued ask = %d, want 403", code)
		}
	}
	if n := asks.Load(); n != 1 {
		t.Fatalf("dialog fired %d times; a deny must answer the asks queued behind it", n)
	}
}

// A file already inside a LIVE trusted folder is answered from the app: no
// dialog, and no refusal either. A page that asks on every load would otherwise
// log a refusal for every sibling on every view — note the registration here is
// ViaTrusted, so this silent answer has to come out ahead of the provenance
// check that would refuse it.
func TestWorkspaceRequestCoveredFileIsSilent(t *testing.T) {
	homeDir, _ := filepath.EvalSymlinks(t.TempDir())
	ws := filepath.Join(homeDir, "ws")
	auto := filepath.Join(ws, "auto.htmlclay")
	if err := os.MkdirAll(ws, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(auto, []byte("<html><body>doc</body></html>"), 0644); err != nil {
		t.Fatal(err)
	}

	mgr := newTestManager(t, homeDir)
	if err := mgr.InstallTrustedRoot(ws); err != nil {
		t.Fatal(err)
	}
	reg, err := mgr.Register(auto, session.ViaTrusted)
	if err != nil {
		t.Fatal(err)
	}

	srv := New(listen(t), mgr, logging.NewStdout(), versions.New(t.TempDir()))
	asked := 0
	srv.SetHooks(Hooks{
		TrustedCovers: func(absPath string) bool { return session.EqualOrUnder(absPath, ws) },
		TrustedLive:   func(absPath string) bool { return session.EqualOrUnder(absPath, ws) },
		TrustRequest: func(string, bool) (string, bool) {
			asked++
			return "", false
		},
	})

	req := httptest.NewRequest("POST", "/_/workspace-request/"+reg.Token, nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	sameOriginHeaders(req)
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("covered file ask = %d: %s", w.Code, w.Body.String())
	}
	if asked != 0 {
		t.Fatalf("a covered file raised %d dialog(s)", asked)
	}
}

// The silent answer above must come from the LIVE question, not the lexical one.
// A trusted folder deleted and recreated still covers its files by path while
// granting them nothing: answering yes from that would tell the page it is
// already trusted and it would never reach the dialog that re-pins the folder now
// on disk, leaving the one page route into trust permanently dead for that
// folder. Only TrustedLive differs from the test above.
func TestWorkspaceRequestBrokenPinStillAsks(t *testing.T) {
	homeDir, _ := filepath.EvalSymlinks(t.TempDir())
	ws := filepath.Join(homeDir, "ws")
	opened := filepath.Join(ws, "opened.htmlclay")
	if err := os.MkdirAll(ws, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(opened, []byte("<html><body>doc</body></html>"), 0644); err != nil {
		t.Fatal(err)
	}

	mgr := newTestManager(t, homeDir)
	reg, err := mgr.Register(opened, session.ViaOsOpen)
	if err != nil {
		t.Fatal(err)
	}

	srv := New(listen(t), mgr, logging.NewStdout(), versions.New(t.TempDir()))
	asked := 0
	srv.SetHooks(Hooks{
		TrustedCovers: func(absPath string) bool { return session.EqualOrUnder(absPath, ws) },
		TrustedLive:   func(string) bool { return false },
		TrustRequest: func(string, bool) (string, bool) {
			asked++
			return "", true
		},
	})

	req := httptest.NewRequest("POST", "/_/workspace-request/"+reg.Token, nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	sameOriginHeaders(req)
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	if asked != 1 {
		t.Fatalf("a folder whose pin no longer matches must still be able to ask: dialogs raised = %d", asked)
	}
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("re-approval = %d: %s", w.Code, w.Body.String())
	}
}

// The auto-register branch: workspace scope decides with no filesystem access,
// only real document loads register, hidden paths are refused at the branch,
// and the first-serve snapshot is deferred until the first real save.
func TestWorkspaceAutoRegisterBranch(t *testing.T) {
	homeDir, _ := filepath.EvalSymlinks(t.TempDir())
	ws := filepath.Join(homeDir, "ws")
	anchor := filepath.Join(ws, "index.htmlclay")
	linked := filepath.Join(ws, "week.htmlclay")
	plainHTML := filepath.Join(ws, "plain.html")
	hidden := filepath.Join(ws, ".secret", "x.htmlclay")
	outside := filepath.Join(homeDir, "elsewhere", "out.htmlclay")
	for _, p := range []string{anchor, linked, plainHTML, hidden, outside} {
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("<html><body>doc</body></html>"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	mgr := newTestManager(t, homeDir)
	if err := mgr.InstallTrustedRoot(ws); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Register(anchor, session.ViaOsOpen); err != nil {
		t.Fatal(err)
	}

	versionsDir := t.TempDir()
	srv := New(listen(t), mgr, logging.NewStdout(), versions.New(versionsDir))
	// The seam registers into this same manager, standing in for route()
	// landing the registration in this site.
	srv.SetHooks(Hooks{
		TrustedCovers: func(absPath string) bool { return session.EqualOrUnder(absPath, ws) },
		Route: func(absPath string) (string, bool) {
			if _, err := mgr.Register(absPath, session.ViaTrusted); err != nil {
				return "", false
			}
			return "http://" + fmt.Sprintf("127.0.0.1:%d", srv.port) + "/", true
		},
	})

	host := fmt.Sprintf("127.0.0.1:%d", srv.port)
	get := func(rel, dest string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/"+rel, nil)
		req.Host = host
		if dest != "" {
			req.Header.Set("Sec-Fetch-Dest", dest)
		}
		w := httptest.NewRecorder()
		srv.httpServer.Handler.ServeHTTP(w, req)
		return w
	}

	// A document load of a trusted-folder sibling auto-registers and serves a token.
	w := get("ws/week.htmlclay", "document")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "htmlclaytoken") {
		t.Fatalf("trusted-folder document load: %d, token=%v", w.Code, strings.Contains(w.Body.String(), "htmlclaytoken"))
	}
	if mgr.Via(linked) != session.ViaTrusted {
		t.Fatalf("linked file provenance = %v", mgr.Via(linked))
	}

	// No first-serve snapshot for an auto-registered file: the versions store
	// stays empty until a real save.
	entries := 0
	filepath.WalkDir(versionsDir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && !strings.HasPrefix(filepath.Base(path), ".") {
			entries++
		}
		return nil
	})
	if entries != 0 {
		t.Fatalf("auto-registration snapshotted %d file(s) on serve", entries)
	}

	// A fetch()-shaped request must not register.
	if w := get("ws/plain2.htmlclay", "empty"); w.Code == 200 && strings.Contains(w.Body.String(), "htmlclaytoken") {
		t.Fatal("a fetch() received a token")
	}
	if mgr.Via(filepath.Join(ws, "plain2.htmlclay")) != 0 {
		t.Fatal("a fetch() auto-registered a file")
	}

	// .html is never auto-registered; it stays a plain asset.
	if w := get("ws/plain.html", "document"); w.Code != 200 || strings.Contains(w.Body.String(), "htmlclaytoken") {
		t.Fatalf("plain html: %d tokened=%v", w.Code, strings.Contains(w.Body.String(), "htmlclaytoken"))
	}
	if mgr.Via(plainHTML) != 0 {
		t.Fatal(".html auto-registered")
	}

	// Hidden under the workspace: refused at the branch (and by the asset path).
	if w := get("ws/.secret/x.htmlclay", "document"); w.Code == 200 {
		t.Fatal("hidden workspace file served")
	}
	if mgr.Via(hidden) != 0 {
		t.Fatal("hidden workspace file registered")
	}

	// Outside the workspace: not auto-registered (parks/denies as before).
	if mgr.Via(outside) != 0 {
		t.Fatal("out-of-workspace file registered")
	}
}

// DropSubscribers ends every live stream for a path — the teardown Unregister
// relies on.
func TestDropSubscribersEndsStreams(t *testing.T) {
	srv, f, _ := setupHandlerTest(t)
	go srv.Start()
	t.Cleanup(func() { srv.Close() })

	base := fmt.Sprintf("http://127.0.0.1:%d", srv.port)
	req, _ := http.NewRequest("GET", base+"/_/live-sync/stream?page-url="+url.QueryEscape(base+"/"+f.RelPath), nil)
	resp, err := http.DefaultClient.Do(sameOriginHeaders(req))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("stream = %d", resp.StatusCode)
	}

	deadline := time.Now().Add(2 * time.Second)
	for srv.hub.subscriberCount(f.AbsPath) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if srv.hub.subscriberCount(f.AbsPath) == 0 {
		t.Fatal("stream never subscribed")
	}

	srv.ls.DropSubscribers(f.AbsPath)

	deadline = time.Now().Add(2 * time.Second)
	for srv.hub.subscriberCount(f.AbsPath) > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := srv.hub.subscriberCount(f.AbsPath); n != 0 {
		t.Fatalf("%d subscriber(s) survived DropSubscribers", n)
	}
}
