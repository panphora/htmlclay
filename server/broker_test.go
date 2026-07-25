package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/panphora/htmlclay/logging"
	"github.com/panphora/htmlclay/platform"
	"github.com/panphora/htmlclay/session"
)

func mustWrite(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
}

// No test may pop a real permission dialog. Every server built in this package's
// tests starts from defaultConfirm, so pinning it to deny here keeps osascript
// off the screen even for tests that never touch the broker directly.
func init() {
	defaultConfirm = func(string, string) (platform.ConfirmChoice, error) {
		return platform.ConfirmDeny, nil
	}
}

func countingConfirm(choice platform.ConfirmChoice, n *int32) brokerConfirm {
	return func(string, string) (platform.ConfirmChoice, error) {
		atomic.AddInt32(n, 1)
		return choice, nil
	}
}

func brokerManager(t *testing.T) (*session.Manager, string) {
	t.Helper()
	home, _ := filepath.EvalSymlinks(t.TempDir())
	return session.NewManagerWithHome(home), home
}

// Many out-of-scope requests under one parent, arriving together, must produce a
// single prompt for their common ancestor, and all must succeed after Allow.
func TestBrokerBatchesToOnePrompt(t *testing.T) {
	mgr, home := brokerManager(t)
	var prompts int32
	b := newBroker(mgr, logging.NewStdout(), countingConfirm(platform.ConfirmAllowOnce, &prompts))

	paths := []string{
		filepath.Join(home, "review", "redpen.js"),
		filepath.Join(home, "review", "fonts", "a.woff"),
		filepath.Join(home, "review", "img", "logo.png"),
	}
	// The grant resolves and opens the common ancestor, so it must exist on disk.
	for _, p := range paths {
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	results := make([]bool, len(paths))
	for i, p := range paths {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			results[i] = b.await(context.Background(), p)
		}(i, p)
	}
	wg.Wait()

	for i, ok := range results {
		if !ok {
			t.Errorf("waiter %d should have been allowed", i)
		}
	}
	if got := atomic.LoadInt32(&prompts); got != 1 {
		t.Errorf("expected exactly one prompt for the common ancestor, got %d", got)
	}
	if _, _, ok := mgr.AssetRoot(filepath.Join(home, "review", "redpen.js")); !ok {
		t.Error("the common ancestor should have been granted")
	}
}

// A denied root suppresses re-asking: later requests under it are denied without
// another prompt.
func TestBrokerSuppressesAfterDeny(t *testing.T) {
	mgr, home := brokerManager(t)
	var prompts int32
	b := newBroker(mgr, logging.NewStdout(), countingConfirm(platform.ConfirmDeny, &prompts))

	if b.await(context.Background(), filepath.Join(home, "vendor", "a.js")) {
		t.Fatal("first request should be denied")
	}
	if b.await(context.Background(), filepath.Join(home, "vendor", "b.js")) {
		t.Fatal("second request under the denied root should be denied")
	}
	if got := atomic.LoadInt32(&prompts); got != 1 {
		t.Errorf("a suppressed root must not prompt again: got %d prompts", got)
	}
}

// A request whose only grantable ancestor is the home directory is denied with no
// prompt at all.
func TestBrokerNeverPromptsForHome(t *testing.T) {
	mgr, home := brokerManager(t)
	var prompts int32
	b := newBroker(mgr, logging.NewStdout(), countingConfirm(platform.ConfirmAllowAlways, &prompts))

	if b.await(context.Background(), filepath.Join(home, "loose.txt")) {
		t.Error("a home-root sibling must be denied")
	}
	if got := atomic.LoadInt32(&prompts); got != 0 {
		t.Errorf("home must never be offered as a grant: got %d prompts", got)
	}
}

// A canceled request stops waiting and does not leak into the next batch.
func TestBrokerContextCancel(t *testing.T) {
	mgr, home := brokerManager(t)
	// A confirm that blocks forever would hold the prompt; use a slow deny so the
	// waiter is parked when we cancel.
	b := newBroker(mgr, logging.NewStdout(), func(string, string) (platform.ConfirmChoice, error) {
		time.Sleep(2 * time.Second)
		return platform.ConfirmDeny, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- b.await(ctx, filepath.Join(home, "x", "y.js")) }()
	cancel()
	select {
	case allow := <-done:
		if allow {
			t.Error("canceled request must not be allowed")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled request should return promptly")
	}
}

func TestCommonDir(t *testing.T) {
	sep := string(filepath.Separator)
	root := sep + "home" + sep + "u"
	cases := []struct {
		dirs []string
		want string
	}{
		{[]string{root + sep + "a"}, root + sep + "a"},
		{[]string{root + sep + "a", root + sep + "b"}, root},
		{[]string{root + sep + "a" + sep + "x", root + sep + "a" + sep + "y"}, root + sep + "a"},
		{[]string{root + sep + "ab", root + sep + "ac"}, root}, // shared prefix, not a boundary
	}
	for _, c := range cases {
		if got := commonDir(c.dirs); got != c.want {
			t.Errorf("commonDir(%v) = %q, want %q", c.dirs, got, c.want)
		}
	}
}

func TestTopSegment(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	if got := topSegment(home, filepath.Join(home, "review", "a.js")); got != "review" {
		t.Errorf("topSegment = %q, want review", got)
	}
	if got := topSegment(home, filepath.Join(home, "loose.js")); got != "loose.js" {
		t.Errorf("topSegment = %q, want loose.js", got)
	}
	if got := topSegment(home, "/somewhere/else"); got != "" {
		t.Errorf("topSegment outside home = %q, want empty", got)
	}
}

// The broker must never prompt for a folder the guard vetoes (the config/versions
// tree); otherwise the user gets an Allow that silently fails inside GrantReadRoot.
func TestBrokerGuardVetoNeverPrompts(t *testing.T) {
	mgr, home := brokerManager(t)
	forbidden := filepath.Join(home, "Library", "Application Support", "htmlclay")
	mgr.SetGuard(func(dir string) bool {
		return session.EqualOrUnder(dir, forbidden) || session.EqualOrUnder(forbidden, dir)
	})
	var prompts int32
	b := newBroker(mgr, logging.NewStdout(), countingConfirm(platform.ConfirmAllowAlways, &prompts))

	// The asset's own folder is the guarded config dir, so the LCA is guard-vetoed and
	// the broker must deny without ever prompting.
	if b.await(context.Background(), filepath.Join(forbidden, "config.json")) {
		t.Error("a guard-vetoed folder must be denied")
	}
	if got := atomic.LoadInt32(&prompts); got != 0 {
		t.Errorf("the broker must never prompt for a guard-vetoed folder: got %d prompts", got)
	}
}

// shutdown must release every parked waiter as denied so in-flight requests drain,
// and refuse further parking.
func TestBrokerShutdownReleasesParkedWaiters(t *testing.T) {
	mgr, home := brokerManager(t)
	gate := make(chan struct{})
	defer close(gate)
	b := newBroker(mgr, logging.NewStdout(), func(string, string) (platform.ConfirmChoice, error) {
		<-gate
		return platform.ConfirmDeny, nil
	})

	const n = 5
	done := make(chan bool, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			done <- b.await(context.Background(), filepath.Join(home, "vendor", fmt.Sprintf("a%d.js", i)))
		}(i)
	}
	waitFor(t, 2*time.Second, "broker state", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.waiters) == n
	})

	b.shutdown()

	for i := 0; i < n; i++ {
		select {
		case allow := <-done:
			if allow {
				t.Error("shutdown must release parked waiters as denied")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("shutdown did not release a parked waiter")
		}
	}
	if b.await(context.Background(), filepath.Join(home, "vendor", "later.js")) {
		t.Error("await after shutdown must return false")
	}
}

// Above the park cap, new requests are denied rather than growing memory unbounded.
func TestBrokerParkCapDeniesOverflow(t *testing.T) {
	mgr, home := brokerManager(t)
	gate := make(chan struct{})
	defer close(gate)
	b := newBroker(mgr, logging.NewStdout(), func(string, string) (platform.ConfirmChoice, error) {
		<-gate
		return platform.ConfirmDeny, nil
	})
	defer b.shutdown()

	done := make(chan bool, brokerParkCap+1)
	for i := 0; i < brokerParkCap; i++ {
		go func(i int) {
			done <- b.await(context.Background(), filepath.Join(home, "vendor", fmt.Sprintf("a%d.js", i)))
		}(i)
	}
	waitFor(t, 2*time.Second, "broker state", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.waiters) == brokerParkCap
	})

	// The overflow request is denied immediately, without blocking.
	overflow := make(chan bool, 1)
	go func() { overflow <- b.await(context.Background(), filepath.Join(home, "vendor", "overflow.js")) }()
	select {
	case allow := <-overflow:
		if allow {
			t.Error("a request over the park cap must be denied")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a request over the park cap must be denied immediately, not parked")
	}
}

// A waiter that parks WHILE a tree's dialog is up must be denied when that tree is
// denied, without a second prompt (the suppression-during-prompt fix).
func TestBrokerSuppressionDuringPromptNoReprompt(t *testing.T) {
	mgr, home := brokerManager(t)
	var prompts int32
	gate := make(chan struct{})
	b := newBroker(mgr, logging.NewStdout(), func(string, string) (platform.ConfirmChoice, error) {
		atomic.AddInt32(&prompts, 1)
		<-gate
		return platform.ConfirmDeny, nil
	})

	first := make(chan bool, 1)
	go func() { first <- b.await(context.Background(), filepath.Join(home, "vendor", "a.js")) }()
	waitFor(t, 2*time.Second, "broker state", func() bool { return atomic.LoadInt32(&prompts) == 1 })

	second := make(chan bool, 1)
	go func() { second <- b.await(context.Background(), filepath.Join(home, "vendor", "b.js")) }()
	waitFor(t, 2*time.Second, "broker state", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.waiters) == 1
	})

	close(gate) // resolve the first prompt: Deny → suppress the vendor tree

	if <-first {
		t.Error("the prompted waiter should be denied")
	}
	if <-second {
		t.Error("a waiter parked during the dialog for a now-denied tree should be denied")
	}
	if got := atomic.LoadInt32(&prompts); got != 1 {
		t.Errorf("a tree denied while its dialog was up must not re-prompt: got %d prompts", got)
	}
}

// A waiter that parks during a prompt, under a path the resulting grant covers, is
// woken by decide's freed-scan rather than left hanging.
func TestBrokerWakesWaiterParkedDuringPrompt(t *testing.T) {
	mgr, home := brokerManager(t)
	proj := filepath.Join(home, "proj")
	mustWrite(t, filepath.Join(proj, "x.js"))
	mustWrite(t, filepath.Join(proj, "sub", "y.js"))

	gate := make(chan struct{})
	b := newBroker(mgr, logging.NewStdout(), func(string, string) (platform.ConfirmChoice, error) {
		<-gate
		return platform.ConfirmAllowOnce, nil
	})

	first := make(chan bool, 1)
	go func() { first <- b.await(context.Background(), filepath.Join(proj, "x.js")) }()
	waitFor(t, 2*time.Second, "broker state", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.prompting
	})

	// Park a second waiter under a subfolder the pending grant (home/proj) will cover.
	second := make(chan bool, 1)
	go func() { second <- b.await(context.Background(), filepath.Join(proj, "sub", "y.js")) }()
	waitFor(t, 2*time.Second, "broker state", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.waiters) == 1
	})

	close(gate) // Allow → grant home/proj, which covers both waiters

	if !<-first {
		t.Error("the prompted waiter should be allowed")
	}
	if !<-second {
		t.Error("a waiter parked during the prompt, covered by the grant, should be woken")
	}
}

// A grant that fails after Allow (here: the LCA no longer resolves) denies the group
// and suppresses the root so it is not re-prompted.
func TestBrokerGrantFailureDeniesAndSuppresses(t *testing.T) {
	mgr, home := brokerManager(t)
	var prompts int32
	b := newBroker(mgr, logging.NewStdout(), countingConfirm(platform.ConfirmAllowOnce, &prompts))

	// home/ghost never exists on disk, so GrantReadRoot's EvalSymlinks fails even
	// though CanGrant (string checks only) passes.
	if b.await(context.Background(), filepath.Join(home, "ghost", "a.js")) {
		t.Error("a waiter whose grant fails must be denied")
	}
	if b.await(context.Background(), filepath.Join(home, "ghost", "b.js")) {
		t.Error("a second request under the failed root should be denied without a new prompt")
	}
	if got := atomic.LoadInt32(&prompts); got != 1 {
		t.Errorf("a failed grant should suppress the root, not re-prompt: got %d prompts", got)
	}
}
