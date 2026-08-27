package platform

// Anchor identifies one file across time, so a caller can tell whether the file
// sitting at a path is still the file it saw before or a different one that took
// the name. Live-sync needs this: retained frames and resume cursors belong to a
// document, not to a pathname, and serving one document's bytes into a page
// showing another is the failure worth spending code to avoid.
//
// The two operating-system halves are opposites, by necessity rather than taste.
//
// On Unix the anchor HOLDS an open descriptor. Comparing (device, inode) is only
// meaningful while something keeps the inode alive: once the last reference goes,
// the number is free for the next file created, and a recycled inode makes a
// stranger look like the file we anchored. Holding the descriptor costs nothing
// there, because a Unix handle never blocks a rename or an unlink.
//
// On Windows the anchor holds NO descriptor, and must not. Windows refuses to
// rename over a file while any handle to it is open, whatever sharing mode that
// handle asked for (openshared_windows_test.go pins that), so a handle parked for
// bookkeeping makes the file unsaveable by htmlclay and by every other program
// for as long as it is held. Nothing is lost: NTFS answers with a 128-bit file id
// whose low half carries an MFT sequence number that increments when a record is
// reused, so a recycled record produces a different id without anyone holding
// anything.
//
// Deliberately NOT used as identity, and worth recording so it is not retried:
// content hashes (cannot tell "same file, edited" from "different file"), the
// htmlclay id attribute (never written to disk, absent from plain .html, and
// copyable, so cp would alias two documents), and Windows creation time (NTFS
// file system tunnelling caches a deleted file's creation time for 15 seconds and
// reapplies it to a new file of the same name in the same directory, which is
// precisely the atomic-replace window we care about).
type Anchor struct {
	identity
}

// NewAnchor reads the identity of the regular file at path. It touches the disk,
// so build it outside any lock you hold and install it under one. A directory or
// any other non-regular file is an error, not an anchor.
func NewAnchor(path string) (*Anchor, error) {
	id, err := readIdentity(path)
	if err != nil {
		return nil, err
	}
	return &Anchor{identity: id}, nil
}

// Same reports whether both anchors name the same file. A nil anchor is never the
// same as anything, including another nil: an identity nobody established is not
// evidence that nothing changed.
func (a *Anchor) Same(b *Anchor) bool {
	if a == nil || b == nil {
		return false
	}
	return a.identity.same(b.identity)
}

// Close releases whatever the anchor holds, which on Windows is nothing. Safe on
// a nil anchor and safe to call more than once, so callers can drop one without
// first working out whether they have one.
func (a *Anchor) Close() {
	if a == nil {
		return
	}
	a.identity.close()
}
