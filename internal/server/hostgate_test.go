package server

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/panphora/htmlclay/internal/logging"
	"github.com/panphora/htmlclay/internal/session"
	"github.com/panphora/htmlclay/internal/versions"
)

// The Malleable HTML File conformance page, run against a real listener.
//
// It is a browser test and cannot be anything else. The spec requires exact origin
// validation on every save, so a save can only be proven from the origin that will be
// allowed to make it, and the cross-origin check needs a real sandboxed iframe with a
// real opaque origin. Neither survives being reimplemented as an httptest request, and
// the Go tests beside this file deliberately do not try: they check the handlers, this
// checks the contract a document on the open web is written against.
//
// Skipped unless HTMLCLAY_HOST_GATE=1, because it needs node, playwright and a
// downloaded browser, which is a heavy thing to put in the way of `go test ./...` on a
// developer's machine. HTMLCLAY_HOST_GATE_RUNNER points at the runner in the
// malleablehtmlfile repo; HTMLCLAY_HOST_GATE_CWD is a directory with playwright
// installed, since the runner resolves it from the caller's directory.
func TestMalleableHTMLFileHostGate(t *testing.T) {
	if os.Getenv("HTMLCLAY_HOST_GATE") != "1" {
		t.Skip("set HTMLCLAY_HOST_GATE=1 to run the conformance page against this host")
	}
	runner := os.Getenv("HTMLCLAY_HOST_GATE_RUNNER")
	if runner == "" {
		t.Fatal("HTMLCLAY_HOST_GATE_RUNNER must point at malleablehtmlfile/scripts/host-test.mjs")
	}
	pageSource := filepath.Join(filepath.Dir(runner), "..", "host-test.html")

	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// The page has to be a REGISTERED file, not merely one sitting in the folder: a
	// save token is minted at registration and binds to one file, so an unregistered
	// copy would be served token-free and the run would test the tokenless lane
	// against a host that does not have one.
	page, err := os.ReadFile(pageSource)
	if err != nil {
		t.Fatalf("reading the conformance page: %v", err)
	}
	const name = "_mhf-host-test.htmlclay"
	if err := os.WriteFile(filepath.Join(home, name), page, 0644); err != nil {
		t.Fatal(err)
	}

	mgr := newTestManager(t, home)
	if _, err := mgr.Register(filepath.Join(home, name), session.ViaOsOpen); err != nil {
		t.Fatalf("registering the conformance page: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := New(ln, mgr, logging.NewStdout(), versions.New(t.TempDir()))
	go srv.Start()
	t.Cleanup(func() { ln.Close() })

	port := ln.Addr().(*net.TCPAddr).Port
	waitForListener(t, port)

	// No --root: the host placed the page itself, because only the host can register
	// it. --token-from-page takes the token the host injected at serve time rather
	// than inventing one, which is the only honest way to test a token host.
	cmd := exec.Command("node", runner,
		"--url", "http://127.0.0.1:"+strconv.Itoa(port),
		"--page", name,
		"--token-from-page",
	)
	if cwd := os.Getenv("HTMLCLAY_HOST_GATE_CWD"); cwd != "" {
		cmd.Dir = cwd
	}
	out, runErr := cmd.CombinedOutput()
	t.Log("\n" + string(out))
	if runErr != nil {
		t.Fatalf("the conformance page reported failures: %v", runErr)
	}
	// A runner that exits 0 having produced nothing would pass this silently.
	if !strings.Contains(string(out), " passed,") {
		t.Fatal("the runner produced no summary line; the run did not happen")
	}
}

func waitForListener(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 200*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the server never accepted a connection on %d", port)
}
