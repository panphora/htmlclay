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

	"github.com/panphora/htmlclay/internal/logging"
	"github.com/panphora/htmlclay/internal/platform"
	"github.com/panphora/htmlclay/internal/session"
	"github.com/panphora/htmlclay/internal/testutil"
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
	defaultConfirm = func(string, string, bool) (platform.ConfirmChoice, error) {
		return platform.ConfirmDeny, nil
	}
}

func countingConfirm(choice platform.ConfirmChoice, n *int32) brokerConfirm {
	return func(string, string, bool) (platform.ConfirmChoice, error) {
		atomic.AddInt32(n, 1)
		return choice, nil
	}
}

func brokerManager(t *testing.T) (*session.Manager, string) {
	t.Helper()
	home, _ := filepath.EvalSymlinks(t.TempDir())
	return newTestManager(t, home), home
}

// holdBatchOpen stops the debounce timer from closing a batch on its own, so a
// test that means to group N waiters can wait until all N have actually parked
// and then close the batch itself. Racing the real 250ms window instead makes
// the assertion depend on how promptly N goroutines get scheduled: a partial
// flush moves some waiters out of b.waiters and into a prompt, and the count the
// test is waiting for is then never reached.
func holdBatchOpen(b *broker) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.debounce = time.Hour
	b.maxBatch = time.Hour
}

// releaseBatch restores the real batch window after holdBatchOpen. Needed by any
// test whose SECOND act depends on a flush happening on its own: flush rearms the
// timer from b.debounce after the dialog resolves, so a batch still held open
// leaves a post-prompt waiter parked until brokerParkMax, and the test passes
// 130 seconds later having proved nothing about the debounce path.
func releaseBatch(b *broker) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.debounce = brokerDebounce
	b.maxBatch = brokerMaxBatch
}

// waitPrompting blocks until the broker has a dialog up. Pair it with
// holdBatchOpen and an explicit `go b.flush()`: waiting on the real debounce
// timer instead makes the test depend on a 250ms timer callback being scheduled,
// which is the first thing a saturated machine stops doing promptly.
func waitPrompting(t *testing.T, b *broker) {
	t.Helper()
	waitFor(t, 10*time.Second, "a prompt to be up", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.prompting
	})
}

// waitParked blocks until exactly n waiters are parked.
func waitParked(t *testing.T, b *broker, n int) {
	t.Helper()
	waitFor(t, 10*time.Second, "parked waiters", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.waiters) == n
	})
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
	holdBatchOpen(b)

	var wg sync.WaitGroup
	results := make([]bool, len(paths))
	for i, p := range paths {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			results[i] = b.await(context.Background(), p)
		}(i, p)
	}
	// All three are parked before the batch closes, which is the arrival the
	// grouping is about. Whether they get there inside 250ms is not.
	waitParked(t, b, len(paths))
	b.flush()
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
	b := newBroker(mgr, logging.NewStdout(), countingConfirm(platform.ConfirmTrustFolder, &prompts))

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
	b := newBroker(mgr, logging.NewStdout(), failIfPrompted(t))
	// Holding the batch open is what puts the waiter in the state this is about:
	// parked, with no prompt raised. Cancelling before it has parked exercises
	// nothing, and sleeping to avoid that only trades one guess for another.
	holdBatchOpen(b)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- b.await(ctx, filepath.Join(home, "x", "y.js")) }()
	waitParked(t, b, 1)

	cancel()
	if testutil.Receive(t, 10*time.Second, "a canceled request to return", done) {
		t.Error("canceled request must not be allowed")
	}
	// The canceled waiter left with it, so it cannot surface in a later batch.
	b.mu.Lock()
	left := len(b.waiters)
	b.mu.Unlock()
	if left != 0 {
		t.Errorf("a canceled request left %d waiter(s) behind", left)
	}
}

// failIfPrompted is a confirm for tests that must never reach a prompt at all.
func failIfPrompted(t *testing.T) brokerConfirm {
	return func(string, string, bool) (platform.ConfirmChoice, error) {
		t.Error("no prompt should have been raised")
		return platform.ConfirmDeny, nil
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
	b := newBroker(mgr, logging.NewStdout(), countingConfirm(platform.ConfirmTrustFolder, &prompts))

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
	b := newBroker(mgr, logging.NewStdout(), func(string, string, bool) (platform.ConfirmChoice, error) {
		<-gate
		return platform.ConfirmDeny, nil
	})

	holdBatchOpen(b)

	const n = 5
	done := make(chan bool, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			done <- b.await(context.Background(), filepath.Join(home, "vendor", fmt.Sprintf("a%d.js", i)))
		}(i)
	}
	waitParked(t, b, n)

	b.shutdown()

	for i := 0; i < n; i++ {
		if testutil.Receive(t, 10*time.Second, "shutdown to release a parked waiter", done) {
			t.Error("shutdown must release parked waiters as denied")
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
	b := newBroker(mgr, logging.NewStdout(), func(string, string, bool) (platform.ConfirmChoice, error) {
		<-gate
		return platform.ConfirmDeny, nil
	})
	defer b.shutdown()

	holdBatchOpen(b)

	done := make(chan bool, brokerParkCap+1)
	for i := 0; i < brokerParkCap; i++ {
		go func(i int) {
			done <- b.await(context.Background(), filepath.Join(home, "vendor", fmt.Sprintf("a%d.js", i)))
		}(i)
	}
	waitParked(t, b, brokerParkCap)

	// The overflow request is denied immediately, without blocking.
	overflow := make(chan bool, 1)
	go func() { overflow <- b.await(context.Background(), filepath.Join(home, "vendor", "overflow.js")) }()
	if testutil.Receive(t, 10*time.Second, "an over-cap request to be denied without parking", overflow) {
		t.Error("a request over the park cap must be denied")
	}
}

// A waiter that parks WHILE a tree's dialog is up must be denied when that tree is
// denied, without a second prompt (the suppression-during-prompt fix).
func TestBrokerSuppressionDuringPromptNoReprompt(t *testing.T) {
	mgr, home := brokerManager(t)
	var prompts int32
	gate := make(chan struct{})
	b := newBroker(mgr, logging.NewStdout(), func(string, string, bool) (platform.ConfirmChoice, error) {
		atomic.AddInt32(&prompts, 1)
		<-gate
		return platform.ConfirmDeny, nil
	})

	holdBatchOpen(b)
	first := make(chan bool, 1)
	go func() { first <- b.await(context.Background(), filepath.Join(home, "vendor", "a.js")) }()
	waitParked(t, b, 1)
	go b.flush()
	waitFor(t, 10*time.Second, "the first prompt to be raised", func() bool {
		return atomic.LoadInt32(&prompts) == 1
	})

	// armLocked is a no-op while prompting, so this waiter stays parked until the
	// dialog resolves. The count is stable once reached rather than a moment the
	// poll has to catch.
	second := make(chan bool, 1)
	go func() { second <- b.await(context.Background(), filepath.Join(home, "vendor", "b.js")) }()
	waitParked(t, b, 1)
	// The second waiter is denied by the flush that rearms after this dialog, so
	// that rearm has to use the real window and not the held-open one.
	releaseBatch(b)

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
	b := newBroker(mgr, logging.NewStdout(), func(string, string, bool) (platform.ConfirmChoice, error) {
		<-gate
		return platform.ConfirmAllowOnce, nil
	})

	holdBatchOpen(b)
	first := make(chan bool, 1)
	go func() { first <- b.await(context.Background(), filepath.Join(proj, "x.js")) }()
	waitParked(t, b, 1)
	go b.flush()
	waitPrompting(t, b)

	// Park a second waiter under a subfolder the pending grant (home/proj) will cover.
	second := make(chan bool, 1)
	go func() { second <- b.await(context.Background(), filepath.Join(proj, "sub", "y.js")) }()
	waitParked(t, b, 1)

	close(gate) // Allow → grant home/proj, which covers both waiters

	if !<-first {
		t.Error("the prompted waiter should be allowed")
	}
	if !<-second {
		t.Error("a waiter parked during the prompt, covered by the grant, should be woken")
	}
}

// The trust-folder choice must do both halves: install the session read root so the
// current request resumes, and hand the granted folder to the trust hook so future
// opens from inside it stop asking.
func TestTrustFolderChoiceGrantsAndTrusts(t *testing.T) {
	mgr, home := brokerManager(t)
	asset := filepath.Join(home, "proj", "app", "x.js")
	mustWrite(t, asset)

	var prompts int32
	b := newBroker(mgr, logging.NewStdout(), countingConfirm(platform.ConfirmTrustFolder, &prompts))
	b.mayTrust = func(string) bool { return true }
	// The waiter is woken before the trust hook runs, so the trusted folder arrives
	// on its own goroutine and is collected through a channel.
	trusted := make(chan string, 4)
	b.trust = func(dir string) error {
		trusted <- dir
		return nil
	}

	if !b.await(context.Background(), asset) {
		t.Fatal("the trust-folder choice must allow the read")
	}
	root, _, ok := mgr.AssetRoot(asset)
	if !ok {
		t.Fatal("the trust-folder choice must install the read root")
	}
	dir := testutil.Receive(t, 10*time.Second, "the trust-folder choice to hand the folder to the trust hook", trusted)
	if dir != root {
		t.Errorf("trust must be handed the granted folder: got %q, want %q", dir, root)
	}
	if extra := len(trusted); extra != 0 {
		t.Errorf("expected exactly one trust call, got %d extra", extra)
	}
}

// Trust is the second step, never a replacement for the grant: a trust hook that
// fails still leaves the waiter resumed and the read root installed, so the page
// works and the folder simply asks again next launch.
func TestTrustFolderChoiceStillGrantsWhenTrustFails(t *testing.T) {
	mgr, home := brokerManager(t)
	asset := filepath.Join(home, "proj", "app", "x.js")
	mustWrite(t, asset)

	var prompts int32
	b := newBroker(mgr, logging.NewStdout(), countingConfirm(platform.ConfirmTrustFolder, &prompts))
	b.mayTrust = func(string) bool { return true }
	b.trust = func(string) error { return fmt.Errorf("refused") }

	if !b.await(context.Background(), asset) {
		t.Fatal("a failed trust must not deny the waiter")
	}
	if _, _, ok := mgr.AssetRoot(asset); !ok {
		t.Error("a failed trust must not undo the read root")
	}
}

// Allow Once is the session-only choice: it grants without touching the trusted list.
func TestAllowOnceDoesNotTrust(t *testing.T) {
	mgr, home := brokerManager(t)
	asset := filepath.Join(home, "proj", "app", "x.js")
	mustWrite(t, asset)

	var prompts int32
	b := newBroker(mgr, logging.NewStdout(), countingConfirm(platform.ConfirmAllowOnce, &prompts))
	// The durable choice is offered, so the refusal under test is the answer
	// itself rather than a suppressed button.
	b.mayTrust = func(string) bool { return true }
	var trustCalls int32
	b.trust = func(string) error {
		atomic.AddInt32(&trustCalls, 1)
		return nil
	}

	if !b.await(context.Background(), asset) {
		t.Fatal("allow-once must allow the read")
	}
	if got := atomic.LoadInt32(&trustCalls); got != 0 {
		t.Errorf("allow-once must never trust the folder: got %d trust calls", got)
	}
}

// The durable choice is decided BEFORE the dialog is drawn: a folder mayTrust
// refuses is never offered, and an answer of ConfirmTrustFolder from a dialog
// that could not have offered it grants the read without recording anything.
// Drawing the button always and refusing afterwards asked the user for a choice
// that could not be honored.
func TestMayTrustFalseSuppressesTheChoiceAndTheTrust(t *testing.T) {
	mgr, home := brokerManager(t)
	asset := filepath.Join(home, "Downloads", "sketchy", "x.js")
	mustWrite(t, asset)

	var offered atomic.Bool
	offered.Store(true)
	b := newBroker(mgr, logging.NewStdout(), func(_, _ string, allowTrust bool) (platform.ConfirmChoice, error) {
		offered.Store(allowTrust)
		return platform.ConfirmTrustFolder, nil
	})
	var mayTrustCalls, trustCalls int32
	b.mayTrust = func(string) bool {
		atomic.AddInt32(&mayTrustCalls, 1)
		return false
	}
	b.trust = func(string) error {
		atomic.AddInt32(&trustCalls, 1)
		return nil
	}

	if !b.await(context.Background(), asset) {
		t.Fatal("a refused durable choice must still allow the read")
	}
	if atomic.LoadInt32(&mayTrustCalls) == 0 {
		t.Error("the durable choice must be gated by mayTrust before the dialog")
	}
	if offered.Load() {
		t.Error("the dialog was offered the trust choice for a folder mayTrust refused")
	}
	if got := atomic.LoadInt32(&trustCalls); got != 0 {
		t.Errorf("a trust answer from a dialog that could not offer it must record nothing: got %d trust calls", got)
	}
	if _, _, ok := mgr.AssetRoot(asset); !ok {
		t.Error("the read root must still be installed")
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

// A waiter queued behind ANOTHER tree's dialog self-resolves to deny once it has
// been parked for brokerParkMax. That is the case the constant's own comment
// calls out as not covered by the batch-plus-dialog budget, and until the broker
// took a clock it could not be tested at all: the deadline is 130 seconds.
func TestBrokerParkExpiresWhileAnotherTreesDialogIsUp(t *testing.T) {
	mgr, home := brokerManager(t)
	first := filepath.Join(home, "one", "a.js")
	second := filepath.Join(home, "two", "b.js")
	mustWrite(t, first)
	mustWrite(t, second)

	gate := make(chan struct{})
	// Signalled from inside confirm, not from b.prompting. flush sets that flag and
	// then does real work — a symlink resolution among it — before calling confirm,
	// so waiting on the flag returns while the prompt counter this test asserts on
	// is still zero.
	entered := make(chan struct{}, 4)
	var prompts int32
	b, clock := manualBroker(t, mgr, func(string, string, bool) (platform.ConfirmChoice, error) {
		atomic.AddInt32(&prompts, 1)
		entered <- struct{}{}
		<-gate
		return platform.ConfirmDeny, nil
	})

	one := make(chan bool, 1)
	go func() { one <- b.await(context.Background(), first) }()
	waitParked(t, b, 1)
	clock.Advance(brokerDebounce)
	testutil.Receive(t, 10*time.Second, "the first tree's dialog to be raised", entered)

	// Parks under a different tree, so the dialog on screen is not its own and
	// prompts are serialized behind it.
	two := make(chan bool, 1)
	go func() { two <- b.await(context.Background(), second) }()
	waitParked(t, b, 1)

	// Just short of the deadline first. Advancing straight to it would accept any
	// shorter deadline as correct, so this half is what pins the duration rather
	// than merely the existence of an expiry.
	clock.Advance(brokerParkMax - time.Second)
	select {
	case <-two:
		t.Fatal("the waiter expired before brokerParkMax")
	case <-time.After(50 * time.Millisecond):
	}

	clock.Advance(time.Second)
	if testutil.Receive(t, 10*time.Second, "the queued waiter to give up", two) {
		t.Error("a waiter held past brokerParkMax must resolve to deny")
	}
	if got := atomic.LoadInt32(&prompts); got != 1 {
		t.Errorf("the expired waiter must never raise its own dialog, got %d prompts", got)
	}
	// Expiring is only half of it: a waiter that returns deny but stays in the list
	// is still queued to be prompted for later, which is the opposite of expired.
	b.mu.Lock()
	left := len(b.waiters)
	b.mu.Unlock()
	if left != 0 {
		t.Errorf("the expired waiter is still parked: %d waiters remain", left)
	}

	close(gate)
	testutil.Receive(t, 10*time.Second, "the prompted waiter to drain", one)
}

// The batch window extends while requests keep arriving, and maxBatch caps when
// the LAST extension may start, not when the flush lands. A reset granted just
// under maxBatch still runs a full debounce after it, so the real bound is close
// to maxBatch+debounce. Both halves are asserted here so neither can be tightened
// or dropped without a red test.
func TestBrokerBatchExtendsPastMaxBatchByOneDebounce(t *testing.T) {
	mgr, home := brokerManager(t)
	var prompts int32
	b, clock := manualBroker(t, mgr, countingConfirm(platform.ConfirmDeny, &prompts))

	results := make(chan bool, 16)
	park := func(name string) {
		p := filepath.Join(home, "proj", name)
		mustWrite(t, p)
		go func() { results <- b.await(context.Background(), p) }()
	}

	// Each arrival lands just inside the open window, so each one extends it.
	step := brokerDebounce - 10*time.Millisecond
	park("first.js")
	waitParked(t, b, 1)

	parked, elapsed := 1, time.Duration(0)
	for elapsed < brokerMaxBatch {
		clock.Advance(step)
		elapsed += step
		park(fmt.Sprintf("%d.js", parked))
		parked++
		waitParked(t, b, parked)
		// Read off the timer, not the waiter count: without this, deleting the
		// Reset in armLocked still passes whenever the fired flush is slow enough
		// to be scheduled after the last arrival parked.
		requireBatchOpen(t, b, clock, fmt.Sprintf("after the arrival at %v", elapsed))
	}

	if elapsed <= brokerMaxBatch {
		t.Fatalf("the batch must be carried past maxBatch, stopped at %v", elapsed)
	}
	if got := atomic.LoadInt32(&prompts); got != 0 {
		t.Fatalf("at %v the window was still open, but %d prompts had already gone up", elapsed, got)
	}

	// The extension granted just under the cap still owes a full debounce.
	clock.Advance(brokerDebounce - step)
	for i := 0; i < parked; i++ {
		if testutil.Receive(t, 10*time.Second, "every waiter in the batch to resolve", results) {
			t.Error("a denied batch must deny every waiter it grouped")
		}
	}
	if got := atomic.LoadInt32(&prompts); got != 1 {
		t.Errorf("the whole batch must close as one prompt, got %d", got)
	}
}

// Arming is skipped while a dialog is up, so a waiter that parks under a second
// tree has no window of its own. Only the rearm on the first dialog's completion
// can open one, and this is the test that fails if that rearm goes away: no
// amount of clock movement raises the second prompt without it.
func TestBrokerRearmsTheBatchAfterADialogCloses(t *testing.T) {
	mgr, home := brokerManager(t)
	one := filepath.Join(home, "one", "a.js")
	two := filepath.Join(home, "two", "b.js")
	mustWrite(t, one)
	mustWrite(t, two)

	gate := make(chan struct{})
	// As above: entering confirm is the event, not the prompting flag, which is set
	// well before it and would let the count assertion below read a stale zero.
	entered := make(chan struct{}, 4)
	var prompts int32
	b, clock := manualBroker(t, mgr, func(string, string, bool) (platform.ConfirmChoice, error) {
		if atomic.AddInt32(&prompts, 1) == 1 {
			entered <- struct{}{}
			<-gate
			return platform.ConfirmDeny, nil
		}
		entered <- struct{}{}
		return platform.ConfirmDeny, nil
	})

	first := make(chan bool, 1)
	go func() { first <- b.await(context.Background(), one) }()
	waitParked(t, b, 1)
	clock.Advance(brokerDebounce)
	testutil.Receive(t, 10*time.Second, "the first tree's dialog to be raised", entered)

	second := make(chan bool, 1)
	go func() { second <- b.await(context.Background(), two) }()
	waitParked(t, b, 1)

	// The claim in this test's name rests on this: armLocked returns early while a
	// dialog is up, so the second tree has no window of its own to close.
	requireNoBatchArmed(t, b, clock, "while the first tree's dialog is up")

	clock.Advance(4 * brokerDebounce)
	if got := atomic.LoadInt32(&prompts); got != 1 {
		t.Fatalf("no second dialog may open while the first is up, got %d prompts", got)
	}

	close(gate)
	if testutil.Receive(t, 10*time.Second, "the prompted waiter", first) {
		t.Error("a denied tree must deny its own waiter")
	}

	waitRearmed(t, b, clock)
	clock.Advance(brokerDebounce)
	if testutil.Receive(t, 10*time.Second, "the waiter that parked during the dialog", second) {
		t.Error("the second tree was denied too, so its waiter must be denied")
	}
	if got := atomic.LoadInt32(&prompts); got != 2 {
		t.Errorf("the second tree needs a dialog of its own, got %d prompts", got)
	}
}
