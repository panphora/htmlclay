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

// H3. Restore identity comes from the stored history key, not the mutable live
// bytes. An external process stripping the id, or deleting the file, must not make
// a restore write id-free bytes that orphan the id: history under a path key on
// the next serve.
func TestRestoreKeepsHistoryKeyIdentity(t *testing.T) {
	cases := []struct {
		name    string
		corrupt func(t *testing.T, path string)
	}{
		{"external strip", func(t *testing.T, path string) { stripIDOnDisk(t, path) }},
		{"external delete", func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := setupFileTest(t, "notes.htmlclay", pageWithID(testUUID, "v1"))
			fx.serve(t, "notes.htmlclay")
			if w := fx.save(t, page("v2")); w.Code != 200 {
				t.Fatalf("save: %d %s", w.Code, w.Body.String())
			}

			entries := fx.history(t)
			if len(entries) < 2 {
				t.Fatalf("need at least 2 versions, got %d", len(entries))
			}
			// The oldest version carries testUUID: an A-backed version.
			target := entries[len(entries)-1].Name

			c.corrupt(t, fx.file.AbsPath)

			if w := fx.restore(t, target); w.Code != 200 {
				t.Fatalf("restore: %d %s", w.Code, w.Body.String())
			}

			onDisk, err := os.ReadFile(fx.file.AbsPath)
			if err != nil {
				t.Fatal(err)
			}
			// The host asserts no identity on disk; the tracked key is what carries it.
			if got := htmlutil.ReadHTMLClayID(onDisk); got != "" {
				t.Fatalf("restored bytes assert an identity on disk: %q", got)
			}
			if got := fx.key(t); got != "id:"+testUUID {
				t.Fatalf("restore moved the tracked key to %q, want id:%s", got, testUUID)
			}
			sw := fx.serve(t, "notes.htmlclay")
			if sw.Code != 200 {
				t.Fatalf("serve after restore: %d", sw.Code)
			}
			if got := htmlutil.ReadHTMLClayID(sw.Body.Bytes()); !strings.EqualFold(got, testUUID) {
				t.Fatalf("the serve after a restore carries id %q, want %s", got, testUUID)
			}

			// Restart survival: a fresh store over the same directory still lists
			// the A history and no second folder was created.
			base, err := fx.srv.versions.Dir()
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadDir(base)
			if err != nil {
				t.Fatal(err)
			}
			fresh := versions.New(base)
			list, err := fresh.List("id:"+testUUID, fx.file.AbsPath)
			if err != nil {
				t.Fatal(err)
			}
			if len(list) == 0 {
				t.Fatal("the A history did not survive a restart")
			}
			after, err := os.ReadDir(base)
			if err != nil {
				t.Fatal(err)
			}
			if len(after) != len(before) {
				t.Fatalf("a second history folder appeared: %d -> %d", len(before), len(after))
			}
		})
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
		files := make([]*session.File, 0, len(paths))
		for _, p := range paths {
			f, err := mgr.Register(p)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, f)
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		srv := New(ln, mgr, logging.NewStdout(), versions.New(t.TempDir()))

		var wg sync.WaitGroup
		served := make([]*httptest.ResponseRecorder, len(paths))
		for i, p := range paths {
			wg.Add(1)
			go func(idx int, rel string) {
				defer wg.Done()
				req := httptest.NewRequest("GET", "/"+rel, nil)
				req.Host = fmt.Sprintf("127.0.0.1:%d", srv.port)
				req.SetPathValue("path", rel)
				w := httptest.NewRecorder()
				srv.handleServeFile(w, req)
				served[idx] = w
			}(i, filepath.Base(p))
		}
		wg.Wait()

		// Serving never rewrites disk, so both copies still carry the shared id
		// there. The fork lives in the tracked key and the bytes served.
		ids := make([]string, 0, len(paths))
		for i, f := range files {
			f.Lock()
			key := f.HistoryKey()
			f.Unlock()
			id, ok := versions.IDFromKey(key)
			if !ok {
				t.Fatalf("attempt %d: %s tracks %q, want an id: key", attempt, paths[i], key)
			}
			if got := htmlutil.ReadHTMLClayID(served[i].Body.Bytes()); got != id {
				t.Fatalf("attempt %d: %s served id %q but tracks %q", attempt, paths[i], got, id)
			}
			ids = append(ids, id)
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
	if key := fx.key(t); !strings.HasPrefix(key, "id:") {
		t.Fatalf("the first real GET skipped identity resolution: key is %q", key)
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

// Blocker 5, under model B′. The host never writes an identity to disk, so a
// reload can no longer manufacture a stale warning against an edit of its own:
// there is nothing on disk for an external strip to take away, which is the whole
// point. What a later serve must still do is self-heal, re-anchoring the file to
// the tracked identity in the bytes it serves and leaving disk byte-for-byte as it
// found it.
func TestLaterServeSelfHealsTheIDWithoutWritingDisk(t *testing.T) {
	fx := setupFileTest(t, "notes.htmlclay", page("original"))

	first := fx.serve(t, "notes.htmlclay")
	if first.Code != 200 {
		t.Fatalf("first serve: %d", first.Code)
	}
	tracked := htmlutil.ReadHTMLClayID(first.Body.Bytes())
	if !versions.IsCanonicalUUID(tracked) {
		t.Fatalf("the first serve carried no canonical htmlclayid: %q", tracked)
	}

	// An external editor round-tripping the file through a tool that does not
	// understand htmlclayid.
	stripIDOnDisk(t, fx.file.AbsPath)
	before, err := os.ReadFile(fx.file.AbsPath)
	if err != nil {
		t.Fatal(err)
	}

	w := fx.serve(t, "notes.htmlclay")
	if w.Code != 200 {
		t.Fatalf("second serve: %d", w.Code)
	}
	if got := htmlutil.ReadHTMLClayID(w.Body.Bytes()); got != tracked {
		t.Fatalf("the reload served id %q, want the tracked %s", got, tracked)
	}

	onDisk, err := os.ReadFile(fx.file.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := htmlutil.ReadHTMLClayID(onDisk); got != "" {
		t.Fatalf("the reload wrote an id to disk: %q", got)
	}
	if versions.Hash(onDisk) != versions.Hash(before) {
		t.Fatalf("the reload rewrote the file: %q", onDisk)
	}

	sw := fx.save(t, page("after reload"))
	if sw.Code != 200 {
		t.Fatalf("save: %d %s", sw.Code, sw.Body.String())
	}
	if got := decodeJSON(t, sw)["msgType"]; got != "success" {
		t.Fatalf("the save warned %q although the host never wrote the file", got)
	}
}

// An external editor stripping the id AFTER a real save is the case that matters:
// disk genuinely carried the identity and genuinely lost it. The tracked id must
// survive, and it must survive a restart too, because the store still binds it to
// this path (model B′ rule 1). Before this design, a strip meant the next serve
// minted a fresh id and wrote it to disk, orphaning the whole history.
func TestExternalStripOfASavedIDSelfHeals(t *testing.T) {
	fx := setupFileTest(t, "notes.htmlclay", page("original"))

	first := fx.serve(t, "notes.htmlclay")
	if first.Code != 200 {
		t.Fatalf("first serve: %d", first.Code)
	}
	tracked := htmlutil.ReadHTMLClayID(first.Body.Bytes())
	if !versions.IsCanonicalUUID(tracked) {
		t.Fatalf("the first serve carried no canonical htmlclayid: %q", tracked)
	}

	// A real client round trip puts the id on disk, which is the only way it ever
	// gets there.
	if sw := fx.save(t, page("v2")); sw.Code != 200 {
		t.Fatalf("save: %d %s", sw.Code, sw.Body.String())
	}
	saved, _ := os.ReadFile(fx.file.AbsPath)
	if got := htmlutil.ReadHTMLClayID(saved); got != tracked {
		t.Fatalf("the client's save did not land the id on disk: %q", got)
	}

	stripIDOnDisk(t, fx.file.AbsPath)

	// Same process: the session still holds the identity.
	w := fx.serve(t, "notes.htmlclay")
	if w.Code != 200 {
		t.Fatalf("serve after the strip: %d", w.Code)
	}
	if got := htmlutil.ReadHTMLClayID(w.Body.Bytes()); got != tracked {
		t.Fatalf("the serve after a strip carried %q, want the tracked %s", got, tracked)
	}
	afterServe, _ := os.ReadFile(fx.file.AbsPath)
	if got := htmlutil.ReadHTMLClayID(afterServe); got != "" {
		t.Fatalf("serving wrote the id back to disk: %q", got)
	}

	// Across a restart: disk carries no id at all, so only the store's binding of
	// this path can recover the identity.
	next := fx.reopen(t, "notes.htmlclay")
	rw := next.serve(t, "notes.htmlclay")
	if rw.Code != 200 {
		t.Fatalf("serve after reopen: %d", rw.Code)
	}
	if got := htmlutil.ReadHTMLClayID(rw.Body.Bytes()); got != tracked {
		t.Fatalf("the reopened serve minted %q instead of resuming the tracked %s", got, tracked)
	}
	if key := next.key(t); key != "id:"+tracked {
		t.Fatalf("the reopened session tracks %q, want id:%s", key, tracked)
	}

	// The history the strip would have orphaned is still reachable.
	found := false
	for _, e := range next.history(t) {
		if strings.Contains(next.versionBody(t, e.Name), "v2") {
			found = true
		}
	}
	if !found {
		t.Fatal("the saved version was orphaned by the strip")
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
	inc := h.incs["/tmp/a.html"]
	var buffered []retainedFrame
	if inc != nil {
		buffered = inc.bucket(laneLive).frames
	}
	h.mu.Unlock()
	if len(buffered) != 2 {
		t.Fatalf("hub retained %d frames, want 2", len(buffered))
	}
	resumeFrom := buffered[0].seq

	// add returns the frames past the resume point; the writer sends them ahead of
	// the live queue, so replay never touches sub.ch.
	sub := newSubscriber("/tmp/a.html", laneLive)
	sub.lastEventID = resumeFrom
	_, replay := h.add(sub)

	if len(replay) != 1 {
		t.Fatalf("replay returned %d frames, want 1 (the frame after the resume point)", len(replay))
	}
	_, payload := splitFrame(t, replay[0])
	if payload["html"] != "<html>two</html>" {
		t.Fatalf("replay delivered %v, want the frame after the resume point", payload["html"])
	}
	expectNoFrame(t, sub, 100*time.Millisecond)
}

func TestFreshSubscriberIsNotReplayedTo(t *testing.T) {
	h := newHub("")
	h.relay("/tmp/a.html", "<html>old</html>", "c1", nil)

	sub := newSubscriber("/tmp/a.html", laneLive)
	_, replay := h.add(sub)
	if len(replay) != 0 {
		t.Fatalf("a fresh subscriber was replayed %d frames, want 0", len(replay))
	}
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
	_, replay := h.add(next)

	if len(replay) == 0 {
		t.Fatal("reconnecting after an eviction replayed nothing")
	}
	_, payload := splitFrame(t, replay[0])
	if payload["html"] == nil {
		t.Fatal("reconnecting after an eviction replayed a frame with no html")
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
