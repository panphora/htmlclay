package main

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/panphora/htmlclay/internal/sparkle"
	"github.com/panphora/htmlclay/internal/tray"
	"github.com/panphora/htmlclay/internal/update"
)

// sparkleInstallTimeout bounds the wait between handing the restart to Sparkle
// and the quit Apple event it sends. Past it the app reopens writes and tells
// the person it is still waiting, but keeps the handoff and the recovery state:
// Sparkle still holds the install request and may yet carry it out. Only a
// definite failure from Sparkle ends the install.
var sparkleInstallTimeout = 40 * time.Second

// sparkleRecoveryFresh is how old a click's recovery may be when the app quits
// for it to still cover that quit. The supervisor waits 60 s for this process
// to exit and the new version accepts a handoff up to 60 s old; past this the
// quit path writes both again.
var sparkleRecoveryFresh = 30 * time.Second

// sparkleFramework is the part of internal/sparkle the app drives, behind an
// interface so tests can stand in for the framework.
type sparkleFramework interface {
	PublicKey() string
	Writable(bundle string) error
	Load(frameworkPath string) error
	Start(events func(sparkle.Event))
	InstallNow()
	CheckNow()
}

type realSparkle struct{}

func (realSparkle) PublicKey() string                { return sparkle.InfoString("SUPublicEDKey") }
func (realSparkle) Writable(bundle string) error     { return sparkle.BundleWritable(bundle) }
func (realSparkle) Load(path string) error           { return sparkle.Load(path) }
func (realSparkle) Start(events func(sparkle.Event)) { sparkle.Start(events) }
func (realSparkle) InstallNow()                      { sparkle.InstallNow() }
func (realSparkle) CheckNow()                        { sparkle.CheckNow() }

// sparkleState is the framework plus the one install that may be waiting on
// Sparkle's quit event. bundle and exe are the installed .app and the running
// binary in it; recovery is set up only when bundle is known.
type sparkleState struct {
	fw     sparkleFramework
	mu     sync.Mutex
	failed chan string
	bundle string
	exe    string
	// prepared is the version Sparkle holds downloaded and prepared. Sparkle
	// installs it on any quit, so prepareSparkleQuit starts recovery for it.
	prepared string
	// begun is when recovery started for the install a click asked for, and
	// zero when no click is waiting on Sparkle.
	begun time.Time
	// startFailed is Sparkle's reason when startUpdater refused.
	startFailed string
	// requested is the version update-now asked for and is waiting on Sparkle
	// to prepare, asked at requestedAt. See update_now.go.
	requested   string
	requestedAt time.Time
	// clicking is set while update-now's click is running, so a resent request
	// never starts a second one.
	clicking bool
	// declined is the version whose update-now restart the person cancelled.
	declined string
}

// fail ends a pending install with reason. It does nothing when no install is
// pending, so an error from an ordinary background check is only logged.
func (s *sparkleState) fail(reason string) {
	s.mu.Lock()
	ch := s.failed
	s.failed = nil
	s.mu.Unlock()
	if ch != nil {
		ch <- reason
	}
}

// appBundleFor returns the .app that holds exe, an executable at
// X.app/Contents/MacOS/name, and false when exe is not inside one.
func appBundleFor(exe string) (string, bool) {
	macos := path.Dir(exe)
	contents := path.Dir(macos)
	bundle := path.Dir(contents)
	if path.Base(macos) != "MacOS" || path.Base(contents) != "Contents" || path.Ext(bundle) != ".app" {
		return "", false
	}
	return bundle, true
}

// sparkleGate returns nil only when this macOS build may update itself through
// Sparkle. Every other answer leaves the tray on the download-page link.
func sparkleGate(channel, ver, edKey string, off bool, bundle string, inBundle bool, writable func(string) error) error {
	switch {
	case channel == "":
		return errors.New("this build has no update channel")
	case !update.IsStrictVersion(ver):
		return fmt.Errorf("version %q is not a release version", ver)
	case !inBundle:
		return errors.New("HTML Clay is not running from an app bundle")
	case edKey == "":
		return errors.New("the bundle has no SUPublicEDKey")
	case off:
		return errors.New(`config sets "updates": "off"`)
	case strings.Contains(bundle, "/AppTranslocation/"):
		return fmt.Errorf("%s is a translocated copy; moving HTML Clay to Applications fixes that", bundle)
	}
	return writable(bundle)
}

// startSparkleUpdater is startSelfUpdater on macOS. It loads the embedded
// framework and starts Sparkle's own scheduled checks when the gate allows it,
// and reports whether it did.
func (a *app) startSparkleUpdater(updateCh chan<- tray.UpdateInfo) bool {
	if a.rt.sparkle == nil {
		a.rt.sparkle = &sparkleState{fw: realSparkle{}}
	}
	fw := a.rt.sparkle.fw
	installPath, err := update.InstallPath()
	bundle, inBundle := appBundleFor(installPath)
	if err == nil {
		err = sparkleGate(updateChannel, version, fw.PublicKey(), a.rt.cfg.SelfUpdateOff(), bundle, inBundle, fw.Writable)
	}
	if err == nil {
		err = fw.Load(filepath.Join(bundle, sparkle.FrameworkRelPath))
	}
	if err != nil {
		a.rt.logger.Printf("Self-update is off: %v", err)
		a.updateOff = err.Error()
		return false
	}
	a.rt.logger.Printf("Self-update is on through Sparkle for %s", bundle)
	a.rt.sparkle.bundle, a.rt.sparkle.exe = bundle, installPath
	sparkle.SetSkip(a.failedSparkleUpdate)
	fw.Start(func(ev sparkle.Event) { a.onSparkleEvent(ev, updateCh) })
	return true
}

func (a *app) onSparkleEvent(ev sparkle.Event, updateCh chan<- tray.UpdateInfo) {
	switch ev.Kind {
	case sparkle.Started:
		a.rt.logger.Printf("Sparkle started")
	case sparkle.StartFailed:
		a.rt.logger.Printf("Sparkle could not start, so updates link to the download page: %s", ev.Text)
		a.rt.sparkle.mu.Lock()
		a.rt.sparkle.startFailed = ev.Text
		a.rt.sparkle.mu.Unlock()
		a.checkForUpdateLink(updateCh)
	case sparkle.Ready:
		target := ev.Text
		if !update.IsStrictVersion(target) {
			a.rt.logger.Printf("Ignoring a Sparkle update with version %q, which is not X.Y.Z", target)
			return
		}
		a.rt.logger.Printf("Update %s is downloaded and prepared by Sparkle", target)
		a.rt.sparkle.mu.Lock()
		a.rt.sparkle.prepared = target
		asked := a.rt.sparkle.takeRequest(target)
		a.rt.sparkle.mu.Unlock()
		if asked {
			a.rt.logger.Printf("Installing v%s now, as update-now asked", target)
			go a.clickForUpdateNow(target)
		}
		updateCh <- tray.UpdateInfo{Version: target, Apply: func() { a.clickSparkleUpdate(target) }}
	case sparkle.Aborted:
		a.rt.logger.Printf("Sparkle stopped an update: %s", ev.Text)
		a.rt.sparkle.fail(ev.Text)
	case sparkle.InstallFailed:
		a.rt.sparkle.fail(ev.Text)
	case sparkle.CheckSkipped:
		a.rt.logger.Printf("Sparkle did not check for updates now: %s", ev.Text)
	}
}

// failedSparkleUpdate is Sparkle's skip check: the reason to refuse target,
// or "" to let it through. A version the recovery supervisor rolled back is
// refused until a newer one is published.
func (a *app) failedSparkleUpdate(target string) string {
	st, err := update.ReadState(update.UpdatesDir(a.rt.configDir))
	if err != nil || st == nil || st.Failed == nil || st.Failed.Version != target {
		return ""
	}
	return fmt.Sprintf("v%s did not start after an earlier update (%s)", target, st.Failed.Reason)
}

// beginSparkleRecovery copies the installed bundle aside and starts the
// recovery supervisor from the copy, which puts it back if target never
// reports serving. quit says Sparkle installs on a plain quit and relaunches
// nothing, so the supervisor checks the new version itself. It returns the
// update's txid, or "" when recovery could not be set up; the install then goes
// ahead without it.
func (a *app) beginSparkleRecovery(target, handoff string, quit bool) string {
	st := a.rt.sparkle
	if st.bundle == "" {
		return ""
	}
	rec, err := update.BeginSparkle(update.UpdatesDir(a.rt.configDir), update.SparkleParams{
		From:        version,
		To:          target,
		Bundle:      st.bundle,
		Executable:  st.exe,
		OldPID:      os.Getpid(),
		HandoffPath: handoff,
		Quit:        quit,
	})
	if err != nil {
		a.rt.logger.Printf("Installing v%s without recovery: %v", target, err)
		return ""
	}
	a.rt.logger.Printf("Recovery for v%s runs from %s", target, rec.PreviousPath)
	return rec.Txid
}

// clickSparkleUpdate is the tray's Restart to Update row on macOS. update-now
// runs the same click and reads its result.
func (a *app) clickSparkleUpdate(target string) error {
	err := a.restartForUpdate(target, func(handoff string) error {
		return a.installWithSparkle(target, handoff)
	})
	if err == nil || errors.Is(err, errUpdateCancelled) {
		return err
	}
	a.reportRestartFailure(target, err)
	return err
}

// installWithSparkle runs Sparkle's immediate install. Sparkle answers by
// sending this app a quit Apple event, which runs the ordinary shutdown, then
// installs and relaunches; the new version takes the handoff back. Only a
// definite failure from Sparkle removes the handoff and the recovery state and
// leaves this version running as it was. If no quit has started within
// sparkleInstallTimeout, writes reopen and both stay, because Sparkle may still
// quit the app to install; prepareSparkleQuit renews them at that quit.
func (a *app) installWithSparkle(target, handoff string) error {
	st := a.rt.sparkle
	failed := make(chan string, 1)
	st.mu.Lock()
	st.failed = failed
	st.begun = time.Now()
	st.mu.Unlock()
	txid := a.beginSparkleRecovery(target, handoff, false)
	a.rt.logger.Printf("Handing the restart to Sparkle to install v%s", target)
	st.fw.InstallNow()
	go a.awaitSparkleInstall(target, handoff, txid, failed, sparkleInstallTimeout)
	return nil
}

func (a *app) awaitSparkleInstall(target, handoff, txid string, failed chan string, timeout time.Duration) {
	st := a.rt.sparkle
	var reason string
	select {
	case reason = <-failed:
	case <-time.After(timeout):
		a.rt.logger.Printf("Sparkle has not restarted HTML Clay within %s; v%s keeps running and the update finishes when Sparkle quits it", timeout, version)
		a.reopenWrites()
		msg := fmt.Sprintf("The update to v%s has not started yet, so HTML Clay is still running v%s. It finishes the next time HTML Clay quits.", target, version)
		if err := a.informUser("HTML Clay", msg); err != nil {
			a.rt.logger.Printf("Could not show the update notice: %v", err)
		}
		reason = <-failed
	}
	st.mu.Lock()
	if st.failed == failed {
		st.failed = nil
	}
	st.begun = time.Time{}
	st.mu.Unlock()
	a.mu.Lock()
	stopping := a.stopping
	a.mu.Unlock()
	if stopping {
		return
	}
	os.Remove(handoff)
	if txid != "" {
		update.ClearState(update.UpdatesDir(a.rt.configDir), txid)
	}
	a.reopenWrites()
	a.updating.Store(false)
	a.reportRestartFailure(target, errors.New(reason))
}

// prepareSparkleQuit runs at every quit, before the shutdown. Sparkle installs
// a prepared update on any quit, so whenever it holds one this makes sure a
// recovery supervisor watches that install. After a click the recovery that
// click started covers the quit while it is fresh; an older one (the quit came
// late) gets a new handoff and a new recovery, so neither has expired by the
// time the new version starts. A plain quit gets a recovery that verifies the
// new version without relaunching it.
func (a *app) prepareSparkleQuit() {
	st := a.rt.sparkle
	if st == nil {
		return
	}
	st.mu.Lock()
	target, begun := st.prepared, st.begun
	st.mu.Unlock()
	if target == "" {
		return
	}
	if !a.updating.Load() {
		a.rt.logger.Printf("Quitting with v%s prepared; Sparkle installs it now", target)
		a.beginSparkleRecovery(target, "", true)
		return
	}
	if !begun.IsZero() && time.Since(begun) < sparkleRecoveryFresh {
		return
	}
	handoff := filepath.Join(a.rt.configDir, updateHandoffFile)
	if !a.drainWrites(updateQuietLimit) {
		a.rt.logger.Printf("Quitting for the update to v%s with writes still in flight", target)
	}
	if err := a.writeUpdateHandoff(handoff, target); err != nil {
		a.rt.logger.Printf("Could not renew the session handoff for v%s: %v", target, err)
		os.Remove(handoff)
		handoff = ""
	}
	st.mu.Lock()
	st.begun = time.Now()
	st.mu.Unlock()
	a.beginSparkleRecovery(target, handoff, false)
}
