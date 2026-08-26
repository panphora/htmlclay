// Package testutil holds the two waits every package's tests need.
//
// It is a real package rather than a _test.go helper because the server, root
// and tray packages all need these and Go does not share test helpers across
// package boundaries. Copying them instead would give the repository three
// timeout policies and three failure messages. No production file imports this,
// so it is compiled only into test binaries.
//
// Both helpers take a duration, and in both it is a hang guard rather than part
// of any assertion: the condition or the received value is what the caller is
// testing. A generous deadline therefore costs nothing except on a run that was
// already going to fail, which is why these are deliberately not scaled by an
// environment variable. A test that needs elapsed time to reach the state it
// asserts is a test to restructure, not one to give a bigger number.
package testutil

import (
	"testing"
	"time"
)

// Lazy defers building a failure message until there is a failure to describe.
// Several waits here want to print the state they gave up on, and that state is
// empty at the moment the wait starts, so an eagerly built string would report
// nothing useful. Pass one as Eventually's what.
type Lazy func() string

func (l Lazy) String() string { return l() }

// Eventually blocks until cond returns true, failing with what if it never does.
// what is usually a string; pass a Lazy to build the message from state that
// only exists once the wait has failed.
func Eventually(t testing.TB, within time.Duration, what any, cond func() bool) {
	t.Helper()
	if cond() {
		return
	}
	deadline := time.NewTimer(within)
	defer deadline.Stop()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case <-poll.C:
			if cond() {
				return
			}
		case <-deadline.C:
			// A tick and the deadline can be ready together, and select picks
			// between ready cases at random, so check once more rather than fail
			// a condition that became true on the boundary.
			if cond() {
				return
			}
			t.Fatalf("timed out after %v waiting for %v", within, what)
			return
		}
	}
}

// Receive returns the next value from ch, failing with what if none arrives. A
// closed channel counts as a receive, because several callers here signal by
// closing rather than by sending.
func Receive[T any](t testing.TB, within time.Duration, what any, ch <-chan T) T {
	t.Helper()
	deadline := time.NewTimer(within)
	defer deadline.Stop()
	select {
	case v := <-ch:
		return v
	case <-deadline.C:
		t.Fatalf("timed out after %v waiting for %v", within, what)
		var zero T
		return zero
	}
}
