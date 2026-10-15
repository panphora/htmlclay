package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/panphora/htmlclay/internal/sparkle"
	"github.com/panphora/htmlclay/internal/tray"
	"github.com/panphora/htmlclay/internal/update"
)

// sparkleInstallTimeout bounds the wait between handing the restart to Sparkle
// and the quit Apple event it sends. Past it the restart counts as failed: the
// handoff is removed and the row can be clicked again. It stays well under the
// handoff's own 60 s lifetime.
var sparkleInstallTimeout = 40 * time.Second

// sparkleFramework is the part of internal/sparkle the app drives, behind an
// interface so tests can stand in for the framework.
type sparkleFramework interface {
	PublicKey() string
	Writable(bundle string) error
	Load(frameworkPath string) error
	Start(events func(sparkle.Event))
	InstallNow()
}

type realSparkle struct{}

func (realSparkle) PublicKey() string                { return sparkle.InfoString("SUPublicEDKey") }
func (realSparkle) Writable(bundle string) error     { return sparkle.BundleWritable(bundle) }
func (realSparkle) Load(path string) error           { return sparkle.Load(path) }
func (realSparkle) Start(events func(sparkle.Event)) { sparkle.Start(events) }
func (realSparkle) InstallNow()                      { sparkle.InstallNow() }

// sparkleState is the framework plus the one install that may be waiting on
// Sparkle's quit event. bundle and exe are the installed .app and the running
// binary in it; recovery is set up only when bundle is known.
type sparkleState struct {
	fw     sparkleFramework
	mu     sync.Mutex
	failed chan string
	bundle string
	exe    string
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
	macos := filepath.Dir(exe)
	contents := filepath.Dir(macos)
	bundle := filepath.Dir(contents)
	if filepath.Base(macos) != "MacOS" || filepath.Base(contents) != "Contents" || filepath.Ext(bundle) != ".app" {
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
		a.checkForUpdateLink(updateCh)
	case sparkle.Ready:
		target := ev.Text
		if !update.IsStrictVersion(target) {
			a.rt.logger.Printf("Ignoring a Sparkle update with version %q, which is not X.Y.Z", target)
			return
		}
		a.rt.logger.Printf("Update %s is downloaded and prepared by Sparkle", target)
		updateCh <- tray.UpdateInfo{Version: target, Apply: func() { a.clickSparkleUpdate(target) }}
	case sparkle.Aborted:
		a.rt.logger.Printf("Sparkle stopped an update: %s", ev.Text)
		a.rt.sparkle.fail(ev.Text)
	case sparkle.InstallFailed:
		a.rt.sparkle.fail(ev.Text)
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
// reports serving. It returns the update's txid, or "" when recovery could not
// be set up; the install then goes ahead without it, as a plain quit's does.
func (a *app) beginSparkleRecovery(target, handoff string) string {
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
	})
	if err != nil {
		a.rt.logger.Printf("Installing v%s without recovery: %v", target, err)
		return ""
	}
	a.rt.logger.Printf("Recovery for v%s runs from %s", target, rec.PreviousPath)
	return rec.Txid
}

// clickSparkleUpdate is the tray's Restart to Update row on macOS.
func (a *app) clickSparkleUpdate(target string) {
	err := a.restartForUpdate(target, func(handoff string) error {
		return a.installWithSparkle(target, handoff)
	})
	if err == nil || errors.Is(err, errUpdateCancelled) {
		return
	}
	a.reportRestartFailure(target, err)
}

// installWithSparkle runs Sparkle's immediate install. Sparkle answers by
// sending this app a quit Apple event, which runs the ordinary shutdown, then
// installs and relaunches; the new version takes the handoff back. If Sparkle
// reports a failure, or no quit has started within sparkleInstallTimeout, the
// handoff and the recovery state are removed and this version keeps running.
func (a *app) installWithSparkle(target, handoff string) error {
	st := a.rt.sparkle
	timeout := sparkleInstallTimeout
	failed := make(chan string, 1)
	st.mu.Lock()
	st.failed = failed
	st.mu.Unlock()
	txid := a.beginSparkleRecovery(target, handoff)
	a.rt.logger.Printf("Handing the restart to Sparkle to install v%s", target)
	st.fw.InstallNow()
	go func() {
		var reason string
		select {
		case reason = <-failed:
		case <-time.After(timeout):
			reason = fmt.Sprintf("Sparkle did not restart HTML Clay within %s", timeout)
		}
		st.mu.Lock()
		if st.failed == failed {
			st.failed = nil
		}
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
		a.updating.Store(false)
		a.reportRestartFailure(target, errors.New(reason))
	}()
	return nil
}
