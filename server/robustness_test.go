package server

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/panphora/htmlclay/htmlutil"
	"github.com/panphora/htmlclay/logging"
	"github.com/panphora/htmlclay/session"
	"github.com/panphora/htmlclay/versions"
)

func (fx *fileFixture) listVersions(t *testing.T) map[string]interface{} {
	t.Helper()
	req := httptest.NewRequest("GET", "/_/versions/"+fx.file.Token, nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", fx.srv.port)
	req.SetPathValue("token", fx.file.Token)
	w := httptest.NewRecorder()
	fx.srv.handleListVersions(w, req)
	if w.Code != 200 {
		t.Fatalf("list versions: %d %s", w.Code, w.Body.String())
	}
	return decodeJSON(t, w)
}

func (fx *fileFixture) restore(t *testing.T, name string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/_/restore/"+fx.file.Token+"/"+name, nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", fx.srv.port)
	req.SetPathValue("token", fx.file.Token)
	req.SetPathValue("name", name)
	w := httptest.NewRecorder()
	fx.srv.handleRestoreVersion(w, req)
	return w
}

// stripIDOnDisk is what an external editor doing a round trip through a tool that
// does not understand htmlclayid looks like.
func stripIDOnDisk(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, htmlutil.StripHTMLClayID(data), 0644); err != nil {
		t.Fatal(err)
	}
}

// Blocker 1. The history key is resolved once and stored on session.File. When it
// was recomputed per request from whatever was on disk, an external process
// stripping the htmlclayid moved the key to a path hash, so the versions API
// listed nothing while the id-keyed backups sat on disk. That defeats the feature
// in exactly the scenario the stale-write warning points the user at.
func TestVersionsSurviveAnExternalIDStrip(t *testing.T) {
	fx := setupFileTest(t, "notes.htmlclay", page("original"))
	fx.serve(t, "notes.htmlclay")
	if w := fx.save(t, page("second")); w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}

	before := fx.listVersions(t)["versions"].([]interface{})
	if len(before) != 2 {
		t.Fatalf("expected 2 versions before the strip, got %d", len(before))
	}

	stripIDOnDisk(t, fx.file.AbsPath)

	after := fx.listVersions(t)["versions"].([]interface{})
	if len(after) != len(before) {
		t.Fatalf("stripping the htmlclayid lost the history: %d versions, want %d",
			len(after), len(before))
	}

	newest := after[0].(map[string]interface{})["name"].(string)
	if w := fx.restore(t, newest); w.Code != 200 {
		t.Fatalf("restore after the strip: %d %s", w.Code, w.Body.String())
	}
}

// Blocker 1, the deletion half: a file deleted out from under the server must not
// silently move its history to a path-derived key either.
func TestVersionsSurviveAnExternalDelete(t *testing.T) {
	fx := setupFileTest(t, "notes.htmlclay", page("original"))
	fx.serve(t, "notes.htmlclay")

	before := fx.listVersions(t)["versions"].([]interface{})
	if len(before) != 1 {
		t.Fatalf("expected 1 snapshot, got %d", len(before))
	}
	if err := os.Remove(fx.file.AbsPath); err != nil {
		t.Fatal(err)
	}

	after := fx.listVersions(t)["versions"].([]interface{})
	if len(after) != 1 {
		t.Fatalf("deleting the file lost the history: %d versions, want 1", len(after))
	}
}

// Blocker 2. The safety backup before a restore is mandatory. It used to be
// attempted only when the current file read cleanly, while the restore itself
// proceeded whenever the selected version read, so a present-but-unreadable live
// file was destroyed with no recovery copy.
func TestRestoreRefusesWhenTheLiveFileCannotBeRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 file, so the failure cannot be staged")
	}
	fx := setupFileTest(t, "notes.htmlclay", page("original"))
	fx.serve(t, "notes.htmlclay")
	if w := fx.save(t, page("second")); w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}

	entries := fx.history(t)
	if len(entries) < 2 {
		t.Fatalf("need at least 2 versions to restore an older one, got %d", len(entries))
	}
	oldest := entries[len(entries)-1].Name

	if err := os.Chmod(fx.file.AbsPath, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(fx.file.AbsPath, 0644) })

	w := fx.restore(t, oldest)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("restore over an unreadable file returned %d, want 500", w.Code)
	}

	if err := os.Chmod(fx.file.AbsPath, 0644); err != nil {
		t.Fatal(err)
	}
	onDisk, err := os.ReadFile(fx.file.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(onDisk), "second") {
		t.Fatalf("the unreadable file was overwritten with no safety backup: %q", onDisk)
	}
}

// Blocker 3. Two copies of one file, first-opened concurrently, must not both
// keep the id. Checking ownership and claiming it in separate store transactions
// let both see no owner, so neither got a fresh id and both landed in one logical
// history.
func TestConcurrentFirstOpensOfOneIDForkDistinctIdentities(t *testing.T) {
	for attempt := 0; attempt < 20; attempt++ {
		homeDir, _ := filepath.EvalSymlinks(t.TempDir())
		body := pageWithID(testUUID, "shared")
		paths := []string{
			filepath.Join(homeDir, "one.htmlclay"),
			filepath.Join(homeDir, "two.htmlclay"),
		}
		for _, p := range paths {
			if err := os.WriteFile(p, []byte(body), 0644); err != nil {
				t.Fatal(err)
			}
		}

		mgr := session.NewManagerWithHome(homeDir)
		for _, p := range paths {
			if _, err := mgr.Register(p); err != nil {
				t.Fatal(err)
			}
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		srv := New(ln, mgr, logging.NewStdout(), versions.New(t.TempDir()))

		var wg sync.WaitGroup
		for _, p := range paths {
			wg.Add(1)
			go func(rel string) {
				defer wg.Done()
				req := httptest.NewRequest("GET", "/"+rel, nil)
				req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
				req.SetPathValue("path", rel)
				srv.handleServeFile(httptest.NewRecorder(), req)
			}(filepath.Base(p))
		}
		wg.Wait()

		ids := make([]string, 0, len(paths))
		for _, p := range paths {
			data, rErr := os.ReadFile(p)
			if rErr != nil {
				t.Fatal(rErr)
			}
			ids = append(ids, htmlutil.ReadHTMLClayID(data))
		}
		srv.hub.shutdown()
		srv.watcher.shutdown()
		ln.Close()

		if ids[0] == ids[1] {
			t.Fatalf("attempt %d: two copies opened concurrently kept one identity (%s), "+
				"so both share a single history", attempt, ids[0])
		}
	}
}

// Blocker 4a. The watcher must not be able to mark a file observed. When observed
// was a stored flag, an origin-trusted SSE subscription naming a never-served
// file let the watcher set it, and that file's first real GET then skipped both
// clone resolution and its opening snapshot.
func TestWatcherObservationDoesNotSkipFirstOpenWork(t *testing.T) {
	fx := setupFileTest(t, "notes.htmlclay", page("original"))

	// Exactly what the watcher does after confirming a stable read.
	data, err := os.ReadFile(fx.file.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	fx.file.Lock()
	fx.file.RecordStableObservation(versions.Hash(data))
	fx.file.Unlock()

	if w := fx.serve(t, "notes.htmlclay"); w.Code != 200 {
		t.Fatalf("serve: %d", w.Code)
	}

	if entries := fx.history(t); len(entries) != 1 {
		t.Fatalf("the first real GET took %d snapshots, want 1: a watcher observation "+
			"suppressed the first-open work", len(entries))
	}
	onDisk, err := os.ReadFile(fx.file.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	if htmlutil.ReadHTMLClayID(onDisk) == "" {
		t.Fatal("the first real GET skipped identity resolution")
	}
}

// Blocker 4b. The first-open snapshot is published inside f.Lock(). Publishing it
// after the unlock let two concurrent GETs interleave: one captured H0 and was
// descheduled, a save then published H0 and H1, and the descheduled GET published
// its stale H0, leaving history ending below the successful save.
// A plain .html file is used so no htmlclayid injection can change the file
// underneath the comparison: the only writer here is the save.
func TestFirstOpenSnapshotCannotLandAfterALaterSave(t *testing.T) {
	// This is a probabilistic detector, not a deterministic one: the unfixed code
	// only loses the race when the serving goroutine is descheduled between
	// releasing the file lock and publishing, which no finite loop can force. It
	// was confirmed to fail on the unfixed code; the fix's real guarantee is
	// structural, that the publish and the record update are one critical section.
	attempts := 120
	if testing.Short() {
		attempts = 10
	}
	for attempt := 0; attempt < attempts; attempt++ {
		fx := setupFileTest(t, "notes.htmlclay", pageWithID(testUUID, "original"))

		// Other files served at the same time share the one store lock. That
		// contention is what turns the gap after the file lock is released into a
		// window a concurrent save can land inside: the snapshot publish queues for
		// the store while the save takes the file lock and gets its versions in
		// first. Each noise file carries its own identity, so they contend for the
		// store and nothing else.
		noise := make([]string, 0, 8)
		for i := 0; i < 8; i++ {
			name := fmt.Sprintf("noise-%d.htmlclay", i)
			p := filepath.Join(fx.home, name)
			id, err := htmlutil.GenerateHTMLClayID()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(pageWithID(id, name)), 0644); err != nil {
				t.Fatal(err)
			}
			if _, err := fx.srv.sessions.Register(p); err != nil {
				t.Fatal(err)
			}
			noise = append(noise, name)
		}

		var wg sync.WaitGroup
		start := make(chan struct{})
		for _, name := range noise {
			wg.Add(1)
			go func(n string) {
				defer wg.Done()
				<-start
				fx.serve(t, n)
			}(name)
		}
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				fx.serve(t, "notes.htmlclay")
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			fx.save(t, page("saved"))
		}()
		close(start)
		wg.Wait()

		onDisk, err := os.ReadFile(fx.file.AbsPath)
		if err != nil {
			t.Fatal(err)
		}
		entries := fx.history(t)
		if len(entries) == 0 {
			t.Fatalf("attempt %d: no history at all", attempt)
		}
		newest := fx.versionBody(t, entries[0].Name)
		if versions.Hash([]byte(newest)) != versions.Hash(onDisk) {
			t.Fatalf("attempt %d: history ends at a version that is not what is on disk, "+
				"so restoring the latest is wrong.\n newest: %q\n on disk: %q",
				attempt, newest, onDisk)
		}
	}
}

// Blocker 5. An htmlclayid injection is a server write on every serve, not only
// the first. When the record update was guarded by firstServe, an external editor
// stripping the id followed by a reload had the server inject a fresh id, write
// disk, leave both records untouched, and then warn stale against its own edit.
func TestIDInjectionOnALaterServeAdvancesTheRecords(t *testing.T) {
	fx := setupFileTest(t, "notes.htmlclay", page("original"))
	fx.serve(t, "notes.htmlclay")

	stripIDOnDisk(t, fx.file.AbsPath)

	// The reload: the server notices the missing id and writes a fresh one.
	if w := fx.serve(t, "notes.htmlclay"); w.Code != 200 {
		t.Fatalf("second serve: %d", w.Code)
	}
	onDisk, err := os.ReadFile(fx.file.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	if htmlutil.ReadHTMLClayID(onDisk) == "" {
		t.Fatal("the reload did not re-inject an htmlclayid")
	}

	fx.file.Lock()
	recorded := fx.file.LastServerWrite()
	stable := fx.file.LastStableObservation()
	fx.file.Unlock()
	if want := versions.Hash(onDisk); recorded != want || stable != want {
		t.Fatalf("the injection did not advance both records: lastServerWrite=%q "+
			"lastStableObservation=%q want %q", recorded, stable, want)
	}

	w := fx.save(t, page("after reload"))
	if w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	if got := decodeJSON(t, w)["msgType"]; got != "success" {
		t.Fatalf("the save warned %q against the server's own injection", got)
	}
}

// Finding 6. Every frame carries an SSE id, and a subscriber reconnecting with
// Last-Event-ID is replayed what it missed. Without that, an event relayed while
// no subscriber was registered was simply gone.
func TestFramesCarryAnSSEID(t *testing.T) {
	h := newHub("")
	sub := newSubscriber("/tmp/a.html", laneLive)
	h.add(sub)

	h.relay("/tmp/a.html", "<html>peer</html>", "c1", nil)

	select {
	case raw := <-sub.ch:
		id, payload := splitFrame(t, raw)
		if seq, _ := payload["seq"].(float64); int64(seq) != id {
			t.Fatalf("SSE id %d does not match the payload seq %v", id, payload["seq"])
		}
	case <-time.After(time.Second):
		t.Fatal("no frame")
	}
}

func TestReplayDeliversFramesMissedWhileDisconnected(t *testing.T) {
	h := newHub("")

	// Nobody is listening: this is the connection-setup gap, and the eviction gap.
	h.relay("/tmp/a.html", "<html>one</html>", "c1", nil)
	h.relay("/tmp/a.html", "<html>two</html>", "c1", nil)

	h.mu.Lock()
	buffered := h.replay[replayKey("/tmp/a.html", laneLive)]
	h.mu.Unlock()
	if len(buffered) != 2 {
		t.Fatalf("hub retained %d frames, want 2", len(buffered))
	}
	resumeFrom := buffered[0].seq

	sub := newSubscriber("/tmp/a.html", laneLive)
	sub.lastEventID = resumeFrom
	h.add(sub)

	msg := waitFrame(t, sub, time.Second)
	if msg["html"] != "<html>two</html>" {
		t.Fatalf("replay delivered %v, want the frame after the resume point", msg["html"])
	}
	expectNoFrame(t, sub, 100*time.Millisecond)
}

func TestFreshSubscriberIsNotReplayedTo(t *testing.T) {
	h := newHub("")
	h.relay("/tmp/a.html", "<html>old</html>", "c1", nil)

	sub := newSubscriber("/tmp/a.html", laneLive)
	h.add(sub)
	expectNoFrame(t, sub, 100*time.Millisecond)
}

func TestEvictedSubscriberRecoversItsEventsOnReconnect(t *testing.T) {
	h := newHub("")
	sub := newSubscriber("/tmp/a.html", laneLive)
	h.add(sub)

	for i := 0; i < subQueueSize+3; i++ {
		h.relay("/tmp/a.html", fmt.Sprintf("<html>%d</html>", i), "c1", nil)
	}
	select {
	case <-sub.done:
	case <-time.After(time.Second):
		t.Fatal("the overflowing subscriber was never evicted")
	}

	// Drain what it did receive, then reconnect from there.
	var lastSeen int64
	for draining := true; draining; {
		select {
		case raw := <-sub.ch:
			id, _ := splitFrame(t, raw)
			lastSeen = id
		default:
			draining = false
		}
	}

	next := newSubscriber("/tmp/a.html", laneLive)
	next.lastEventID = lastSeen
	h.add(next)

	msg := waitFrame(t, next, time.Second)
	if msg["html"] == nil {
		t.Fatal("reconnecting after an eviction replayed nothing")
	}
}

// Finding 7. A poll that is already in flight when the last subscriber leaves
// must not record its hash: doing so advanced lastStableObservation for a change
// nobody received, and that change was then suppressed forever on reconnect.
func TestWatcherDoesNotRecordAfterTheLastSubscriberLeaves(t *testing.T) {
	h := setupWatchTest(t, page("original"))

	// The tick loop copies entry pointers outside the watcher lock, so this is the
	// pointer an in-flight check would still be holding.
	h.srv.watcher.mu.Lock()
	entry := h.srv.watcher.entries[h.file.AbsPath]
	h.srv.watcher.mu.Unlock()
	if entry == nil {
		t.Fatal("the watched file has no entry")
	}

	if err := os.WriteFile(h.file.AbsPath, []byte(page("changed outside")), 0644); err != nil {
		t.Fatal(err)
	}
	h.srv.watcher.unwatch(h.file)

	// Run the orphaned check to completion, twice, so it clears its quiet interval.
	h.srv.watcher.check(entry)
	time.Sleep(2 * h.srv.watcher.quiet)
	h.srv.watcher.check(entry)

	h.file.Lock()
	stable := h.file.LastStableObservation()
	h.file.Unlock()
	if stable == versions.Hash([]byte(page("changed outside"))) {
		t.Fatal("an orphaned check recorded the change, so it is suppressed forever " +
			"and the user never sees it")
	}
}

// Finding 9. The ETag is content-derived. A metadata-only validator returned 304
// for a same-size replacement with a preserved timestamp, so the browser kept
// stale bytes, while the watcher path explicitly accounts for that exact
// replacement pattern.
func TestAssetETagIsContentDerived(t *testing.T) {
	fx := setupAssetTest(t, "style.css", []byte("body{color:red}"))
	assetPath := filepath.Join(fx.home, "assets", "style.css")

	first := serveAssetRequest(t, fx, "assets/style.css", nil)
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("asset has no ETag")
	}
	info, err := os.Stat(assetPath)
	if err != nil {
		t.Fatal(err)
	}

	// A same-size replacement with the timestamp preserved: what an editor writing
	// through a tool that restores mtime, or a checkout, produces.
	if err := os.WriteFile(assetPath, []byte("body{color:BLU}"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(assetPath, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(assetPath)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		t.Skip("the filesystem did not preserve size and mtime, so the case cannot be staged")
	}

	second := serveAssetRequest(t, fx, "assets/style.css", map[string]string{"If-None-Match": etag})
	if second.Code == http.StatusNotModified {
		t.Fatal("a same-size, same-timestamp replacement returned 304, so the browser " +
			"keeps stale bytes")
	}
	if got := second.Header().Get("ETag"); got == etag {
		t.Fatalf("the ETag %q did not change when the content did", got)
	}
}

// Finding 10. A backward clock change plus a restart must not put the sequence
// below what an open client retained, which would make both clients discard every
// update until real time caught up.
func TestSequenceResumesAbovePersistedHighWaterMark(t *testing.T) {
	dir := t.TempDir()
	seqPath := filepath.Join(dir, ".livesync-seq")

	first := newHub(seqPath)
	first.mu.Lock()
	handedOut := first.nextSeq()
	first.mu.Unlock()

	if _, err := os.Stat(seqPath); err != nil {
		t.Fatalf("the high-water mark was never persisted: %v", err)
	}

	// The restart. Wall clock alone would seed at roughly handedOut, so a clock
	// that had rolled backwards would seed below it.
	second := newHub(seqPath)
	if second.seq <= handedOut {
		t.Fatalf("restarted hub seeded at %d, at or below the %d already handed out",
			second.seq, handedOut)
	}

	// The rollback itself: a mark far in the future must still be respected.
	future := time.Now().Add(72 * time.Hour).UnixMilli()
	writeSeqHighWater(seqPath, future)
	third := newHub(seqPath)
	if third.seq <= future {
		t.Fatalf("hub seeded at %d, below the persisted high-water mark %d", third.seq, future)
	}
}

// Finding 11. A blocked SSE write must not outlive the shutdown budget, or
// graceful shutdown necessarily times out into a forced close.
func TestSSEWriteDeadlineFitsInsideTheShutdownBudget(t *testing.T) {
	if sseWriteDeadline >= ShutdownBudget {
		t.Fatalf("sseWriteDeadline %v is not under ShutdownBudget %v, so a blocked write "+
			"always forces shutdown to time out", sseWriteDeadline, ShutdownBudget)
	}
}
