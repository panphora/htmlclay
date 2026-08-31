package server

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/panphora/htmlclay/internal/htmlutil"
	"github.com/panphora/htmlclay/internal/logging"
	"github.com/panphora/htmlclay/internal/session"
	"github.com/panphora/htmlclay/internal/specwire"
	"github.com/panphora/htmlclay/internal/versions"
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
//
// The deadline is a hang guard rather than part of what any caller asserts, so it
// is generous on purpose: a loaded runner that holds the handle for a while costs
// nothing here, and a fixed short retry budget turns that delay into a failure
// about Windows file sharing rather than about the watcher.
func removeWatched(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := os.Remove(path)
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
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

// The stamp on the frame is the one the save route would compare an If-Match
// against, which is the stamp of the bytes on DISK. The frame's html is the
// browser-facing rendering of those bytes, and it differs (forBrowser injects
// the identity attribute), so stamping the frame's own html instead would hand
// every tab a value the save route can never match and refuse its next save.
func TestWatcherStampsTheDiskBytesNotTheBrowserRendering(t *testing.T) {
	h := setupWatchTest(t, "<!DOCTYPE html>\n<html><body>one</body></html>")

	h.file.Lock()
	data, _ := os.ReadFile(h.file.AbsPath)
	h.file.RecordServerWrite(versions.Hash(data))
	h.file.Unlock()

	// A save token on disk is what makes the two candidate stampings differ:
	// forBrowser strips it, the save route strips it before writing, and this is
	// the shape an external editor leaves behind when someone saves a copy of the
	// served page back over the file.
	changed := "<!DOCTYPE html>\n<html savetoken=\"t-abc\"><body>edited elsewhere</body></html>"
	if err := os.WriteFile(h.file.AbsPath, []byte(changed), 0644); err != nil {
		t.Fatal(err)
	}

	notice := waitFrame(t, h.live, 2*time.Second)
	payload, ok := notice["data"].(map[string]interface{})
	if !ok {
		t.Fatalf("notification carries no data object: %v", notice)
	}
	want := specwire.Etag([]byte(changed))
	if payload["etag"] != want {
		t.Fatalf("etag = %v, want %v (the stamp of the disk bytes)", payload["etag"], want)
	}
	// Prove the two really are different values, or the assertion above proves
	// nothing about which one was stamped.
	served, _ := payload["html"].(string)
	if served == changed || specwire.Etag([]byte(served)) == want {
		t.Fatal("the frame's html equals the disk bytes here, so this test cannot tell the two stampings apart")
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
// The polls are driven by hand and the interval is aged by hand, because the
// wall clock cannot state this property. Racing a writing goroutine against a
// running ticker asserts only "no gap between two writes outlasted the quiet
// interval", which is the scheduler's decision rather than the watcher's: a
// writer descheduled between os.WriteFile's truncate and its write leaves a
// zero-byte file sitting still, and the watcher then correctly publishes it.
// That failure says nothing about the code under test.
//
// Deliberately NOT asserted here: that a truncated mid-write file is never
// broadcast. No finite quiet interval can prove a paused non-atomic writer has
// finished, and HasHTMLTag accepts `<html><body>partial`. The promise is
// best-effort stability with a documented paused-writer residual. Its zero-byte
// case is covered, by TestWatcherDoesNotPublishATruncateAtTheNormalInterval.
func TestWatcherWaitsWhileContentKeepsChanging(t *testing.T) {
	srv, f := setupLiveSyncTest(t)
	wt := srv.watcher
	// An interval no elapsed time in this test can cross, so the only thing that
	// can move the candidate past it is the aging below.
	wt.quiet = time.Hour

	live := newSubscriber(f.AbsPath, laneLive)
	srv.hub.add(live)

	original, err := os.ReadFile(f.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	f.Lock()
	f.RecordServerWrite(versions.Hash(original))
	f.Unlock()

	// Nothing has called watch, so no poll loop exists and every check below is
	// one deliberate poll.
	e := &watchEntry{file: f, refs: 1}

	for i := 0; i < 5; i++ {
		chunk := fmt.Sprintf("<!DOCTYPE html>\n<html><body>chunk %d</body></html>", i)
		if err := os.WriteFile(f.AbsPath, []byte(chunk), 0644); err != nil {
			t.Fatal(err)
		}
		// The candidate left by the previous pass has been held past a whole
		// interval by the aging at the end of the loop. New bytes must restart it
		// anyway: a watcher that replaced the hash without resetting the clock
		// publishes on the very next look.
		wt.check(e)
		wt.check(e)
		e.pendingAt = e.pendingAt.Add(-2 * wt.quiet)
	}
	expectNoFrame(t, live, 50*time.Millisecond)

	// Once it stops moving, the settled content is published exactly once.
	wt.check(e)
	if notice := waitFrame(t, live, 2*time.Second); notice["type"] != "notification" {
		t.Fatalf("live lane got %v, want a notification", notice)
	}
	wt.check(e)
	expectNoFrame(t, live, 50*time.Millisecond)
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
// The polls are driven by hand because the first half asserts an absence, and an
// absence is only evidence once the thing that would have caused it has actually
// happened. Sleeping 300ms and finding the record unadvanced proves nothing if
// the publish had not run yet, and the second half then produces the expected
// frame either way: the whole test passed with the record-on-empty-delivery bug
// present.
func TestExternalChangeWithNoAudienceIsNotRecorded(t *testing.T) {
	srv, f := setupLiveSyncTest(t)
	wt := srv.watcher
	wt.quiet = time.Hour

	original, err := os.ReadFile(f.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	f.Lock()
	f.RecordServerWrite(versions.Hash(original))
	f.Unlock()

	changed := "<!DOCTYPE html>\n<html><body>the agent wrote this</body></html>"
	if err := os.WriteFile(f.AbsPath, []byte(changed), 0644); err != nil {
		t.Fatal(err)
	}

	// Nobody is subscribed, which is the shape a wire handler's watch lease
	// produces: a watched file with no tab open.
	e := &watchEntry{file: f, refs: 1}
	// Registered so the arriving subscriber below goes through the real watch()
	// path. watch takes the existing-entry branch, which sets rearm and returns
	// without starting a ticker, so this stays hand-driven.
	wt.mu.Lock()
	wt.entries[f.AbsPath] = e
	wt.mu.Unlock()

	wt.check(e)
	e.pendingAt = e.pendingAt.Add(-2 * wt.quiet)
	wt.check(e) // this poll publishes, into an empty hub

	f.Lock()
	stable := f.LastStableObservation()
	f.Unlock()
	if stable == versions.Hash([]byte(changed)) {
		t.Fatal("suppression advanced for a change nobody received")
	}

	// A tab arriving afterwards rearms the read, so it learns about the change
	// instead of inheriting a file the watcher has quietly written off. Setting
	// rearm by hand here would let watch stop setting it with this test still
	// green, so the subscriber arrives through watch itself.
	live := newSubscriber(f.AbsPath, laneLive)
	srv.hub.add(live)
	wt.watch(f)

	wt.check(e)
	e.pendingAt = e.pendingAt.Add(-2 * wt.quiet)
	wt.check(e)

	notice := waitFrame(t, live, 2*time.Second)
	if notice["type"] != "notification" {
		t.Fatalf("late subscriber got %v, want a notification", notice)
	}
	f.Lock()
	stable = f.LastStableObservation()
	f.Unlock()
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
	// The gate compares a fingerprint, so the case only exists if the filesystem
	// actually reproduced one. Without this check, a filesystem that rounds the
	// timestamp makes the watcher correctly notice a changed fingerprint and the
	// test fails for having failed to stage itself.
	after, err := os.Stat(f.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Skip("the filesystem did not preserve size and mtime, so the case cannot be staged")
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

// setupEmptyFileTest gives a hand-driven watcher a file with a resolved history
// key, so the versioning half of an empty-file assertion is real rather than
// skipped for want of a key. quiet and emptyQuiet are hours apart, so a test can
// age a candidate past one without reaching the other.
func setupEmptyFileTest(t *testing.T) (*Server, *session.File, *subscriber, *subscriber, string) {
	t.Helper()
	srv, f := setupLiveSyncTest(t)
	srv.watcher.quiet = time.Hour
	srv.watcher.emptyQuiet = 5 * time.Hour

	live := newSubscriber(f.AbsPath, laneLive)
	saved := newSubscriber(f.AbsPath, laneSaved)
	srv.hub.add(live)
	srv.hub.add(saved)

	original, err := os.ReadFile(f.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	f.Lock()
	key, _ := srv.ensureHistoryKeyLocked(f, original)
	f.NoteFirstObservation(versions.Hash(original))
	f.RecordServerWrite(versions.Hash(original))
	f.Unlock()
	if key == "" {
		t.Fatal("no history key was resolved, so this would not test versioning")
	}
	return srv, f, live, saved, key
}

// A writer paused between its truncate and its write leaves an empty file holding
// perfectly still, and at the normal interval that is indistinguishable from
// settled content. Publishing it blanks the reader's tab. The empty candidate must
// therefore survive the NORMAL interval without being published, and the writer's
// real bytes must then publish on their own.
func TestWatcherDoesNotPublishATruncateAtTheNormalInterval(t *testing.T) {
	srv, f, live, saved, key := setupEmptyFileTest(t)
	wt := srv.watcher
	original, err := os.ReadFile(f.AbsPath)
	if err != nil {
		t.Fatal(err)
	}

	// The writer has truncated and not yet written.
	if err := os.WriteFile(f.AbsPath, nil, 0644); err != nil {
		t.Fatal(err)
	}
	e := &watchEntry{file: f, refs: 1}
	wt.check(e)
	// Past the normal interval, nowhere near the empty one.
	e.pendingAt = e.pendingAt.Add(-2 * wt.quiet)
	wt.check(e)

	expectNoFrame(t, live, 50*time.Millisecond)
	expectNoFrame(t, saved, 50*time.Millisecond)
	entries, err := srv.versions.List(key, f.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a truncate was versioned: %d entry/entries", len(entries))
	}
	f.Lock()
	stable := f.LastStableObservation()
	f.Unlock()
	if stable != versions.Hash(original) {
		t.Fatal("an unpublished empty read advanced the suppression record")
	}

	// The writer finishes. Nothing about the held candidate holds this back.
	changed := "<!DOCTYPE html>\n<html><body>the write landed</body></html>"
	if err := os.WriteFile(f.AbsPath, []byte(changed), 0644); err != nil {
		t.Fatal(err)
	}
	wt.check(e)
	e.pendingAt = e.pendingAt.Add(-2 * wt.quiet)
	wt.check(e)

	if notice := waitFrame(t, live, 2*time.Second); notice["type"] != "notification" {
		t.Fatalf("live lane got %v, want a notification", notice)
	}
	// Compared with the identity stripped: a broadcast now re-stamps the document's
	// tracked id, exactly as serving does, so the published bytes are deliberately
	// not byte-identical to what landed on disk. What this test is about is WHICH
	// content was published, so the id is removed before comparing and checked
	// separately.
	published, _ := waitFrame(t, saved, 2*time.Second)["html"].(string)
	if got := string(htmlutil.StripHTMLClayID([]byte(published))); got != changed {
		t.Fatalf("saved lane html = %v", got)
	}
	// The id is asserted unconditionally. Guarding on `id != ""` would make this pass
	// on a build that resolved no identity at all, which is one of the two ways the
	// thing being tested can break.
	id, ok := versions.IDFromKey(key)
	if !ok || id == "" {
		t.Fatalf("no identity was tracked for %q, so the broadcast had none to carry", key)
	}
	if got := htmlutil.ReadHTMLClayID([]byte(published)); got != id {
		t.Fatalf("the broadcast carries identity %q, want the tracked %q: %q", got, id, published)
	}
	entries, err = srv.versions.List(key, f.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("want exactly the one real version, got %d", len(entries))
	}
	newest, err := srv.versions.Read(key, f.AbsPath, entries[0].Name)
	if err != nil {
		t.Fatal(err)
	}
	if string(newest) != changed {
		t.Fatalf("newest version is %q, want the external bytes", newest)
	}
}

// A poke says our own writer finished, so it lets an ordinary candidate skip most
// of its quiet interval. It must not do that for an EMPTY candidate: the writer a
// truncate guards against is the one we cannot see, and a poke says nothing about
// it. The first version of this shortened the hold anyway, because the generic
// first-observation backdating ran before the empty branch could refuse the poke.
func TestWatcherPokeDoesNotShortenTheEmptyHold(t *testing.T) {
	srv, f, live, saved, _ := setupEmptyFileTest(t)
	wt := srv.watcher
	// Margins chosen so the backdating is the only thing that could carry the
	// candidate over the line: aged by 2 minutes it is far short of emptyQuiet,
	// but a backdate of one quiet interval would put it past.
	wt.quiet = time.Hour
	wt.emptyQuiet = time.Hour + time.Minute

	if err := os.WriteFile(f.AbsPath, nil, 0644); err != nil {
		t.Fatal(err)
	}
	e := &watchEntry{file: f, refs: 1}
	e.poke = true
	wt.check(e)
	e.pendingAt = e.pendingAt.Add(-2 * time.Minute)
	wt.check(e)

	expectNoFrame(t, live, 50*time.Millisecond)
	expectNoFrame(t, saved, 50*time.Millisecond)
}

// The other half, and the reason the truncate guard is an interval rather than a
// refusal: a file that is GENUINELY emptied stays empty, so once it has held still
// past the longer interval it publishes and versions like any other change. A flat
// refusal loses this case permanently, because publishConfirmed clears the pending
// candidate and the stat gate then skips an unchanging file on every later poll.
func TestWatcherPublishesAGenuinelyEmptyFileAfterTheLongerInterval(t *testing.T) {
	srv, f, live, saved, key := setupEmptyFileTest(t)
	wt := srv.watcher

	if err := os.WriteFile(f.AbsPath, nil, 0644); err != nil {
		t.Fatal(err)
	}
	e := &watchEntry{file: f, refs: 1}
	wt.check(e)
	// Past the empty interval this time, which is the only difference.
	e.pendingAt = e.pendingAt.Add(-2 * wt.emptyQuiet)
	wt.check(e)

	if notice := waitFrame(t, live, 2*time.Second); notice["type"] != "notification" {
		t.Fatalf("live lane got %v, want a notification", notice)
	}
	if content := waitFrame(t, saved, 2*time.Second); content["html"] != "" {
		t.Fatalf("saved lane html = %v, want the empty document", content["html"])
	}
	entries, err := srv.versions.List(key, f.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("a genuinely empty file must still be versioned, got %d entries", len(entries))
	}
	f.Lock()
	stable := f.LastStableObservation()
	f.Unlock()
	if stable != versions.Hash(nil) {
		t.Fatal("publishing the empty file did not advance the suppression record, so it will resurface as foreign")
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
//
// The polls are driven by hand because the assertion is about their NUMBER, and
// with a ticker running that is the scheduler's to decide: the loop starts when
// the harness calls watch, not when the negative assertion does, so a late test
// goroutine can put the confirming poll inside the window that exists to exclude
// it.
func TestWatcherPokeStillTakesAConfirmingRead(t *testing.T) {
	srv, f := setupLiveSyncTest(t)
	wt := srv.watcher
	// An interval nothing here can wait out: the only route to a publish is the poke.
	wt.quiet = 10 * time.Second

	live := newSubscriber(f.AbsPath, laneLive)
	saved := newSubscriber(f.AbsPath, laneSaved)
	srv.hub.add(live)
	srv.hub.add(saved)

	original, err := os.ReadFile(f.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	f.Lock()
	f.RecordServerWrite(versions.Hash(original))
	f.Unlock()

	changed := "<!DOCTYPE html>\n<html><body>the agent wrote this</body></html>"
	if err := os.WriteFile(f.AbsPath, []byte(changed), 0644); err != nil {
		t.Fatal(err)
	}

	e := &watchEntry{file: f, refs: 1, poke: true}

	// The first poll is this candidate's first and only look at the file.
	wt.check(e)
	expectNoFrame(t, live, 50*time.Millisecond)

	// The poke leaves half a poll on the clock; the confirming poll arrives one
	// whole poll later, which is what the running loop would have done.
	e.pendingAt = e.pendingAt.Add(-wt.poll)
	wt.check(e)
	content := waitFrame(t, saved, 2*time.Second)
	if content["html"] != changed {
		t.Fatalf("saved lane html = %v", content["html"])
	}
}

// A poke that finds nothing to publish is spent all the same. Retaining it until
// something arrived would let a terminal frame shorten the quiet interval of a
// write it has nothing to do with.
//
// Driven by hand so that "the poke has been consumed" is a fact the test can
// check rather than a duration it has to guess: a sleep long enough for a poll
// on an idle machine is not long enough on a loaded one, and a poke that
// outlived the sleep would shorten the later write's interval instead.
func TestWatcherPokeWithNoChangeIsStillSpent(t *testing.T) {
	srv, f := setupLiveSyncTest(t)
	wt := srv.watcher
	wt.quiet = 30 * time.Second

	live := newSubscriber(f.AbsPath, laneLive)
	srv.hub.add(live)

	original, err := os.ReadFile(f.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	f.Lock()
	f.RecordServerWrite(versions.Hash(original))
	f.Unlock()

	e := &watchEntry{file: f, refs: 1, poke: true}

	// The poke finds the file exactly as the server last wrote it: nothing to
	// publish, and spent anyway.
	wt.check(e)
	expectNoFrame(t, live, 50*time.Millisecond)
	if e.poke {
		t.Fatal("a poke that found nothing to publish must still be spent")
	}

	// A later, unrelated write gets a whole quiet interval, not the leftovers of
	// a poke it has nothing to do with.
	if err := os.WriteFile(f.AbsPath, []byte("<html><body>a later, unrelated write</body></html>"), 0644); err != nil {
		t.Fatal(err)
	}
	wt.check(e)
	wt.check(e)
	expectNoFrame(t, live, 50*time.Millisecond)
}

// stageTruncation puts the watcher in the state a poll reaches a moment after an
// external writer has truncated the file and not yet written: the entry is
// registered, so the save path can see it, and holding an empty candidate.
func stageTruncation(t *testing.T, srv *Server, f *session.File) *watchEntry {
	t.Helper()
	if err := os.WriteFile(f.AbsPath, nil, 0644); err != nil {
		t.Fatal(err)
	}
	wt := srv.watcher
	e := &watchEntry{file: f, refs: 1}
	wt.mu.Lock()
	wt.entries[f.AbsPath] = e
	wt.mu.Unlock()
	wt.check(e)
	if !wt.emptyPending(f.AbsPath) {
		t.Fatal("the watcher is not holding an empty candidate, so nothing below tests the guard")
	}
	return e
}

func postSaveRec(t *testing.T, srv *Server, f *session.File, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/_/save/"+f.Token, strings.NewReader(body))
	req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
	req.SetPathValue("token", f.Token)
	w := httptest.NewRecorder()
	srv.handleSave(w, req)
	return w
}

func postSave(t *testing.T, srv *Server, f *session.File, body string) int {
	t.Helper()
	return postSaveRec(t, srv, f, body).Code
}

const truncationDoc = "<!DOCTYPE html>\n<html><body>what the tab still holds</body></html>"

// The window watchEmptyQuiet opens has to be safe, or holding the empty candidate
// for longer is just a longer chance to destroy the writer's work. An autosave
// firing while a truncation is pending must not overwrite it: rename would unlink
// the inode the external writer still has open, and its later write would land on
// a file with no name and be lost with no version of it anywhere.
func TestSaveWillNotOverwriteATruncationInProgress(t *testing.T) {
	srv, f, _, _, _ := setupEmptyFileTest(t)
	stageTruncation(t, srv, f)

	w := postSaveRec(t, srv, f, truncationDoc)
	if w.Code != http.StatusConflict {
		t.Fatalf("a save landing on a pending truncation = %d, want %d", w.Code, http.StatusConflict)
	}
	// The refusal is temporary and lifts itself, so it must not claim to be §6's
	// `conflict`. A client that read that code would treat a condition already on
	// its way out as an If-Match failure and stop autosaving over it.
	var refusal map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &refusal); err != nil {
		t.Fatalf("the refusal body is not JSON: %v (%q)", err, w.Body.String())
	}
	if _, ok := refusal["code"]; ok {
		t.Fatalf("the truncation refusal carries code %q; §3's registry has no name for it and `conflict` is not it", refusal["code"])
	}
	if msg, _ := refusal["msg"].(string); msg == "" {
		t.Fatalf("the truncation refusal carries no msg; a page has nothing to show: %q", w.Body.String())
	}
	onDisk, err := os.ReadFile(f.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(onDisk) != 0 {
		t.Fatalf("the truncated file was overwritten with %d bytes; the external writer's inode is now unlinked", len(onDisk))
	}
}

// And it lifts on its own. Once the empty state has published, the page has been
// shown what is on disk, so a save that still means to overwrite is an informed
// one and must go through.
func TestSaveResumesOnceTheTruncationHasPublished(t *testing.T) {
	srv, f, live, _, _ := setupEmptyFileTest(t)
	wt := srv.watcher
	e := stageTruncation(t, srv, f)

	e.pendingAt = e.pendingAt.Add(-2 * wt.emptyQuiet)
	wt.check(e)
	waitFrame(t, live, 2*time.Second)

	if wt.emptyPending(f.AbsPath) {
		t.Fatal("the candidate is still pending after publishing")
	}
	if code := postSave(t, srv, f, truncationDoc); code != http.StatusOK {
		t.Fatalf("a save after the truncation published = %d, want 200", code)
	}
}

// The guard is keyed on a watcher that is actually watching. With nothing watching
// nothing would ever publish the candidate, so a guard that still fired here could
// never lift and would wedge the file against every future save.
func TestSaveIsNotRefusedWhenNothingIsWatching(t *testing.T) {
	srv, f, _, _, _ := setupEmptyFileTest(t)
	if err := os.WriteFile(f.AbsPath, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if srv.watcher.emptyPending(f.AbsPath) {
		t.Fatal("nothing should be watching this file")
	}
	if code := postSave(t, srv, f, truncationDoc); code != http.StatusOK {
		t.Fatalf("a save with no watcher = %d, want 200: the guard must not be able to wedge a file", code)
	}
}
