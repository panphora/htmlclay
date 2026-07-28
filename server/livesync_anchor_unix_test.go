//go:build !windows

package server

import (
	"os"
	"path/filepath"
	"testing"
)

// openDescriptors counts what this process currently holds open. /dev/fd is the
// per-process descriptor directory on macOS and a symlink to /proc/self/fd on
// Linux, so one read covers both.
// Names only: os.ReadDir stats every entry, and on macOS one of them is always
// the descriptor doing the reading, which is gone by then.
func openDescriptors(t *testing.T) int {
	t.Helper()
	dir, err := os.Open("/dev/fd")
	if err != nil {
		t.Skipf("cannot count open descriptors here: %v", err)
	}
	defer dir.Close()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		t.Skipf("cannot count open descriptors here: %v", err)
	}
	return len(names)
}

// Every path through the hub either installs an anchor or closes it. A dropped
// Close leaks a descriptor and pins the inode it names, so the file's disk space
// is never reclaimed for as long as htmlclay runs. Nothing else in the suite can
// see that: on Unix an anchor IS an open descriptor, and it is invisible from the
// outside until the process runs out of them.
//
// Covered here: the anchor displaced by a generation roll, the one displaced by a
// server replacement, the probe taken by a subscriber that arrives at a closed
// hub, both reaper branches, and shutdown. Each round leaks one descriptor per
// broken path, so a real leak is a hundred and the slack below cannot hide it.
func TestTheHubLeavesNoAnchorBehind(t *testing.T) {
	const rounds = 100
	const slack = 8

	dir, _ := filepath.EvalSymlinks(t.TempDir())
	path := filepath.Join(dir, "page.html")
	if err := os.WriteFile(path, []byte("<html>one</html>"), 0644); err != nil {
		t.Fatal(err)
	}

	before := openDescriptors(t)

	for i := 0; i < rounds; i++ {
		h := newHub("")
		sub := newSubscriber(path, laneSaved)
		h.add(sub)
		h.broadcastSaved(path, "<html>one</html>", "file-system")

		writeThroughATempFile(t, path, "<html>two</html>")
		h.acceptServerReplacement(path)

		writeThroughATempFile(t, path, "<html>three</html>")
		h.add(newSubscriber(path, laneSaved))

		h.remove(sub)
		h.shutdown()
		h.add(newSubscriber(path, laneSaved))
	}

	for i := 0; i < rounds; i++ {
		h := newHub("")
		sub := newSubscriber(path, laneSaved)
		h.add(sub)
		h.remove(sub)
		h.mu.Lock()
		h.reapIncarnationsLocked()
		h.mu.Unlock()
		if len(h.incs) != 0 {
			t.Fatal("an idle incarnation was not reaped, so this round proves nothing")
		}
		h.shutdown()
	}

	if leaked := openDescriptors(t) - before; leaked > slack {
		t.Fatalf("the hub leaked %d descriptors across %d rounds: an anchor is being dropped without Close", leaked, rounds)
	}
}
