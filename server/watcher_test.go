package server

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/panphora/htmlclay/logging"
	"github.com/panphora/htmlclay/session"
	"github.com/panphora/htmlclay/versions"
)

type watchHarness struct {
	srv   *Server
	file  *session.File
	live  *subscriber
	saved *subscriber
}

func setupWatchTest(t *testing.T, initial string) *watchHarness {
	t.Helper()
	homeDir, _ := filepath.EvalSymlinks(t.TempDir())
	filePath := filepath.Join(homeDir, "watched.htmlclay")
	if err := os.WriteFile(filePath, []byte(initial), 0644); err != nil {
		t.Fatal(err)
	}

	mgr := session.NewManagerWithHome(homeDir)
	f, err := mgr.Register(filePath)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	srv := New(ln, mgr, logging.NewStdout(), versions.New(t.TempDir()))
	srv.watcher.poll = 10 * time.Millisecond
	srv.watcher.quiet = 40 * time.Millisecond

	h := &watchHarness{
		srv:   srv,
		file:  f,
		live:  newSubscriber(f.AbsPath, laneLive),
		saved: newSubscriber(f.AbsPath, laneSaved),
	}
	srv.hub.add(h.live)
	srv.hub.add(h.saved)
	srv.watcher.watch(f)

	t.Cleanup(func() {
		srv.watcher.shutdown()
		srv.hub.shutdown()
	})
	return h
}

// The watcher seeds nothing on its own: a file it has never seen becomes one
// change event once it holds still, so the very first poll cycle is not silently
// swallowed.
func TestWatcherPublishesExternalChange(t *testing.T) {
	h := setupWatchTest(t, "<!DOCTYPE html>\n<html><body>one</body></html>")

	// Mark the starting content as already known, the way a serve or save would.
	h.file.Lock()
	data, _ := os.ReadFile(h.file.AbsPath)
	h.file.RecordServerWrite(versions.Hash(data))
	h.file.Unlock()

	changed := "<!DOCTYPE html>\n<html><body>edited elsewhere</body></html>"
	if err := os.WriteFile(h.file.AbsPath, []byte(changed), 0644); err != nil {
		t.Fatal(err)
	}

	notice := waitFrame(t, h.live, 2*time.Second)
	if notice["type"] != "notification" || notice["msgType"] != "warning" {
		t.Fatalf("live lane got %v, want a warning notification", notice)
	}
	if _, ok := notice["html"]; ok {
		t.Fatal("live lane received content instead of a notice")
	}

	content := waitFrame(t, h.saved, 2*time.Second)
	if content["html"] != changed {
		t.Fatalf("saved lane html = %v", content["html"])
	}

	h.file.Lock()
	stable, lastWrite := h.file.LastStableObservation(), h.file.LastServerWrite()
	h.file.Unlock()
	if stable != versions.Hash([]byte(changed)) {
		t.Fatal("watcher did not advance lastStableObservation")
	}
	if lastWrite == versions.Hash([]byte(changed)) {
		t.Fatal("watcher advanced lastServerWrite, which only server writes may do")
	}
}

// Suppression is by hash and stays valid until content diverges, not on a timer.
// The browser's own write never resurfaces as foreign, however long it sits.
func TestWatcherSuppressesOwnWriteUntilContentDiverges(t *testing.T) {
	h := setupWatchTest(t, "<!DOCTYPE html>\n<html><body>one</body></html>")

	saved := "<!DOCTYPE html>\n<html><body>saved by the browser</body></html>"
	h.file.Lock()
	if err := atomicWriteFile(h.file.AbsPath, []byte(saved)); err != nil {
		h.file.Unlock()
		t.Fatal(err)
	}
	h.file.RecordServerWrite(versions.Hash([]byte(saved)))
	h.file.Unlock()

	// Well past any plausible timer-based suppression window.
	expectNoFrame(t, h.live, 500*time.Millisecond)

	// Rewriting the identical bytes is not a meaningful external change either.
	if err := os.WriteFile(h.file.AbsPath, []byte(saved), 0644); err != nil {
		t.Fatal(err)
	}
	expectNoFrame(t, h.live, 300*time.Millisecond)

	// Diverging content does fire.
	if err := os.WriteFile(h.file.AbsPath, []byte("<html><body>foreign</body></html>"), 0644); err != nil {
		t.Fatal(err)
	}
	waitFrame(t, h.live, 2*time.Second)
}

// A vanished file is not a change event. The absence is recorded and the watcher
// waits, which is also what covers the brief gap during an atomic replacement.
func TestWatcherDeletionIsNotAChangeEvent(t *testing.T) {
	initial := "<!DOCTYPE html>\n<html><body>one</body></html>"
	h := setupWatchTest(t, initial)

	h.file.Lock()
	h.file.RecordServerWrite(versions.Hash([]byte(initial)))
	h.file.Unlock()

	if err := os.Remove(h.file.AbsPath); err != nil {
		t.Fatal(err)
	}

	expectNoFrame(t, h.live, 500*time.Millisecond)
	expectNoFrame(t, h.saved, 10*time.Millisecond)

	// Reappearing with different content is exactly one change event.
	replaced := "<!DOCTYPE html>\n<html><body>replaced</body></html>"
	if err := os.WriteFile(h.file.AbsPath, []byte(replaced), 0644); err != nil {
		t.Fatal(err)
	}

	notice := waitFrame(t, h.live, 2*time.Second)
	if notice["type"] != "notification" {
		t.Fatalf("expected a notification, got %v", notice)
	}
	expectNoFrame(t, h.live, 300*time.Millisecond)
}

// A file that vanishes and returns with the same bytes is not a change at all.
// This is the atomic-replacement gap.
func TestWatcherAtomicReplacementWithSameContentIsSilent(t *testing.T) {
	initial := "<!DOCTYPE html>\n<html><body>one</body></html>"
	h := setupWatchTest(t, initial)

	h.file.Lock()
	h.file.RecordServerWrite(versions.Hash([]byte(initial)))
	h.file.Unlock()

	os.Remove(h.file.AbsPath)
	if err := os.WriteFile(h.file.AbsPath, []byte(initial), 0644); err != nil {
		t.Fatal(err)
	}

	expectNoFrame(t, h.live, 500*time.Millisecond)
}

// A file still being written keeps restarting the quiet interval, so nothing is
// published while it keeps changing.
//
// Deliberately NOT asserted here: that a truncated mid-write file is never
// broadcast. No finite quiet interval can prove a paused non-atomic writer has
// finished, and HasHTMLTag accepts `<html><body>partial`. The promise is
// best-effort stability with a documented paused-writer residual.
func TestWatcherWaitsWhileContentKeepsChanging(t *testing.T) {
	initial := "<!DOCTYPE html>\n<html><body>one</body></html>"
	h := setupWatchTest(t, initial)

	h.file.Lock()
	h.file.RecordServerWrite(versions.Hash([]byte(initial)))
	h.file.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 25; i++ {
			os.WriteFile(h.file.AbsPath, []byte("<html><body>chunk"+string(rune('a'+i))+"</body></html>"), 0644)
			time.Sleep(15 * time.Millisecond)
		}
	}()

	expectNoFrame(t, h.live, 300*time.Millisecond)
	<-done
	// Once it stops moving, the settled content is published exactly once.
	waitFrame(t, h.live, 2*time.Second)
}

// The watcher polls currently-subscribed files only: it starts on the first
// subscriber and stops on the last, because session.Manager never unregisters.
func TestWatcherLifecycleFollowsSubscribers(t *testing.T) {
	h := setupWatchTest(t, "<!DOCTYPE html>\n<html><body>one</body></html>")
	wt := h.srv.watcher

	wt.mu.Lock()
	running, entries := wt.running, len(wt.entries)
	wt.mu.Unlock()
	if !running || entries != 1 {
		t.Fatalf("watcher not running for its one subscriber: running=%v entries=%d", running, entries)
	}

	// A second subscriber on the same file shares the one entry.
	wt.watch(h.file)
	wt.mu.Lock()
	entries = len(wt.entries)
	wt.mu.Unlock()
	if entries != 1 {
		t.Fatalf("expected 1 watch entry, got %d", entries)
	}

	wt.unwatch(h.file)
	wt.mu.Lock()
	running = wt.running
	wt.mu.Unlock()
	if !running {
		t.Fatal("watcher stopped while a subscriber remained")
	}

	wt.unwatch(h.file)
	wt.mu.Lock()
	running, entries = wt.running, len(wt.entries)
	wt.mu.Unlock()
	if running || entries != 0 {
		t.Fatalf("watcher did not stop on the last unwatch: running=%v entries=%d", running, entries)
	}
}
