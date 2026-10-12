package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/panphora/htmlclay/internal/server"
)

// freeTestPort returns a port nothing is listening on, for the tests that need a
// legacy per-folder port or a candidate the app must skip.
func freeTestPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

// serveHostForTest drives the one handler directly, with no listener: the routing
// decisions are all in the handler, and the Host header is the whole of its input.
func serveHostForTest(t *testing.T, a *app, host string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "http://"+host+"/", nil)
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	a.serveHost(rec, req)
	return rec
}

func TestServeHostBareLoopbackIsTheIndexPage(t *testing.T) {
	a := buildTestApp(t, t.TempDir(), t.TempDir())
	a.rt.port = 24190
	for _, host := range []string{"localhost:24190", "127.0.0.1:24190", "[::1]:24190"} {
		rec := serveHostForTest(t, a, host, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", host, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "HTML Clay is running") {
			t.Errorf("%s: body = %q, want the index page", host, rec.Body.String())
		}
	}
}

func TestServeHostRefusesForeignAndWrongPort(t *testing.T) {
	a := buildTestApp(t, t.TempDir(), t.TempDir())
	a.rt.port = 24190
	for _, host := range []string{"evil.example:24190", "x-0000.localhost:24191", "Bad_Label.localhost:24190"} {
		if rec := serveHostForTest(t, a, host, nil); rec.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", host, rec.Code)
		}
	}
}

func TestServeHostUnknownSlugIsTheRecoveryPage(t *testing.T) {
	a := buildTestApp(t, t.TempDir(), t.TempDir())
	a.rt.port = 24190
	rec := serveHostForTest(t, a, "nobody-0000.localhost:24190", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if body := rec.Body.String(); body != string(recoveryPage) {
		t.Fatalf("body = %q, want the recovery page", body)
	}
	if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("Referrer-Policy = %q, want no-referrer", got)
	}
}

func TestServeHostCrossSiteFetchIsRefusedEvenForTheIndexPage(t *testing.T) {
	a := buildTestApp(t, t.TempDir(), t.TempDir())
	a.rt.port = 24190
	rec := serveHostForTest(t, a, "localhost:24190", func(r *http.Request) {
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		r.Header.Set("Sec-Fetch-Mode", "cors")
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

// Two folders are two origins on the one port: each hostname serves its own file
// editable and hands out no token for the other folder's file.
func TestTwoFoldersAreTwoHostnamesOnOnePort(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	one := filepath.Join(home, "one")
	two := filepath.Join(home, "two")
	pageOne := filepath.Join(one, "index.htmlclay")
	pageTwo := filepath.Join(two, "index.htmlclay")
	writeTestFile(t, pageOne, "<html><body>ONE</body></html>")
	writeTestFile(t, pageTwo, "<html><body>TWO</body></html>")

	a := newTestApp(t, home)
	siteOne, relOne := a.openForTest(t, pageOne)
	siteTwo, relTwo := a.openForTest(t, pageTwo)

	if siteOne.slug == siteTwo.slug {
		t.Fatalf("two folders must get two labels, both are %q", siteOne.slug)
	}
	for _, s := range []*site{siteOne, siteTwo} {
		if s.host != siteHost(s.slug, a.rt.port) {
			t.Fatalf("%s is not %s on the app's port %d", s.host, s.slug, a.rt.port)
		}
	}

	if code, body := fetch(t, siteOne.url(relOne)); code != http.StatusOK || !strings.Contains(body, "ONE") {
		t.Fatalf("the first folder's own file = %d %q", code, body)
	}
	if code, body := fetch(t, siteTwo.url(relTwo)); code != http.StatusOK || !strings.Contains(body, "TWO") {
		t.Fatalf("the second folder's own file = %d %q", code, body)
	}
	if code, body := fetch(t, siteOne.url(relTwo)); strings.Contains(body, "savetoken") {
		t.Fatalf("the first hostname handed out a token for the second folder's file: %d %q", code, body)
	}
}

// A file loose in home anchors at home, which every other such file shares, so it is
// remembered under the file itself. Two of them are two origins, and both addresses
// survive a restart on the same config.
func TestLooseHomeFilesGetTheirOwnStableHostnames(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	alpha := filepath.Join(home, "alpha.htmlclay")
	beta := filepath.Join(home, "beta.htmlclay")
	writeTestFile(t, alpha, "<html><body>ALPHA</body></html>")
	writeTestFile(t, beta, "<html><body>BETA</body></html>")

	cfgBase := t.TempDir()
	a := newTestAppWithConfigDir(t, home, cfgBase)
	siteAlpha, _ := a.openForTest(t, alpha)
	siteBeta, _ := a.openForTest(t, beta)
	if siteAlpha.host == siteBeta.host || siteAlpha.slug == siteBeta.slug {
		t.Fatalf("two loose files must not share an origin: %s and %s", siteAlpha.host, siteBeta.host)
	}
	alphaHost, betaHost := siteAlpha.host, siteBeta.host
	a.shutdown()

	restarted := newTestAppWithConfigDir(t, home, cfgBase)
	againAlpha, alphaRel := restarted.openForTest(t, alpha)
	againBeta, betaRel := restarted.openForTest(t, beta)
	if againAlpha.host != alphaHost || againBeta.host != betaHost {
		t.Fatalf("a loose file's address must survive a restart: got %s/%s, want %s/%s",
			againAlpha.host, againBeta.host, alphaHost, betaHost)
	}
	for _, tc := range []struct {
		s      *site
		rel    string
		canary string
	}{{againAlpha, alphaRel, "ALPHA"}, {againBeta, betaRel, "BETA"}} {
		code, body := fetch(t, tc.s.url(tc.rel))
		if code != http.StatusOK || !strings.Contains(body, tc.canary) {
			t.Fatalf("%s = %d %q", tc.s.host, code, body)
		}
	}
}

// A per-folder port remembered from before hostnames is bound at startup, so an old
// bookmark answers with a redirect to the folder's hostname instead of a refusal.
func TestLegacyPortRedirectsToTheHostname(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	proj := filepath.Join(home, "proj")
	page := filepath.Join(proj, "index.htmlclay")
	writeTestFile(t, page, "<html><body>index</body></html>")

	cfgBase := t.TempDir()
	first := newTestAppWithConfigDir(t, home, cfgBase)
	if err := first.trustFolder(proj); err != nil {
		t.Fatal(err)
	}
	first.openForTest(t, page)
	first.shutdown()

	second := newTestAppWithConfigDir(t, home, cfgBase)
	legacy := freeTestPort(t)
	second.rt.cfg.RememberSitePort(proj, legacy)
	second.startSites()
	t.Cleanup(second.shutdown)

	live := liveSite(t, second, proj)
	rel := "proj/index.htmlclay"
	bookmark := fileURL(server.LoopbackHosts(legacy)[0], rel)
	if got := recoveryRedirect(t, bookmark); got != live.url(rel) {
		t.Fatalf("the legacy port relocated to %q, want %q", got, live.url(rel))
	}
	if code, body := fetch(t, bookmark); code != http.StatusNotFound || strings.Contains(body, "savetoken") {
		t.Fatalf("a plain GET on the legacy port = %d %q", code, body)
	}
}

// A browser sends the hostname as typed; the router lowercases it before dispatch.
func TestRouterLowercasesTheHostname(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	proj := filepath.Join(home, "proj")
	page := filepath.Join(proj, "index.htmlclay")
	writeTestFile(t, page, "<html><body>index</body></html>")

	a := newTestApp(t, home)
	s, rel := a.openForTest(t, page)

	req, err := http.NewRequest(http.MethodGet, s.url(rel), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = strings.ToUpper(s.host)
	code, _, body := do(t, http.DefaultClient, req)
	if code != http.StatusOK || !strings.Contains(body, "savetoken") {
		t.Fatalf("an uppercased Host = %d %q, want the site's own file", code, body)
	}
}

func TestStartListenerUsesTheRememberedPort(t *testing.T) {
	port := freeTestPort(t)
	a := buildTestApp(t, t.TempDir(), t.TempDir())
	a.rt.cfg.SetPort(port)
	if !a.startListener() {
		t.Fatal("the app could not bind its remembered port")
	}
	t.Cleanup(a.shutdown)
	if a.rt.port != port {
		t.Fatalf("the app bound %d, want the remembered %d", a.rt.port, port)
	}
}

func TestStartListenerTriesCandidatesInOrder(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	taken := held.Addr().(*net.TCPAddr).Port

	a := buildTestApp(t, t.TempDir(), t.TempDir())
	a.rt.portCandidates = []int{taken, 0}
	logger, read := diagnosticLog(t)
	a.rt.logger = logger
	if !a.startListener() {
		t.Fatal("the app should fall through to the next candidate")
	}
	t.Cleanup(a.shutdown)
	if a.rt.port == taken {
		t.Fatalf("the app bound the taken port %d", taken)
	}
	if log := read(); !strings.Contains(log, fmt.Sprintf("Port %d is not available", taken)) {
		t.Fatalf("the log should name the port it could not take:\n%s", log)
	}
}
