//go:build linux

package platform

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"strings"

	"github.com/godbus/dbus/v5"
)

// The portal stays first so GNOME, KDE, Wayland, and Flatpak receive the
// desktop's own picker without requiring a helper binary. Once it may have
// opened a dialog, an error is surfaced instead of stacking a fallback picker.
func selectFile(prompt string) (string, bool, error) {
	path, ok, err := portalSelectFile(prompt)
	if err == nil {
		return path, ok, nil
	}
	if !errors.Is(err, errPortalUnavailable) {
		return "", false, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), selectFileTimeout)
	defer cancel()

	if bin, err := exec.LookPath("zenity"); err == nil {
		out, err := exec.CommandContext(ctx, bin, "--file-selection", "--title", prompt).Output()
		if err != nil {
			if cancelled(err) {
				return "", false, nil
			}
			return "", false, fmt.Errorf("zenity file picker failed: %w", err)
		}
		if path := pickedFile(out); path != "" {
			return path, true, nil
		}
		return "", false, nil
	}

	if bin, err := exec.LookPath("kdialog"); err == nil {
		home, _ := os.UserHomeDir()
		out, err := exec.CommandContext(ctx, bin, "--title", prompt, "--getopenfilename", home).Output()
		if err != nil {
			if cancelled(err) {
				return "", false, nil
			}
			return "", false, fmt.Errorf("kdialog file picker failed: %w", err)
		}
		if path := pickedFile(out); path != "" {
			return path, true, nil
		}
		return "", false, nil
	}

	return "", false, errors.New("no native file picker found (no desktop portal, and neither zenity nor kdialog is installed)")
}

func pickedFile(out []byte) string {
	return strings.Trim(string(out), "\n")
}

// This mirrors portalSelectFolder without its directory option. The folder
// implementation cannot be reused because an older portal ignores an unknown
// directory option and would silently widen a folder request into a file pick.
func portalSelectFile(prompt string) (string, bool, error) {
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
	if !ok || version < 1 {
		return "", false, fmt.Errorf("%w: invalid FileChooser version %v", errPortalUnavailable, v.Value())
	}

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
	}
	ctx, cancel := context.WithTimeout(context.Background(), selectFileTimeout)
	defer cancel()

	var handle dbus.ObjectPath
	if err := obj.CallWithContext(ctx, fileChooserIface+".OpenFile", 0, "", prompt, options).Store(&handle); err != nil {
		if ctx.Err() != nil {
			return "", false, errors.New("portal file picker timed out opening the dialog")
		}
		return "", false, fmt.Errorf("%w: %v", errPortalUnavailable, err)
	}
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
			return portalFile(code, results)
		case <-ctx.Done():
			closeCtx, cancelClose := context.WithTimeout(context.Background(), portalProbeTimeout)
			conn.Object(portalBusName, handle).CallWithContext(closeCtx, requestIface+".Close", 0)
			cancelClose()
			return "", false, errors.New("portal file picker timed out")
		}
	}
}

func portalFile(code uint32, results map[string]dbus.Variant) (string, bool, error) {
	switch code {
	case 0:
	case 1:
		return "", false, nil
	default:
		return "", false, fmt.Errorf("portal file picker failed (response %d)", code)
	}
	uriList, exists := results["uris"]
	if !exists {
		return "", false, errors.New("portal reported success with no uris")
	}
	uris, ok := uriList.Value().([]string)
	if !ok || len(uris) == 0 {
		return "", false, errors.New("portal reported success with no uris")
	}
	path, err := pathFromFileURI(uris[0])
	if err != nil {
		return "", false, err
	}
	return path, true, nil
}
