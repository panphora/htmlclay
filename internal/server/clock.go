package server

import "time"

// clockSource is the broker's view of time. Production passes realClock, whose
// three methods are the time package functions themselves. A test passes a
// manual clock, which turns the debounce window, the maxBatch cap and the 130s
// park deadline into things a test steps through rather than waits out.
//
// Only the broker takes one. The watcher's tests drive its poll loop by calling
// check directly, so a clock there would swap one explicit mechanism for another
// and buy nothing.
type clockSource interface {
	Now() time.Time
	// After backs the park deadline in await. The returned channel receives once
	// the clock has passed d.
	After(d time.Duration) <-chan time.Time
	// AfterFunc backs the batch timer. fn runs on its own goroutine, as
	// time.AfterFunc's does, because flush takes the broker lock and the code
	// that armed the timer is usually still holding it.
	AfterFunc(d time.Duration, fn func()) clockTimer
}

// clockTimer is the part of *time.Timer the broker uses.
type clockTimer interface {
	Reset(d time.Duration) bool
	Stop() bool
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

func (realClock) AfterFunc(d time.Duration, fn func()) clockTimer { return time.AfterFunc(d, fn) }
