package tailwind

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
)

// ErrUnavailable means this build carries no Tailwind binary.
var ErrUnavailable = errors.New("tailwind: no compiler in this build")

// Locate returns the path of a runnable Tailwind binary, extracting the bundled
// one under root on first use. A build without the bundle answers
// HTMLCLAY_TAILWIND_BIN when it is set (development only) and ErrUnavailable
// otherwise; a bundled build never reads that variable.
var Locate = func(root string) (string, error) {
	if bundled == nil {
		if p := os.Getenv("HTMLCLAY_TAILWIND_BIN"); p != "" {
			return p, nil
		}
		return "", ErrUnavailable
	}
	if root == "" {
		return "", ErrUnavailable
	}
	sum, size, err := parseSum(bundledSum)
	if err != nil {
		return "", err
	}
	return install(root, bundled, sum, size)
}

// Bundled reports whether this build carries the Tailwind binary.
func Bundled() bool { return bundled != nil }

// CompileDocument compiles html with the binary Locate finds under root.
func CompileDocument(ctx context.Context, root string, html []byte) ([]byte, error) {
	bin, err := Locate(root)
	if err != nil {
		return nil, err
	}
	return Compile(ctx, bin, html)
}

// Fingerprint names one compile's output: the compiler, the bundled compiler's
// checksum, its input CSS and plugin, and the candidate list the document
// yields. Equal fingerprints mean equal stylesheets.
func Fingerprint(html []byte) string {
	h := sha256.New()
	h.Write([]byte(Version))
	h.Write([]byte{0})
	h.Write([]byte(bundledSum))
	h.Write([]byte{0})
	h.Write([]byte(inputCSS))
	h.Write([]byte{0})
	h.Write(ephemeralPlugin)
	h.Write([]byte{0})
	h.Write([]byte(strings.Join(Candidates(html), "\n")))
	return hex.EncodeToString(h.Sum(nil))
}
