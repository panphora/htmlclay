//go:build linux

package platform

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/url"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

// errPortalUnavailable marks the failures that mean "this desktop has no usable
// portal", which is the only class the caller may fall back from. Once OpenFile
// has been accepted the user can see a dialog, and falling back after that
// would stack a second picker on top of the first, so every later failure is
// surfaced instead.
var errPortalUnavailable = errors.New("xdg desktop portal unavailable")

const (
	portalBusName    = "org.freedesktop.portal.Desktop"
	portalObjectPath = "/org/freedesktop/portal/desktop"
	fileChooserIface = "org.freedesktop.portal.FileChooser"
	requestIface     = "org.freedesktop.portal.Request"
)

// portalProbeTimeout bounds the version probe only, never the dialog. The dialog
// may legitimately take as long as the user takes, which is what pickerTimeout is
// for. A portal that will not answer "what version are you" within a couple of
// seconds is one to walk away from: the portal is tried FIRST, so an unbounded
// probe against a wedged portal service would be a multi-minute hang on a desktop
// where zenity would have answered immediately.
const portalProbeTimeout = 2 * time.Second

// portalSelectFolder asks the desktop itself for a directory through
// org.freedesktop.portal.FileChooser. Unlike the zenity and kdialog paths it
// needs no helper binary and yields the session's own dialog on GNOME, KDE,
// Wayland and Flatpak alike, which is why selectFolder tries it first.
//
// A private bus connection, not dbus.SessionBus: the shared connection is
// process-wide, so a Close to clean up after one picker would tear it out from
// under any other user (the systray talks D-Bus on Linux too), and leaving
// match rules behind on it would leak. A private connection is closed whole.
func portalSelectFolder(prompt string) (string, bool, error) {
	// NoAutoStartup: the plain SessionBusPrivate runs dbus-launch when no session
	// bus is found, which would spawn a stray bus daemon that outlives us, on a
	// machine that by definition has no portal either. Failing instead is the
	// fallback signal the zenity path wants.
	conn, err := dbus.SessionBusPrivateNoAutoStartup()
	if err != nil {
		return "", false, fmt.Errorf("%w: %v", errPortalUnavailable, err)
	}
	defer conn.Close()
	if err := conn.Auth(nil); err != nil {
		return "", false, fmt.Errorf("%w: %v", errPortalUnavailable, err)
	}
	if err := conn.Hello(); err != nil {
		return "", false, fmt.Errorf("%w: %v", errPortalUnavailable, err)
	}

	// The "directory" option arrived in FileChooser version 3. An older portal
	// does not reject unknown options, it ignores them and opens a FILE picker,
	// which would hand back a file where the caller granted a folder. Refusing
	// to proceed without the version property makes that silent widening
	// impossible; the property also fails fast when no portal service exists at
	// all, which is the fallback signal for the zenity path.
	obj := conn.Object(portalBusName, portalObjectPath)
	probeCtx, cancelProbe := context.WithTimeout(context.Background(), portalProbeTimeout)
	defer cancelProbe()
	var v dbus.Variant
	if err := obj.CallWithContext(
		probeCtx,
		"org.freedesktop.DBus.Properties.Get",
		0,
		fileChooserIface,
		"version",
	).Store(&v); err != nil {
		return "", false, fmt.Errorf("%w: %v", errPortalUnavailable, err)
	}
	version, ok := v.Value().(uint32)
	if !ok || version < 3 {
		return "", false, fmt.Errorf("%w: FileChooser version %v has no directory mode", errPortalUnavailable, v.Value())
	}

	// The response arrives as a signal on a request object whose path is
	// derivable from our unique name and a token we choose, so the match rule
	// can be installed before the call: a reply that raced the subscription
	// would otherwise be lost and the picker would hang to its timeout.
	token := fmt.Sprintf("htmlclay_%d", rand.Int63())
	sender := strings.TrimPrefix(conn.Names()[0], ":")
	sender = strings.ReplaceAll(sender, ".", "_")
	expected := dbus.ObjectPath(portalObjectPath + "/request/" + sender + "/" + token)

	if err := conn.AddMatchSignal(
		dbus.WithMatchInterface(requestIface),
		dbus.WithMatchMember("Response"),
		dbus.WithMatchObjectPath(expected),
	); err != nil {
		return "", false, fmt.Errorf("%w: %v", errPortalUnavailable, err)
	}
	signals := make(chan *dbus.Signal, 1)
	conn.Signal(signals)

	options := map[string]dbus.Variant{
		"handle_token": dbus.MakeVariant(token),
		"directory":    dbus.MakeVariant(true),
	}
	// One deadline covers the dialog from here: the OpenFile handshake and the
	// wait for its Response. OpenFile returns a request handle rather than waiting
	// for the user, so it is normally immediate, but godbus applies no reply
	// timeout of its own and a wedged portal would park this goroutine for the
	// life of the process, one per Add Trusted Folder click.
	ctx, cancel := context.WithTimeout(context.Background(), pickerTimeout)
	defer cancel()

	var handle dbus.ObjectPath
	if err := obj.CallWithContext(ctx, fileChooserIface+".OpenFile", 0, "", prompt, options).Store(&handle); err != nil {
		// A deadline here is not errPortalUnavailable. The portal may already be
		// showing the dialog, and falling back would stack zenity on top of it.
		if ctx.Err() != nil {
			return "", false, errors.New("portal folder picker timed out opening the dialog")
		}
		return "", false, fmt.Errorf("%w: %v", errPortalUnavailable, err)
	}
	// Portals predating handle_token (0.5) return a server-chosen path instead
	// of the derived one; subscribe to it too rather than assuming.
	if handle != expected {
		conn.AddMatchSignal(
			dbus.WithMatchInterface(requestIface),
			dbus.WithMatchMember("Response"),
			dbus.WithMatchObjectPath(handle),
		)
	}

	for {
		select {
		case sig, open := <-signals:
			if !open {
				return "", false, errors.New("portal connection closed before a response")
			}
			if sig.Name != requestIface+".Response" || (sig.Path != expected && sig.Path != handle) {
				continue
			}
			code, results, err := portalResponse(sig.Body)
			if err != nil {
				return "", false, err
			}
			return portalDir(code, results)
		case <-ctx.Done():
			// Same contract as the helper-tool timeout: kill the dialog and
			// surface an error, never a silent cancel. Close gets its own short
			// deadline, and ctx is already spent: this branch runs precisely when
			// the portal is not answering, so an unbounded Close here would block
			// forever on the very service that just failed to reply, leaking the
			// connection and swallowing the timeout error it was about to return.
			closeCtx, cancelClose := context.WithTimeout(context.Background(), portalProbeTimeout)
			conn.Object(portalBusName, handle).CallWithContext(closeCtx, requestIface+".Close", 0)
			cancelClose()
			return "", false, errors.New("portal folder picker timed out")
		}
	}
}

// portalResponse unpacks a Request.Response body: a uint32 outcome and a
// results dict.
func portalResponse(body []interface{}) (uint32, map[string]dbus.Variant, error) {
	if len(body) != 2 {
		return 0, nil, fmt.Errorf("portal response carried %d values, want 2", len(body))
	}
	code, ok := body[0].(uint32)
	if !ok {
		return 0, nil, fmt.Errorf("portal response code is %T, want uint32", body[0])
	}
	results, ok := body[1].(map[string]dbus.Variant)
	if !ok {
		return 0, nil, fmt.Errorf("portal response results are %T, want a dict", body[1])
	}
	return code, results, nil
}

// portalDir maps a response onto selectFolder's contract: 0 with a usable uri
// is a choice, 1 is a clean cancel, anything else is a genuine failure.
func portalDir(code uint32, results map[string]dbus.Variant) (string, bool, error) {
	switch code {
	case 0:
	case 1:
		return "", false, nil
	default:
		return "", false, fmt.Errorf("portal folder picker failed (response %d)", code)
	}
	uris, ok := results["uris"].Value().([]string)
	if !ok || len(uris) == 0 {
		return "", false, errors.New("portal reported success with no uris")
	}
	dir, err := pathFromFileURI(uris[0])
	if err != nil {
		return "", false, err
	}
	return dir, true, nil
}

// pathFromFileURI turns the portal's file:// URI into a local path. Anything
// that is not a local file URI is refused rather than guessed at: the caller
// canonicalizes and grants against this path, so it must never be a remote
// location's path spelled locally.
func pathFromFileURI(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", fmt.Errorf("portal returned an unparseable uri: %w", err)
	}
	if u.Scheme != "file" || (u.Host != "" && u.Host != "localhost") {
		return "", fmt.Errorf("portal returned a non-local uri %q", uri)
	}
	if u.Path == "" {
		return "", fmt.Errorf("portal returned a pathless uri %q", uri)
	}
	return u.Path, nil
}
