package tailwind

import (
	"context"
	"errors"
	"regexp"
	"testing"
)

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

func TestFingerprint(t *testing.T) {
	a := Fingerprint([]byte(`<html><body class="p-4">one</body></html>`))
	b := Fingerprint([]byte(`<html><body class="p-4">one</body></html>`))
	// A text-only edit leaves the candidate list alone, so the stylesheet is the
	// same and so is the fingerprint.
	sameClasses := Fingerprint([]byte(`<html><body class="p-4">two</body></html>`))
	c := Fingerprint([]byte(`<html><body class="m-2">one</body></html>`))

	if a != b {
		t.Fatalf("same input gave two fingerprints: %q and %q", a, b)
	}
	if a != sameClasses {
		t.Fatalf("same classes in different text gave two fingerprints: %q and %q", a, sameClasses)
	}
	if a == c {
		t.Fatalf("different classes gave the same fingerprint: %q", a)
	}
	if !hex64.MatchString(a) {
		t.Fatalf("fingerprint is not 64 lowercase hex characters: %q", a)
	}

	t.Setenv("HTMLCLAY_TAILWIND_BIN", "")
	_, err := CompileDocument(context.Background(), t.TempDir(), []byte(`<html></html>`))
	if Bundled() {
		if err != nil {
			t.Fatalf("CompileDocument with a bundled binary = %v, want no error", err)
		}
	} else if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("CompileDocument with no binary = %v, want an error wrapping ErrUnavailable", err)
	}
}
