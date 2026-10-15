// Package sparkle drives Sparkle 2, the framework HTML Clay updates itself
// through on macOS. The framework is not linked: Load opens the copy embedded
// in the running bundle at Contents/Frameworks, so a binary built with plain
// `go build`, and every test binary, runs without it.
package sparkle

// FrameworkRelPath is where build.sh embeds the framework, relative to the
// .app bundle.
const FrameworkRelPath = "Contents/Frameworks/Sparkle.framework"

// EventKind says what Sparkle reported.
type EventKind int

const (
	// Started: the updater is running and schedules its own checks.
	Started EventKind = iota + 1
	// StartFailed: startUpdater refused; Text is Sparkle's reason.
	StartFailed
	// Ready: an update is downloaded, verified and prepared, and InstallNow
	// will install it and relaunch. Text is its CFBundleVersion.
	Ready
	// Aborted: an update cycle stopped on an error; Text is the error.
	Aborted
	// InstallFailed: InstallNow found no prepared update to install.
	InstallFailed
)

// Event is one report from Sparkle. Events arrive on their own goroutine, never
// on the main thread, so a handler may block.
type Event struct {
	Kind EventKind
	Text string
}
