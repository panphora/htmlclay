package server

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/panphora/htmlclay/logging"
	"github.com/panphora/htmlclay/platform"
	"github.com/panphora/htmlclay/session"
)

const (
	// A page's out-of-scope subresource requests arrive in a burst; debounce so
	// they collect into one prompt. The hard cap bounds how long the first
	// waiter is held while later ones keep resetting the idle timer.
	brokerDebounce = 250 * time.Millisecond
	brokerMaxBatch = 750 * time.Millisecond
	// A parked request is held this long before it self-resolves to deny. It covers
	// the batch window plus one full 120s dialog, so a waiter whose prompt is raised
	// promptly always outlives it. It does NOT cover a waiter queued behind another
	// tree's dialog: prompts are serialized, so that waiter can still expire partway
	// through its own prompt, and a late Allow then installs a grant with no request
	// left to resume. That grant is harmless and revocable from the tray. Fixing it
	// properly means starting the deadline when the waiter's prompt is raised rather
	// than when it parks, which is deferred.
	brokerParkMax = 130 * time.Second
	// Above this many simultaneously parked requests, deny rather than grow
	// unbounded. A hostile page cannot pin memory by firing thousands of misses.
	brokerParkCap = 64
)

type brokerConfirm func(title, message string) (platform.ConfirmChoice, error)

type trustFunc func(dir string) error

// defaultConfirm is the confirm every new broker starts with. Production leaves
// it as the real native dialog; the server test binary overrides it in an init()
// so no test ever pops an actual dialog on the user's screen.
var defaultConfirm brokerConfirm = platform.Confirm

type parkWaiter struct {
	path string
	dir  string
	ch   chan bool // buffered(1): a resolve never blocks on a gone waiter
}

// broker turns an out-of-scope asset request into a one-shot native permission
// prompt, holding the request open until the user answers so the browser gets a
// slow success instead of a reload. One broker per site.
type broker struct {
	mu       sync.Mutex
	sessions *session.Manager
	logger   *logging.Logger
	confirm  brokerConfirm
	trust    trustFunc
	home     string
	label    string

	waiters    []*parkWaiter
	suppressed []string // roots the user denied; descendants deny without re-asking
	timer      *time.Timer
	batchAt    time.Time
	prompting  bool
	closed     bool
	// promptDone wakes runPrompt callers waiting for the prompting flag to
	// clear. Signaled wherever prompting is set false, and on shutdown.
	promptDone *sync.Cond
}

func newBroker(sessions *session.Manager, logger *logging.Logger, confirm brokerConfirm) *broker {
	b := &broker{
		sessions: sessions,
		logger:   logger,
		confirm:  confirm,
		home:     sessions.HomeDir(),
	}
	b.promptDone = sync.NewCond(&b.mu)
	return b
}

// runPrompt runs fn — a native dialog raised outside the grant pipeline (the
// open-request and workspace-request confirms) — under the broker's one
// prompting flag, so no two native dialogs can ever be on screen at once. A
// second dialog timed to appear under a click aimed at the first's Deny is the
// attack this serializes away. It waits for any grant prompt in flight, and
// returns false without running fn when the broker is shutting down.
func (b *broker) runPrompt(fn func()) bool {
	b.mu.Lock()
	for b.prompting && !b.closed {
		b.promptDone.Wait()
	}
	if b.closed {
		b.mu.Unlock()
		return false
	}
	b.prompting = true
	b.mu.Unlock()

	fn()

	b.mu.Lock()
	b.prompting = false
	b.promptDone.Broadcast()
	b.rearmLocked()
	b.mu.Unlock()
	return true
}

// await parks candidate until a grant covers it (true) or it is denied, times
// out, or the client disconnects (false). Caller must NOT hold any lock.
func (b *broker) await(ctx context.Context, candidate string) bool {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return false
	}
	// A concurrent grant may have landed between the serveAsset miss and here.
	if _, _, ok := b.sessions.AssetRoot(candidate); ok {
		b.mu.Unlock()
		return true
	}
	if b.isSuppressedLocked(candidate) {
		b.mu.Unlock()
		return false
	}
	if len(b.waiters) >= brokerParkCap {
		b.mu.Unlock()
		b.logger.Printf("broker: park cap reached, denying %s", candidate)
		return false
	}
	w := &parkWaiter{path: candidate, dir: filepath.Dir(candidate), ch: make(chan bool, 1)}
	b.waiters = append(b.waiters, w)
	b.armLocked()
	b.mu.Unlock()

	select {
	case allow := <-w.ch:
		return allow
	case <-ctx.Done():
		b.removeWaiter(w)
		return false
	case <-time.After(brokerParkMax):
		b.removeWaiter(w)
		return false
	}
}

// armLocked starts or extends the debounce window. A prompt in flight owns the
// re-arm on its completion, so it does not start a competing timer.
func (b *broker) armLocked() {
	if b.prompting || b.closed {
		return
	}
	if b.timer == nil {
		b.batchAt = time.Now()
		b.timer = time.AfterFunc(brokerDebounce, b.flush)
		return
	}
	if time.Since(b.batchAt) < brokerMaxBatch {
		b.timer.Reset(brokerDebounce)
	}
}

func (b *broker) rearmLocked() {
	if b.closed || b.prompting || b.timer != nil || len(b.waiters) == 0 {
		return
	}
	b.batchAt = time.Now()
	b.timer = time.AfterFunc(brokerDebounce, b.flush)
}

// flush prompts once for the oldest waiter's tree. Runs on the timer goroutine.
func (b *broker) flush() {
	b.mu.Lock()
	b.timer = nil
	if b.closed || b.prompting || len(b.waiters) == 0 {
		b.mu.Unlock()
		return
	}

	// A request that parked while a tree's dialog was up was not yet suppressed when
	// it checked at park time; drop and deny any now-suppressed waiter before
	// grouping, so a denied tree is never re-prompted (a page cannot nag by firing
	// requests during the dialog).
	var denied []*parkWaiter
	if len(b.suppressed) > 0 {
		kept := make([]*parkWaiter, 0, len(b.waiters))
		for _, w := range b.waiters {
			if b.isSuppressedLocked(w.path) {
				denied = append(denied, w)
			} else {
				kept = append(kept, w)
			}
		}
		b.waiters = kept
	}
	if len(b.waiters) == 0 {
		b.rearmLocked()
		b.mu.Unlock()
		denyAll(denied)
		return
	}

	// Group by the oldest waiter's top-level home segment so two unrelated pages
	// denied at once each get their own prompt rather than one over-broad LCA.
	seg := topSegment(b.home, b.waiters[0].path)
	var group, rest []*parkWaiter
	var dirs []string
	for _, w := range b.waiters {
		if topSegment(b.home, w.path) == seg {
			group = append(group, w)
			dirs = append(dirs, w.dir)
		} else {
			rest = append(rest, w)
		}
	}
	lca := commonDir(dirs)

	// Never even prompt for an ancestor that cannot be granted: outside home, a
	// hidden component, or vetoed by the guard (the config/versions tree, e.g. macOS
	// ~/Library which swallows the config dir). CanGrant folds all three, so an Allow
	// the broker offers can never silently no-op inside GrantReadRoot.
	if !b.sessions.CanGrant(lca) {
		b.waiters = rest
		b.rearmLocked()
		b.mu.Unlock()
		denyAll(denied)
		denyAll(group)
		return
	}

	b.prompting = true
	b.waiters = rest
	confirm := b.confirm
	trust := b.trust
	b.mu.Unlock()

	denyAll(denied)
	b.decide(group, lca, confirm, trust)

	b.mu.Lock()
	b.prompting = false
	b.promptDone.Broadcast()
	b.rearmLocked()
	b.mu.Unlock()
}

// decide runs the native prompt (outside the lock) and resolves the group, plus
// any waiter parked during the prompt that the grant now covers.
func (b *broker) decide(group []*parkWaiter, lca string, confirm brokerConfirm, trust trustFunc) {
	// Resolve the folder we are about to name and to grant, so the dialog, the log,
	// the tray, and the installed capability all agree on one directory. Before
	// this, a symlinked folder was named by the alias the page used and granted by
	// its real target, so the user approved one name and a different folder opened.
	//
	// This runs AFTER the decision to prompt, which was made on the lexical path
	// and must stay that way: resolving before it would let a path that fails to
	// resolve answer differently from one that succeeds, which is exactly the
	// existence oracle this design closes. Resolving here changes only WHAT we name
	// and grant, never WHETHER we ask. A folder that does not resolve keeps its
	// lexical name in the dialog and fails closed on Allow.
	grantPath := lca
	if resolved, rErr := filepath.EvalSymlinks(lca); rErr == nil {
		grantPath = filepath.Clean(resolved)
	}

	title, msg := b.dialogText(grantPath)
	choice, err := confirm(title, msg)
	if err != nil {
		b.logger.Printf("broker: confirm error for %s: %v", grantPath, err)
		choice = platform.ConfirmDeny
	}

	if choice == platform.ConfirmDeny {
		b.suppress(lca)
		denyAll(group)
		return
	}

	// GrantCanonicalRoot still re-validates home containment, hidden components,
	// and the guard against the resolved path, so a symlink pointing into the
	// config tree or outside home is refused here even though it was named in a
	// prompt the user answered.
	if gErr := b.sessions.GrantCanonicalRoot(grantPath); gErr != nil {
		b.logger.Printf("broker: grant %s failed: %v", grantPath, gErr)
		b.suppress(lca)
		denyAll(group)
		return
	}
	b.logger.Printf("broker: granted read access to %s", grantPath)

	// Wake the group before any durable-trust work. The read root is installed, so
	// the requests can proceed now; making them wait on a config write or a
	// notification only burns their remaining park deadline and can 403 a request
	// that was already allowed.
	for _, w := range group {
		w.ch <- true
	}

	// The durable half of "Trust this folder". The read root above is installed
	// either way, so the page works even when this is refused: the folder simply
	// asks again next launch. Trust is deliberately the second step, never a
	// replacement for the grant, because a trusted folder only seeds sites whose
	// opened file lives inside it, and the folder in this dialog is often a cousin
	// of the opened page rather than an ancestor of it.
	if choice == platform.ConfirmTrustFolder {
		switch {
		case trust == nil:
			b.logger.Printf("broker: no trust hook wired, %s is granted for this session only", grantPath)
		default:
			if tErr := trust(grantPath); tErr != nil {
				b.logger.Printf("broker: could not trust %s: %v", grantPath, tErr)
			} else {
				b.logger.Printf("broker: trusted folder added from prompt: %s", grantPath)
			}
		}
	}

	// Wake anything parked during the prompt that the new root now authorizes. The
	// resolving form of that check is only needed when the grant landed somewhere
	// other than the name the waiters used, so the ordinary unsymlinked case keeps
	// a filesystem call off the broker lock.
	aliased := grantPath != lca
	b.mu.Lock()
	var remaining, freed []*parkWaiter
	for _, w := range b.waiters {
		if b.authorizedNow(w.path, aliased) {
			freed = append(freed, w)
		} else {
			remaining = append(remaining, w)
		}
	}
	b.waiters = remaining
	b.mu.Unlock()
	for _, w := range freed {
		w.ch <- true
	}
}

func (b *broker) dialogText(lca string) (string, string) {
	who := b.label
	if who == "" {
		who = "A page you opened in HTML Clay"
	}
	msg := fmt.Sprintf("%s is trying to read files in:\n\n%s\n\nAllow read-only access to that folder and everything inside it?", who, lca)
	return "HTML Clay", msg
}

func (b *broker) suppress(root string) {
	b.mu.Lock()
	b.suppressed = append(b.suppressed, root)
	b.mu.Unlock()
}

func (b *broker) isSuppressedLocked(candidate string) bool {
	for _, root := range b.suppressed {
		if session.EqualOrUnder(candidate, root) {
			return true
		}
	}
	return false
}

// authorizedNow reports whether candidate is now covered by some read root. With
// resolve set it also tests the resolved path, so a grant installed under a
// symlink's real name still frees waiters that asked for it under the alias. The
// caller sets it only when the grant landed on a different path than the waiters
// named, which keeps EvalSymlinks off the broker lock the rest of the time. Only
// ever called after the user has already answered a prompt, so the filesystem
// access here cannot influence whether anything is asked.
func (b *broker) authorizedNow(candidate string, resolve bool) bool {
	if _, _, ok := b.sessions.AssetRoot(candidate); ok {
		return true
	}
	if !resolve {
		return false
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return false
	}
	_, _, ok := b.sessions.AssetRoot(filepath.Clean(resolved))
	return ok
}

func (b *broker) removeWaiter(w *parkWaiter) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, x := range b.waiters {
		if x == w {
			b.waiters = append(b.waiters[:i], b.waiters[i+1:]...)
			return
		}
	}
}

// shutdown wakes every parked waiter with deny so in-flight requests can drain,
// and refuses further parking. A group already inside a prompt is resolved when
// the prompt returns (the dialog gives up after 120s).
func (b *broker) shutdown() {
	b.mu.Lock()
	b.closed = true
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	waiters := b.waiters
	b.waiters = nil
	b.promptDone.Broadcast()
	b.mu.Unlock()
	denyAll(waiters)
}

func denyAll(waiters []*parkWaiter) {
	for _, w := range waiters {
		w.ch <- false
	}
}

// topSegment is path's first component below home, or "" if path is not under
// home. It scopes a prompt batch to one top-level tree.
func topSegment(home, path string) string {
	rel, err := filepath.Rel(home, path)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return ""
	}
	if i := strings.IndexByte(rel, filepath.Separator); i >= 0 {
		return rel[:i]
	}
	return rel
}

// commonDir is the deepest directory that contains every path in dirs. Components
// are compared with case folded on case-insensitive filesystems (SamePathComponent),
// so a page cannot reference one real directory under two casings to force a broader
// common ancestor than the assets need. The result keeps the first path's casing,
// which names the same directory.
func commonDir(dirs []string) string {
	if len(dirs) == 0 {
		return ""
	}
	sep := string(filepath.Separator)
	common := strings.Split(dirs[0], sep)
	for _, d := range dirs[1:] {
		parts := strings.Split(d, sep)
		n := len(common)
		if len(parts) < n {
			n = len(parts)
		}
		i := 0
		for i < n && session.SamePathComponent(common[i], parts[i]) {
			i++
		}
		common = common[:i]
	}
	return strings.Join(common, sep)
}
