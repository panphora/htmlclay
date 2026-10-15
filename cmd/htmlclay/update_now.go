package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/panphora/htmlclay/internal/config"
	"github.com/panphora/htmlclay/internal/platform"
	"github.com/panphora/htmlclay/internal/update"
)

// `htmlclay update-now --version X.Y.Z` asks the running app to update itself
// now, through the same restart as the tray's Restart to Update row. A release
// runs it from scripts/update-canary.sh to prove the update path on a real
// install. It is a client of the running app, like the wire CLI: it never
// takes the single-instance lock and never starts the app. The request travels
// as a command frame over the single-instance socket, which only this user can
// reach, and the app only ever installs a version its own updater downloaded
// from the signed feed and verified.
const (
	// accepted: the app is updating to that version, or will as soon as
	// Sparkle has it prepared. "already X.Y.Z": it is running that version.
	updateNowExitOK = 0
	// Bad arguments.
	updateNowExitUsage = 1
	// Nothing answers on the single-instance socket: the app is not running.
	updateNowExitNotRunning = 2
	// "refused <reason>": the app cannot update itself, so nothing happened.
	updateNowExitRefused = 3
	// The app read the request and did not answer: it predates update-now.
	updateNowExitTooOld = 4
	// "failed <reason>": the update cannot go on, and installing by hand now
	// would race an install already set up (a cancelled restart, or Sparkle
	// holding another version that installs at the next quit).
	updateNowExitFailed = 5
	// "starting": the app holds the lock but is not taking commands yet, so
	// the request can be sent again shortly.
	updateNowExitStarting = 6
)

// updateNowOS is the platform update-now runs on. Tests change it.
var updateNowOS = runtime.GOOS

// updateNowWindow is how long a request waits for Sparkle to prepare the
// version. The canary resends every 30 s and each resend starts it again, so a
// request dies about a minute after the canary stops asking.
var updateNowWindow = time.Minute

// updateNowDispatch runs `htmlclay update-now ...` and reports whether it
// handled argv. Like wireDispatch it runs before flag.Parse and the lock.
func updateNowDispatch(argv []string) (int, bool) {
	if len(argv) < 2 || argv[1] != "update-now" {
		return 0, false
	}
	dir, err := config.Dir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "htmlclay update-now: cannot resolve the config dir: %v\n", err)
		return updateNowExitUsage, true
	}
	return runUpdateNow(platform.NewSingleInstance(dir), argv[2:], os.Stdout, os.Stderr), true
}

// runUpdateNow sends the request and prints the app's answer on stdout.
func runUpdateNow(si platform.SingleInstance, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("update-now", flag.ContinueOnError)
	fs.SetOutput(stderr)
	target := fs.String("version", "", "the version to update to, X.Y.Z")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || !update.IsStrictVersion(*target) {
		fmt.Fprintln(stderr, "usage: htmlclay update-now --version X.Y.Z")
		return updateNowExitUsage
	}
	answer, err := si.SendCommand("update-now " + *target)
	if errors.Is(err, platform.ErrNoAnswer) {
		fmt.Fprintln(stdout, "refused the running HTML Clay predates update-now")
		return updateNowExitTooOld
	}
	if err != nil {
		fmt.Fprintf(stderr, "htmlclay update-now: HTML Clay is not running: %v\n", err)
		return updateNowExitNotRunning
	}
	fmt.Fprintln(stdout, answer)
	switch {
	case answer == "accepted", answer == "already "+*target:
		return updateNowExitOK
	case answer == platform.CommandStarting:
		return updateNowExitStarting
	case strings.HasPrefix(answer, "failed "):
		return updateNowExitFailed
	default:
		return updateNowExitRefused
	}
}

// handleInstanceCommand answers one command frame from the single-instance
// socket. The socket only carries frames from this user's own processes.
func (a *app) handleInstanceCommand(cmd string) string {
	fields := strings.Fields(cmd)
	if len(fields) != 2 || fields[0] != "update-now" {
		return "refused unknown command"
	}
	answer := a.updateNow(fields[1])
	a.rt.logger.Printf("update-now %s: %s", fields[1], answer)
	return answer
}

// updateNow starts the update to target through the tray's own click, or says
// why it cannot. It never waits on the update: the canary watches for the new
// version, so the answer only says whether the app took the request.
//
// When Sparkle already holds target prepared it clicks at once. Otherwise it
// records the request and asks Sparkle for a check now; the Ready event clicks
// when target arrives. A resent request repeats the check, which is what
// catches an appcast that had not caught up with the release yet.
func (a *app) updateNow(target string) string {
	if !update.IsStrictVersion(target) {
		return fmt.Sprintf("refused %q is not X.Y.Z", target)
	}
	if updateNowOS != "darwin" {
		return "refused update-now runs on macOS only; this build updates from the tray"
	}
	if target == version {
		return "already " + version
	}
	if !(&update.Release{Version: target}).Newer(version) {
		return fmt.Sprintf("refused v%s is not newer than the running v%s", target, version)
	}
	if a.updateOff != "" {
		return "refused self-update is off: " + a.updateOff
	}
	st := a.rt.sparkle
	if st == nil {
		return "refused self-update is off: Sparkle is not running"
	}
	if reason := a.failedSparkleUpdate(target); reason != "" {
		return "refused " + reason
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	switch {
	case st.startFailed != "":
		return "refused Sparkle could not start: " + st.startFailed
	case st.declined == target:
		return fmt.Sprintf("failed the restart to v%s was cancelled on screen", target)
	case st.prepared == target:
		if !st.clicking && !a.updating.Load() {
			st.clicking = true
			go a.clickForUpdateNow(target)
		}
		return "accepted"
	case st.prepared != "":
		return fmt.Sprintf("failed Sparkle holds v%s prepared, which installs at the next quit", st.prepared)
	}
	st.requested, st.requestedAt = target, time.Now()
	st.fw.CheckNow()
	return "accepted"
}

// takeRequest reports whether update-now is waiting on target, and if so
// claims the click for it. An expired request is dropped. Call with s.mu held.
func (s *sparkleState) takeRequest(target string) bool {
	if s.requested != "" && time.Since(s.requestedAt) > updateNowWindow {
		s.requested = ""
	}
	if s.requested != target || s.clicking {
		return false
	}
	s.requested = ""
	s.clicking = true
	return true
}

// clickForUpdateNow is the tray click on update-now's behalf. A restart the
// person cancelled at the running-work prompt is not tried again for target.
func (a *app) clickForUpdateNow(target string) {
	err := a.clickSparkleUpdate(target)
	st := a.rt.sparkle
	st.mu.Lock()
	st.clicking = false
	if errors.Is(err, errUpdateCancelled) {
		st.declined = target
	}
	st.mu.Unlock()
}
