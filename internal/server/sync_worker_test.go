package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The worker script is served as-is, as JavaScript, revalidated on every worker
// start, and reachable without the same-origin attestations a stream needs.
func TestSyncWorkerIsServed(t *testing.T) {
	srv, _, _ := setupHandlerTest(t)

	w := getThroughMux(t, srv, "/_/sync/worker.js")
	if w.Code != 200 {
		t.Fatalf("worker script: got %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/javascript; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if v := w.Header().Get("X-Content-Type-Options"); v != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", v)
	}
	if v := w.Header().Get("Cache-Control"); v != "no-cache" {
		t.Errorf("Cache-Control = %q", v)
	}
	body := w.Body.String()
	for _, marker := range []string{"/_/sync?", "\"subscribe\"", "\"gone\"", "v: 1"} {
		if !strings.Contains(body, marker) {
			t.Errorf("worker script lacks %q", marker)
		}
	}
}

// Node has no SharedWorker, so the harness supplies the scope, the ports and the
// EventSource and drives the script through the page contract. Skipped without
// node, as the conformance gate is; GitHub's runners all carry it.
func TestSyncWorkerScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; the worker harness needs it")
	}
	harness, err := filepath.Abs(filepath.Join("..", "..", "testdata", "sync-worker", "harness.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "sync-worker.js")
	if err := os.WriteFile(script, syncWorkerJS, 0644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, harness, script).CombinedOutput()
	if err != nil {
		t.Fatalf("worker harness failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "sync-worker harness: ok") {
		t.Fatalf("worker harness did not report ok:\n%s", out)
	}
}
