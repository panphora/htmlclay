package main

import (
	"encoding/json"
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

// releaseInfoURL is the receipt the download-page link reads. Tests point it
// at a local server.
var releaseInfoURL = update.DefaultVersionURL

// The restart waits for a moment with no write request in flight, so a save
// already on the wire lands in this process rather than failing against a
// closed port. Then it stops admitting writes and waits for the ones already
// admitted to finish. A request still running after updateQuietLimit cancels
// the restart instead of being cut off, and admission reopens; the person can
// click again.
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
	if runtime.GOOS == "darwin" {
		return a.startSparkleUpdater(updateCh)
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
	a.reportRestartFailure(r.Version, err)
}

// reportRestartFailure logs a restart that did not happen and tells the person
// this version is still running.
func (a *app) reportRestartFailure(target string, err error) {
	a.rt.logger.Printf("Update to v%s not applied, still running v%s: %v", target, version, err)
	msg := fmt.Sprintf("HTML Clay could not restart into v%s just now, so it is still running v%s. Try Restart to Update again in a moment.", target, version)
	if notifyErr := a.informUser("HTML Clay", msg); notifyErr != nil {
		a.rt.logger.Printf("Could not show the update notice: %v", notifyErr)
	}
}

// checkForUpdateLink is the check every build falls back to when it cannot
// update itself: it only offers the download page.
func (a *app) checkForUpdateLink(updateCh chan<- tray.UpdateInfo) {
	if info := update.Check(version, releaseInfoURL); info != nil {
		a.rt.logger.Printf("Update available: v%s at %s", info.Version, info.URL)
		updateCh <- tray.UpdateInfo{Version: info.Version, URL: info.URL}
	}
}

// applyUpdate restarts into a staged update: restartForUpdate, then swap the
// binary, start the supervisor and quit through the ordinary shutdown.
func (a *app) applyUpdate(r update.Ready, installPath string) error {
	return a.restartForUpdate(r.Version, func(handoff string) error {
		_, err := update.Apply(update.UpdatesDir(a.rt.configDir), update.ApplyParams{
			From:        version,
			To:          r.Version,
			InstallPath: installPath,
			StagedPath:  r.StagedPath,
			OldPID:      os.Getpid(),
			HandoffPath: handoff,
		})
		if err != nil {
			return err
		}
		a.rt.logger.Printf("Restarting to finish the update to v%s", r.Version)
		if a.rt.quitForUpdate != nil {
			a.rt.quitForUpdate()
		} else {
			tray.Quit()
		}
		return nil
	})
}

// restartForUpdate is what every update restart shares. In order: ask when an
// AI edit or a program is running, wait for in-flight writes to land and stop
// admitting new ones, hand the open sessions to target in the handoff file, then
// call install with its path. Until install returns nil every failure removes
// the handoff, reopens writes and leaves this process running as it was; after
// that the app stays marked as updating with writes closed, so a second click
// cannot start another restart and no save can land after the handoff.
func (a *app) restartForUpdate(target string, install func(handoffPath string) error) error {
	if !a.updating.CompareAndSwap(false, true) {
		return errors.New("an update restart is already under way")
	}
	exiting := false
	defer func() {
		if !exiting {
			a.reopenWrites()
			a.updating.Store(false)
		}
	}()

	if a.runningWork() > 0 {
		msg := fmt.Sprintf("Restart HTML Clay now to update to v%s?\n\nAn AI edit or a program started by a document is still running, and restarting stops it.", target)
		ok, err := a.confirmTrayChange("HTML Clay", msg, "Restart")
		if err != nil || !ok {
			return errUpdateCancelled
		}
	}
	if !waitQuiet(&a.rt.writesInFlight, updateQuietFor, updateQuietLimit) {
		return errUpdateBusy
	}
	if !a.drainWrites(updateQuietLimit) {
		return errUpdateBusy
	}

	handoff := filepath.Join(a.rt.configDir, updateHandoffFile)
	if err := a.writeUpdateHandoff(handoff, target); err != nil {
		os.Remove(handoff)
		return fmt.Errorf("write the session handoff: %w", err)
	}
	if err := install(handoff); err != nil {
		os.Remove(handoff)
		return err
	}
	exiting = true
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
// does, and counting them would mean the app is never quiet. While an update
// restart has writes closed, a write is refused with a retriable 503 instead.
func (a *app) countWrites(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if !a.admitWrite() {
			refuseWriteWhileRestarting(w)
			return
		}
		defer a.rt.writesInFlight.Add(-1)
		next.ServeHTTP(w, r)
	})
}

// admitWrite counts one write in, unless writes are closed. The check and the
// count happen under one lock, so once drainWrites has closed admission the
// count can only fall.
func (a *app) admitWrite() bool {
	a.rt.writesMu.Lock()
	defer a.rt.writesMu.Unlock()
	if a.rt.writesClosed {
		return false
	}
	a.rt.writesInFlight.Add(1)
	return true
}

// drainWrites closes admission and waits up to limit for every admitted write
// to finish. On timeout it reopens admission and reports false, so the app
// keeps running as it was.
func (a *app) drainWrites(limit time.Duration) bool {
	a.rt.writesMu.Lock()
	a.rt.writesClosed = true
	a.rt.writesMu.Unlock()
	deadline := time.Now().Add(limit)
	for a.rt.writesInFlight.Load() != 0 {
		if time.Now().After(deadline) {
			a.reopenWrites()
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return true
}

func (a *app) reopenWrites() {
	a.rt.writesMu.Lock()
	a.rt.writesClosed = false
	a.rt.writesMu.Unlock()
}

// refuseWriteWhileRestarting answers a write that arrived after the restart
// closed admission. ClayJS treats a 503 like a lost connection: the page stays
// dirty and the next save sends it again, by then to the new version.
func refuseWriteWhileRestarting(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "2")
	w.WriteHeader(http.StatusServiceUnavailable)
	json.NewEncoder(w).Encode(map[string]any{
		"ok":      false,
		"msg":     "HTML Clay is restarting to finish an update, so nothing was saved. Try again in a few seconds.",
		"msgType": "error",
		"code":    "restarting",
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
