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

// removeWatched deletes a file the watcher is actively polling.
//
// These tests deliberately run the watcher at a 10ms poll, and Windows refuses to
// delete a file another handle has open. The watcher's read lasts microseconds, so
// retrying clears it. Unix deletes on the first try and never sleeps.
func removeWatched(t *testing.T, path string) {
	t.Helper()
	var err error
	for _, backoff := range []time.Duration{0, 5, 15, 40, 100} {
		if backoff > 0 {
			time.Sleep(backoff * time.Millisecond)
		}
		if err = os.Remove(path); err == nil {
			return
		}
	}
	t.Fatal(err)
}

func setupWatchTest(t *testing.T, initial string) *watchHarness {
	t.Helper()
	return newWatchHarness(t, initial, true)
}

// newWatchHarness watches a file with or without live subscribers behind the
// watch. Without them it is the shape a wire handler's lease produces: a watched
// file with nobody listening.
//
// tune runs before the polling goroutine starts, which is the only safe moment to
// change the intervals: the loop reads them from the watcher itself.
func newWatchHarness(t *testing.T, initial string, subscribe bool, tune ...func(*watcher)) *watchHarness {
	t.Helper()
	homeDir, _ := filepath.EvalSymlinks(t.TempDir())
	filePath := filepath.Join(homeDir, "watched.htmlclay")
	if err := os.WriteFile(filePath, []byte(initial), 0644); err != nil {
		t.Fatal(err)
	}

	mgr := newTestManager(t, homeDir)
	f, err := mgr.Register(filePath, session.ViaOsOpen)
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
	for _, fn := range tune {
		fn(srv.watcher)
	}

	h := &watchHarness{
		srv:   srv,
		file:  f,
		live:  newSubscriber(f.AbsPath, laneLive),
		saved: newSubscriber(f.AbsPath, laneSaved),
	}
	if subscribe {
		srv.hub.add(h.live)
		srv.hub.add(h.saved)
	}
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

	removeWatched(t, h.file.AbsPath)

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

	removeWatched(t, h.file.AbsPath)
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

// watchedPath is the test's own probe for a live watch entry. The production
// code no longer asks the question: publishing gates on delivery now, not on the
// watch, and the lease is the whole reason those are different.
func watchedPath(wt *watcher, path string) bool {
	wt.mu.Lock()
	defer wt.mu.Unlock()
	e, ok := wt.entries[path]
	return ok && !e.removed
}

// A change published into an empty hub must not advance suppression. A wire
// handler's watch lease is the first thing that makes a file watched with nobody
// subscribed, and recording the hash there would hide the change: the retained
// frame ages out, and every later poll sees content the watcher believes it has
// already reported.
func TestExternalChangeWithNoAudienceIsNotRecorded(t *testing.T) {
	h := newWatchHarness(t, "<!DOCTYPE html>\n<html><body>one</body></html>", false)

	h.file.Lock()
	data, _ := os.ReadFile(h.file.AbsPath)
	h.file.RecordServerWrite(versions.Hash(data))
	h.file.Unlock()

	changed := "<!DOCTYPE html>\n<html><body>the agent wrote this</body></html>"
	if err := os.WriteFile(h.file.AbsPath, []byte(changed), 0644); err != nil {
		t.Fatal(err)
	}

	// Well past the 40ms quiet interval and many 10ms polls.
	time.Sleep(300 * time.Millisecond)

	h.file.Lock()
	stable := h.file.LastStableObservation()
	h.file.Unlock()
	if stable == versions.Hash([]byte(changed)) {
		t.Fatal("suppression advanced for a change nobody received")
	}

	// A tab arriving afterwards rearms the read, so it learns about the change
	// instead of inheriting a file the watcher has quietly written off.
	live := newSubscriber(h.file.AbsPath, laneLive)
	h.srv.hub.add(live)
	h.srv.watcher.watch(h.file)

	notice := waitFrame(t, live, 2*time.Second)
	if notice["type"] != "notification" {
		t.Fatalf("late subscriber got %v, want a notification", notice)
	}
	h.file.Lock()
	stable = h.file.LastStableObservation()
	h.file.Unlock()
	if stable != versions.Hash([]byte(changed)) {
		t.Fatal("delivery to a real subscriber did not advance suppression")
	}
}

// The gate is what makes a lease affordable, and this is the honest statement of
// its cost: a write that lands with the same size, modtime and file identity is
// invisible until the next write metadata can see.
//
// The poll is driven by hand here rather than by the loop. With a ticker running
// there is a window between writing the same-length bytes and putting the
// timestamp back in which a tick can arm a candidate, and a pending candidate
// bypasses the gate on every later poll, so the test fails for a reason that has
// nothing to do with what it checks.
func TestWatcherReadGateSkipsAnIdenticalFingerprint(t *testing.T) {
	srv, f := setupLiveSyncTest(t)
	wt := srv.watcher
	wt.quiet = 0

	live := newSubscriber(f.AbsPath, laneLive)
	srv.hub.add(live)

	original, err := os.ReadFile(f.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	f.Lock()
	f.RecordServerWrite(versions.Hash(original))
	f.Unlock()

	// Nothing has called watch, so no poll loop exists and this entry is the only
	// thing touching the file.
	e := &watchEntry{file: f, refs: 1}
	wt.check(e) // reads, and arms the fingerprint
	wt.check(e) // clears the candidate: the content is already known

	before, err := os.Stat(f.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	sneaky := []byte("<!DOCTYPE html>\n<html><body>ho</body></html>")
	if len(sneaky) != int(before.Size()) {
		t.Fatalf("fixture lengths diverged: %d vs %d", len(sneaky), before.Size())
	}
	if err := os.WriteFile(f.AbsPath, sneaky, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(f.AbsPath, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}

	wt.check(e)
	wt.check(e)
	expectNoFrame(t, live, 50*time.Millisecond)

	// The next write metadata can see surfaces the current content, which is the
	// bound on how long the miss lasts.
	repaired := "<!DOCTYPE html>\n<html><body>three, and longer</body></html>"
	if err := os.WriteFile(f.AbsPath, []byte(repaired), 0644); err != nil {
		t.Fatal(err)
	}
	wt.check(e)
	wt.check(e)
	if notice := waitFrame(t, live, time.Second); notice["type"] != "notification" {
		t.Fatalf("live lane got %v, want a notification", notice)
	}
}

// An external edit is the one write to a file that no other path versions.
func TestWatcherVersionsAnExternalChange(t *testing.T) {
	h := setupWatchTest(t, "<!DOCTYPE html>\n<html><body>one</body></html>")

	// Resolve a history key the way a serve would, then record the starting
	// content as known.
	h.file.Lock()
	data, _ := os.ReadFile(h.file.AbsPath)
	key, _ := h.srv.ensureHistoryKeyLocked(h.file, data)
	h.file.RecordServerWrite(versions.Hash(data))
	h.file.Unlock()

	changed := "<!DOCTYPE html>\n<html><body>edited elsewhere</body></html>"
	if err := os.WriteFile(h.file.AbsPath, []byte(changed), 0644); err != nil {
		t.Fatal(err)
	}
	waitFrame(t, h.saved, 2*time.Second)

	entries, err := h.srv.versions.List(key, h.file.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("external change was broadcast but never versioned")
	}
	// List is newest-first.
	newest, err := h.srv.versions.Read(key, h.file.AbsPath, entries[0].Name)
	if err != nil {
		t.Fatal(err)
	}
	if string(newest) != changed {
		t.Fatalf("newest version is %q, want the external bytes", newest)
	}
}

// A file with no resolved identity is not versioned by the watcher: minting one
// here would claim a durable id for a file nobody has opened.
func TestWatcherDoesNotVersionAFileWithNoHistoryKey(t *testing.T) {
	h := setupWatchTest(t, "<!DOCTYPE html>\n<html><body>one</body></html>")

	h.file.Lock()
	data, _ := os.ReadFile(h.file.AbsPath)
	h.file.RecordServerWrite(versions.Hash(data))
	hadKey := h.file.HistoryKey()
	h.file.Unlock()
	if hadKey != "" {
		t.Fatalf("fixture already has a history key: %q", hadKey)
	}

	changed := "<!DOCTYPE html>\n<html><body>edited elsewhere</body></html>"
	if err := os.WriteFile(h.file.AbsPath, []byte(changed), 0644); err != nil {
		t.Fatal(err)
	}
	// The broadcast still happens; only the backup is skipped.
	waitFrame(t, h.saved, 2*time.Second)

	h.file.Lock()
	key := h.file.HistoryKey()
	h.file.Unlock()
	if key != "" {
		t.Fatalf("the watcher resolved an identity: %q", key)
	}
}

// The poke: a wire handler that reports it finished writing publishes its change
// on the next poll instead of waiting out the quiet interval. The interval here is
// long enough that nothing below can publish by waiting, so every publish is the
// poke's doing.
func TestWatcherPokePublishesWithoutTheQuietInterval(t *testing.T) {
	initial := "<!DOCTYPE html>\n<html><body>one</body></html>"
	h := newWatchHarness(t, initial, true, func(wt *watcher) {
		wt.quiet = 30 * time.Second
	})

	h.file.Lock()
	h.file.RecordServerWrite(versions.Hash([]byte(initial)))
	h.file.Unlock()

	changed := "<!DOCTYPE html>\n<html><body>the agent wrote this</body></html>"
	if err := os.WriteFile(h.file.AbsPath, []byte(changed), 0644); err != nil {
		t.Fatal(err)
	}
	expectNoFrame(t, h.live, 200*time.Millisecond)

	h.srv.watcher.poke(h.file.AbsPath)

	notice := waitFrame(t, h.live, 2*time.Second)
	if notice["type"] != "notification" {
		t.Fatalf("live lane got %v, want a notification", notice)
	}
	content := waitFrame(t, h.saved, 2*time.Second)
	if content["html"] != changed {
		t.Fatalf("saved lane html = %v", content["html"])
	}
}

// One poll of validity. A poke that outlived its request would shorten an
// unrelated later write's quiet interval, which is the one thing that interval
// exists to prevent.
func TestWatcherPokeDoesNotOutliveItsPoll(t *testing.T) {
	initial := "<!DOCTYPE html>\n<html><body>one</body></html>"
	h := newWatchHarness(t, initial, true, func(wt *watcher) {
		wt.quiet = 30 * time.Second
	})

	h.file.Lock()
	h.file.RecordServerWrite(versions.Hash([]byte(initial)))
	h.file.Unlock()

	if err := os.WriteFile(h.file.AbsPath, []byte("<html><body>the agent wrote this</body></html>"), 0644); err != nil {
		t.Fatal(err)
	}
	h.srv.watcher.poke(h.file.AbsPath)
	waitFrame(t, h.live, 2*time.Second)

	if err := os.WriteFile(h.file.AbsPath, []byte("<html><body>a later, unrelated write</body></html>"), 0644); err != nil {
		t.Fatal(err)
	}
	expectNoFrame(t, h.live, 300*time.Millisecond)
}

// A poke for a file nobody is watching is a no-op, not a panic: a handler can
// report done after its own lease has been released.
func TestWatcherPokeForAnUnwatchedFileIsHarmless(t *testing.T) {
	h := setupWatchTest(t, "<!DOCTYPE html>\n<html><body>one</body></html>")
	h.srv.watcher.poke(filepath.Join(filepath.Dir(h.file.AbsPath), "never-watched.htmlclay"))
}

// A poke arriving before the write is first seen still takes a confirming read.
// Publishing on a single read is what the quiet interval exists to prevent, and
// the poke shortens that interval rather than replacing it.
func TestWatcherPokeStillTakesAConfirmingRead(t *testing.T) {
	initial := "<!DOCTYPE html>\n<html><body>one</body></html>"
	h := newWatchHarness(t, initial, true, func(wt *watcher) {
		// Slow polls and an interval nothing here can wait out: the only route to
		// a publish is the poke, and the polls are far enough apart to count.
		wt.poll = 500 * time.Millisecond
		wt.quiet = 10 * time.Second
	})

	h.file.Lock()
	h.file.RecordServerWrite(versions.Hash([]byte(initial)))
	h.file.Unlock()

	changed := "<!DOCTYPE html>\n<html><body>the agent wrote this</body></html>"
	if err := os.WriteFile(h.file.AbsPath, []byte(changed), 0644); err != nil {
		t.Fatal(err)
	}
	h.srv.watcher.poke(h.file.AbsPath)

	// The first poll is this candidate's first and only look at the file.
	expectNoFrame(t, h.live, 600*time.Millisecond)

	// The second poll finds the same bytes and publishes.
	content := waitFrame(t, h.saved, 3*time.Second)
	if content["html"] != changed {
		t.Fatalf("saved lane html = %v", content["html"])
	}
}

// A poke that finds nothing to publish is spent all the same. Retaining it until
// something arrived would let a terminal frame shorten the quiet interval of a
// write it has nothing to do with.
func TestWatcherPokeWithNoChangeIsStillSpent(t *testing.T) {
	initial := "<!DOCTYPE html>\n<html><body>one</body></html>"
	h := newWatchHarness(t, initial, true, func(wt *watcher) {
		wt.quiet = 30 * time.Second
	})

	h.file.Lock()
	h.file.RecordServerWrite(versions.Hash([]byte(initial)))
	h.file.Unlock()

	h.srv.watcher.poke(h.file.AbsPath)
	expectNoFrame(t, h.live, 200*time.Millisecond) // several polls, nothing to publish

	if err := os.WriteFile(h.file.AbsPath, []byte("<html><body>a later, unrelated write</body></html>"), 0644); err != nil {
		t.Fatal(err)
	}
	expectNoFrame(t, h.live, 400*time.Millisecond)
}
