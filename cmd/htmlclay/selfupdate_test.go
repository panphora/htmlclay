package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/panphora/htmlclay/internal/update"
)

func TestSelfUpdateGate(t *testing.T) {
	writable := func(string) error { return nil }
	readOnly := func(string) error { return update.ErrNotWritable }
	cases := []struct {
		name     string
		channel  string
		version  string
		goos     string
		keys     int
		off      bool
		writable func(string) error
		want     string
	}{
		{"release build on windows", "stable", "1.14.0", "windows", 1, false, writable, ""},
		{"release build on linux", "stable", "1.14.0", "linux", 2, false, writable, ""},
		{"local build", "", "1.14.0", "linux", 1, false, writable, "no update channel"},
		{"dev version", "stable", "1.14.0-dev", "linux", 1, false, writable, "not a release version"},
		{"macOS uses Sparkle", "stable", "1.14.0", "darwin", 1, false, writable, "not darwin"},
		{"no signing key", "stable", "1.14.0", "windows", 0, false, writable, "no feed signing key"},
		{"turned off in config", "stable", "1.14.0", "linux", 1, true, writable, `"updates": "off"`},
		{"install folder read-only", "stable", "1.14.0", "windows", 1, false, readOnly, "not writable"},
	}
	for _, tc := range cases {
		err := selfUpdateGate(tc.channel, tc.version, tc.goos, tc.keys, tc.off, "/opt/htmlclay/htmlclay", tc.writable)
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: gate refused: %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: gate gave %v, want an error containing %q", tc.name, err, tc.want)
		}
	}
}

func TestCountWritesCountsOnlyRequestsThatCanChangeAFile(t *testing.T) {
	a := &app{rt: &appRuntime{}}
	release := make(chan struct{})
	h := a.countWrites(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	srv := httptest.NewServer(h)
	defer srv.Close()

	go http.Get(srv.URL + "/stream")
	done := make(chan struct{})
	go func() {
		http.Post(srv.URL+"/_/save/tok", "text/html", strings.NewReader("x"))
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for a.rt.writesInFlight.Load() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("writesInFlight = %d, want 1 (the POST, not the GET)", a.rt.writesInFlight.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	<-done
	if n := a.rt.writesInFlight.Load(); n != 0 {
		t.Fatalf("writesInFlight = %d after the save finished", n)
	}
}

func TestWaitQuiet(t *testing.T) {
	var n atomic.Int64
	if !waitQuiet(&n, 30*time.Millisecond, time.Second) {
		t.Fatal("an idle counter was not quiet")
	}
	n.Store(1)
	start := time.Now()
	if waitQuiet(&n, 30*time.Millisecond, 200*time.Millisecond) {
		t.Fatal("a busy counter was reported quiet")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("waitQuiet ignored its limit, took %s", took)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		n.Store(0)
	}()
	if !waitQuiet(&n, 30*time.Millisecond, 2*time.Second) {
		t.Fatal("a counter that went idle was never reported quiet")
	}
}

// updateTestApp is a test app whose config dir exists, with an install path
// beside it, so applyUpdate can write the handoff and the update state.
func updateTestApp(t *testing.T) (*app, string) {
	t.Helper()
	home, _ := filepath.EvalSymlinks(t.TempDir())
	a := buildTestApp(t, home, t.TempDir())
	if err := os.MkdirAll(a.rt.configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	install := filepath.Join(t.TempDir(), "htmlclay")
	if err := os.WriteFile(install, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	a.rt.quitForUpdate = func() { t.Error("the app quit after an update that was not applied") }
	return a, install
}

func TestApplyUpdateWaitsForWritesAndCancelsWhenTheyNeverFinish(t *testing.T) {
	a, install := updateTestApp(t)
	defer func(q, l time.Duration) { updateQuietFor, updateQuietLimit = q, l }(updateQuietFor, updateQuietLimit)
	updateQuietFor, updateQuietLimit = 20*time.Millisecond, 150*time.Millisecond
	a.rt.writesInFlight.Store(1)

	err := a.applyUpdate(update.Ready{Version: "9.9.9", StagedPath: filepath.Join(t.TempDir(), "missing")}, install)
	if !errors.Is(err, errUpdateBusy) {
		t.Fatalf("applyUpdate = %v, want errUpdateBusy", err)
	}
	if _, err := os.Stat(filepath.Join(a.rt.configDir, updateHandoffFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a cancelled restart left a handoff behind")
	}
	if a.updating.Load() {
		t.Fatal("a cancelled restart left the app marked as updating")
	}
}

func TestApplyUpdateFailureBeforeTheSwapRemovesTheHandoff(t *testing.T) {
	a, install := updateTestApp(t)
	err := a.applyUpdate(update.Ready{Version: "9.9.9", StagedPath: filepath.Join(t.TempDir(), "missing")}, install)
	if err == nil {
		t.Fatal("an update with no staged binary was applied")
	}
	if _, err := os.Stat(filepath.Join(a.rt.configDir, updateHandoffFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a failed restart left a handoff with live tokens behind")
	}
	if st, _ := update.ReadState(update.UpdatesDir(a.rt.configDir)); st != nil {
		t.Fatalf("a failed restart left update state behind: %+v", st)
	}
	data, _ := os.ReadFile(install)
	if string(data) != "old" {
		t.Fatal("the install changed although the update failed")
	}
	if a.updating.Load() {
		t.Fatal("a failed restart left the app marked as updating")
	}
}
