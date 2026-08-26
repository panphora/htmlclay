package server

import (
	"sync"
	"testing"
	"time"

	"github.com/panphora/htmlclay/internal/testutil"
	"github.com/panphora/htmlclay/logging"
	"github.com/panphora/htmlclay/session"
)

// manualClock is a clockSource whose time moves only when a test moves it. It
// turns the broker's three timing behaviours into things a test states exactly:
// the debounce window, the maxBatch cap on extending that window, and the 130s
// park deadline. Waiting those out in real time is what the old tests did, and a
// 130s deadline is why brokerParkMax had no test at all.
type manualClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*manualTimer
}

type manualTimer struct {
	c        *manualClock
	deadline time.Time
	fn       func()         // set by AfterFunc
	ch       chan time.Time // set by After
	done     bool           // no longer pending: fired, or stopped before it could
	fired    bool           // reached its deadline, as opposed to being stopped
}

// newManualClock starts at a fixed, non-zero instant. Non-zero because code that
// treats a zero time.Time as "unset" would read a zero clock as never having run.
func newManualClock() *manualClock {
	return &manualClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.timers = append(c.timers, &manualTimer{c: c, deadline: c.now.Add(d), ch: ch})
	return ch
}

func (c *manualClock) AfterFunc(d time.Duration, fn func()) clockTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &manualTimer{c: c, deadline: c.now.Add(d), fn: fn}
	c.timers = append(c.timers, t)
	return t
}

// Advance moves the clock forward by d, firing every timer whose deadline it
// passes, in deadline order, with now set to each deadline as that timer fires.
//
// It does not wait for an AfterFunc callback to finish. flush is the only
// callback the broker arms, and flush runs the permission dialog inline, so a
// test whose confirm blocks would deadlock an Advance that waited on it. Tests
// advance and then wait on an observable instead (waitPrompting, waitParked, a
// result channel). Those are lower bounds, so a slow machine delays them and
// cannot fail them.
//
// Advance one window at a time. A single large advance past several deadlines
// races the rearm that a fired callback performs, because the new timer's
// deadline is relative to whatever now had reached by the time the callback got
// to register it.
func (c *manualClock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		var next *manualTimer
		for _, t := range c.timers {
			if t.done || t.deadline.After(target) {
				continue
			}
			if next == nil || t.deadline.Before(next.deadline) {
				next = t
			}
		}
		if next == nil {
			c.now = target
			c.mu.Unlock()
			return
		}
		c.now = next.deadline
		next.done = true
		next.fired = true
		fn, ch, at := next.fn, next.ch, next.deadline
		c.mu.Unlock()

		if ch != nil {
			ch <- at // buffered to one, and each After channel fires once
		}
		if fn != nil {
			go fn()
		}
	}
}

func (t *manualTimer) Reset(d time.Duration) bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	active := !t.done
	t.deadline = t.c.now.Add(d)
	t.done = false
	t.fired = false
	return active
}

func (t *manualTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	active := !t.done
	t.done = true
	return active
}

// manualBroker builds a broker whose clock is stopped. Nothing has been armed at
// this point, so replacing the clock after newBroker cannot orphan a timer.
func manualBroker(t *testing.T, mgr *session.Manager, confirm brokerConfirm) (*broker, *manualClock) {
	t.Helper()
	b := newBroker(mgr, logging.NewStdout(), confirm)
	c := newManualClock()
	b.clock = c
	return b, c
}

// batchTimer returns the broker's armed batch timer and whether it has already
// fired. Advance marks a timer fired synchronously but runs flush on its own
// goroutine, so between those two moments the broker still holds a non-nil timer
// that is spent. Every assertion about an open batch has to distinguish them.
func batchTimer(b *broker, c *manualClock) (armed, fired bool) {
	b.mu.Lock()
	timer := b.timer
	b.mu.Unlock()
	if timer == nil {
		return false, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return true, timer.(*manualTimer).fired
}

// requireBatchOpen asserts the batch window is still open: a timer is armed and it
// has not fired. Inferring this from the waiter count instead would accept a flush
// that has fired and simply not been scheduled yet, which is the manual clock's
// one sharp edge.
func requireBatchOpen(t *testing.T, b *broker, c *manualClock, what string) {
	t.Helper()
	armed, fired := batchTimer(b, c)
	if !armed {
		t.Fatalf("%s: no batch timer is armed", what)
	}
	if fired {
		t.Fatalf("%s: the batch timer has already fired, so the window closed early", what)
	}
}

// requireNoBatchArmed asserts no batch window is open at all.
func requireNoBatchArmed(t *testing.T, b *broker, c *manualClock, what string) {
	t.Helper()
	if armed, _ := batchTimer(b, c); armed {
		t.Fatalf("%s: a batch timer is armed and should not be", what)
	}
}

// waitRearmed blocks until the broker has armed a FRESH batch timer. The rearm
// runs on the flush goroutine once a dialog resolves, so a test that advances the
// clock without waiting for it advances past a timer that does not exist yet and
// then waits out a prompt that will never come. Requiring not-yet-fired is what
// stops a spent timer from being mistaken for the rearm.
func waitRearmed(t *testing.T, b *broker, c *manualClock) {
	t.Helper()
	waitFor(t, 10*time.Second, "a fresh batch timer to be armed", func() bool {
		armed, fired := batchTimer(b, c)
		return armed && !fired
	})
}

// The clock is test infrastructure that several assertions now rest on, so the
// three behaviours the broker takes from it are worth stating directly: Stop
// cancels (shutdown), Reset moves a deadline rather than adding a second timer
// (armLocked extending a batch), and an After channel delivers at its deadline
// (the park deadline in await).
//
// Firing is inspected through the timer's own done flag, which Advance sets
// before it spawns the callback. Watching for the callback's side effect instead
// would assert the order two goroutines happened to run in.
func TestManualClockStopsResetsAndDelivers(t *testing.T) {
	c := newManualClock()

	fired := func(ts ...clockTimer) []bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		out := make([]bool, len(ts))
		for i, x := range ts {
			out[i] = x.(*manualTimer).fired
		}
		return out
	}

	stopped := c.AfterFunc(10*time.Millisecond, func() {})
	moved := c.AfterFunc(10*time.Millisecond, func() {})
	park := c.After(100 * time.Millisecond)

	if !stopped.Stop() {
		t.Error("stopping a pending timer should report it was active")
	}
	if !moved.Reset(50 * time.Millisecond) {
		t.Error("resetting a pending timer should report it was active")
	}

	c.Advance(20 * time.Millisecond)
	if got := fired(stopped, moved); got[0] || got[1] {
		t.Errorf("past the original 10ms deadline: stopped fired=%v, reset fired=%v, want both false", got[0], got[1])
	}

	c.Advance(30 * time.Millisecond)
	if got := fired(stopped, moved); got[0] {
		t.Error("a stopped timer must never fire")
	} else if !got[1] {
		t.Error("a reset timer must fire at its new deadline")
	}

	select {
	case <-park:
		t.Fatal("the park deadline is at 100ms and the clock has only reached 50ms")
	default:
	}

	c.Advance(50 * time.Millisecond)
	at := testutil.Receive(t, 10*time.Second, "the park deadline", park)
	if want := c.Now(); !at.Equal(want) {
		t.Errorf("delivered %v, want the deadline instant %v", at, want)
	}
}
