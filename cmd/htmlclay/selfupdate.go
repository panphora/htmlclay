package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/panphora/htmlclay/internal/tray"
	"github.com/panphora/htmlclay/internal/update"
)

// updateChannel is set by the release workflow's Windows and Linux builds with
// -X main.updateChannel=stable. A local build leaves it empty, so it can never
// replace itself with a published release. macOS updates through Sparkle.
var updateChannel = ""

// The restart waits for a moment with no write request in flight, so a save
// already on the wire lands in this process rather than failing against a
// closed port. A request still running after updateQuietLimit cancels the
// restart instead of being cut off; the person can click again.
var (
	updateQuietFor   = 500 * time.Millisecond
	updateQuietLimit = 5 * time.Second
)

var (
	errUpdateCancelled = errors.New("the restart was cancelled")
	errUpdateBusy      = errors.New("requests were still in flight")
)

// selfUpdateGate returns nil only when this build may download and apply its
// own updates. Every other answer leaves the tray on the download-page link.
func selfUpdateGate(channel, ver, goos string, trustedKeys int, off bool, installPath string, writable func(string) error) error {
	switch {
	case channel == "":
		return errors.New("this build has no update channel")
	case !update.IsStrictVersion(ver):
		return fmt.Errorf("version %q is not a release version", ver)
	case goos != "windows" && goos != "linux":
		return fmt.Errorf("the self-updater runs on Windows and Linux, not %s", goos)
	case trustedKeys == 0:
		return errors.New("no feed signing key is built in")
	case off:
		return errors.New(`config sets "updates": "off"`)
	}
	return writable(installPath)
}

// startSelfUpdater starts the background check when the gate allows it and
// reports whether it did. When it returns false the caller keeps the plain
// release-info check that only links to the website.
func (a *app) startSelfUpdater(updateCh chan<- tray.UpdateInfo) bool {
	// The restart is offered from the tray, so a run without one has no way to
	// apply what it would download.
	if a.noTray {
		return false
	}
	installPath, err := update.InstallPath()
	if err == nil {
		err = selfUpdateGate(updateChannel, version, runtime.GOOS, len(update.TrustedKeys), a.rt.cfg.SelfUpdateOff(), installPath, update.CheckWritable)
	}
	if err != nil {
		a.rt.logger.Printf("Self-update is off: %v", err)
		return false
	}
	feed := update.DefaultFeedURL
	if v := os.Getenv("HTMLCLAY_UPDATE_FEED"); v != "" {
		feed = v
	}
	u := &update.Updater{
		FeedURL:     feed,
		Keys:        update.TrustedKeys,
		Client:      &http.Client{Timeout: 10 * time.Minute},
		Current:     version,
		GOOS:        runtime.GOOS,
		GOARCH:      runtime.GOARCH,
		Dir:         update.UpdatesDir(a.rt.configDir),
		InstallPath: installPath,
		Logf:        a.rt.logger.Printf,
	}
	a.rt.logger.Printf("Self-update is on, checking %s", feed)
	go u.Run(nil, func(r update.Ready) {
		updateCh <- tray.UpdateInfo{Version: r.Version, Apply: func() { a.clickApplyUpdate(r, installPath) }}
	})
	return true
}

// clickApplyUpdate is the tray's Restart to Update row. A failure leaves this
// version running and the row in place, so the person can try again.
func (a *app) clickApplyUpdate(r update.Ready, installPath string) {
	err := a.applyUpdate(r, installPath)
	if err == nil || errors.Is(err, errUpdateCancelled) {
		return
	}
	a.rt.logger.Printf("Update to v%s not applied, still running v%s: %v", r.Version, version, err)
	msg := fmt.Sprintf("HTML Clay could not restart into v%s just now, so it is still running v%s. Try Restart to Update again in a moment.", r.Version, version)
	if notifyErr := a.informUser("HTML Clay", msg); notifyErr != nil {
		a.rt.logger.Printf("Could not show the update notice: %v", notifyErr)
	}
}

// applyUpdate restarts into a staged update. In order: ask when an AI edit or
// a program is running, wait for in-flight writes to land, hand the open
// sessions to the new version, swap the binary and start the supervisor, then
// quit through the ordinary shutdown. Until the swap succeeds every failure
// removes the handoff and leaves this process running as it was.
func (a *app) applyUpdate(r update.Ready, installPath string) error {
	if !a.updating.CompareAndSwap(false, true) {
		return errors.New("an update restart is already under way")
	}
	exiting := false
	defer func() {
		if !exiting {
			a.updating.Store(false)
		}
	}()

	if a.runningWork() > 0 {
		msg := fmt.Sprintf("Restart HTML Clay now to update to v%s?\n\nAn AI edit or a program started by a document is still running, and restarting stops it.", r.Version)
		ok, err := a.confirmTrayChange("HTML Clay", msg, "Restart")
		if err != nil || !ok {
			return errUpdateCancelled
		}
	}
	if !waitQuiet(&a.rt.writesInFlight, updateQuietFor, updateQuietLimit) {
		return errUpdateBusy
	}

	handoff := filepath.Join(a.rt.configDir, updateHandoffFile)
	if err := a.writeUpdateHandoff(handoff, r.Version); err != nil {
		os.Remove(handoff)
		return fmt.Errorf("write the session handoff: %w", err)
	}
	_, err := update.Apply(update.UpdatesDir(a.rt.configDir), update.ApplyParams{
		From:        version,
		To:          r.Version,
		InstallPath: installPath,
		StagedPath:  r.StagedPath,
		OldPID:      os.Getpid(),
		HandoffPath: handoff,
	})
	if err != nil {
		os.Remove(handoff)
		return err
	}
	exiting = true
	a.rt.logger.Printf("Restarting to finish the update to v%s", r.Version)
	if a.rt.quitForUpdate != nil {
		a.rt.quitForUpdate()
	} else {
		tray.Quit()
	}
	return nil
}

// runningWork counts the AI edits and helper program requests running across
// every live site.
func (a *app) runningWork() int {
	a.mu.Lock()
	sites := append([]*site(nil), a.sites...)
	a.mu.Unlock()
	n := 0
	for _, s := range sites {
		if s.srv != nil {
			n += s.srv.RunningWork()
		}
	}
	return n
}

// countWrites wraps the one HTTP handler and counts requests that can change
// a file: saves, uploads, restores. Reads are left out on purpose, because the
// live-sync and wire streams are GETs that stay open for as long as a tab
// does, and counting them would mean the app is never quiet.
func (a *app) countWrites(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		a.rt.writesInFlight.Add(1)
		defer a.rt.writesInFlight.Add(-1)
		next.ServeHTTP(w, r)
	})
}

// waitQuiet reports whether inFlight stayed at zero for quiet, waiting at
// most limit for that to happen.
func waitQuiet(inFlight *atomic.Int64, quiet, limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	var since time.Time
	for {
		now := time.Now()
		if inFlight.Load() == 0 {
			if since.IsZero() {
				since = now
			}
			if now.Sub(since) >= quiet {
				return true
			}
		} else {
			since = time.Time{}
		}
		if now.After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}
