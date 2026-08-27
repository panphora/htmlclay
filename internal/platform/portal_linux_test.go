//go:build linux

package platform

import (
	"testing"

	"github.com/godbus/dbus/v5"
)

// The response decoding is the part of the portal picker a test can reach
// without a desktop: everything after it is D-Bus plumbing, everything in it
// decides whether a grant lands on the right path. Cancel and failure must
// stay distinguishable, and a malformed success must error rather than grant.
func TestPortalDirMapsEveryOutcome(t *testing.T) {
	uris := func(u ...string) map[string]dbus.Variant {
		return map[string]dbus.Variant{"uris": dbus.MakeVariant(u)}
	}

	if dir, ok, err := portalDir(0, uris("file:///home/me/My%20Project")); err != nil || !ok || dir != "/home/me/My Project" {
		t.Errorf("success = (%q, %v, %v), want the unescaped path", dir, ok, err)
	}
	if dir, ok, err := portalDir(1, nil); err != nil || ok || dir != "" {
		t.Errorf("cancel = (%q, %v, %v), want a clean no-choice", dir, ok, err)
	}
	if _, ok, err := portalDir(2, nil); err == nil || ok {
		t.Error("a portal failure must surface as an error, not read as a cancel")
	}
	if _, ok, err := portalDir(0, map[string]dbus.Variant{}); err == nil || ok {
		t.Error("success with no uris must error, never grant")
	}
	if _, ok, err := portalDir(0, uris("https://example.com/dir")); err == nil || ok {
		t.Error("a non-local uri must be refused; the grant would land on a remote location's local spelling")
	}
	if _, ok, err := portalDir(0, uris("file://otherhost/srv/dir")); err == nil || ok {
		t.Error("a foreign-host file uri must be refused")
	}
}

func TestPortalResponseShape(t *testing.T) {
	if _, _, err := portalResponse([]interface{}{uint32(0), map[string]dbus.Variant{}}); err != nil {
		t.Errorf("a well-formed body must decode: %v", err)
	}
	for _, body := range [][]interface{}{
		{},
		{uint32(0)},
		{"0", map[string]dbus.Variant{}},
		{uint32(0), "not a dict"},
	} {
		if _, _, err := portalResponse(body); err == nil {
			t.Errorf("malformed body %v must error", body)
		}
	}
}
