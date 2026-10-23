// Package tailwind compiles a document's Tailwind stylesheet with Tailwind's
// standalone CLI, the way tailwind-hyperclay does on hyperclay.com and in
// Hyperclay Local.
package tailwind

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Version is the Tailwind release every build pins.
const Version = "4.1.17"

// Timeout bounds one compile.
const Timeout = 30 * time.Second

//go:embed ephemeral.js
var ephemeralPlugin []byte

// The candidates come from a file we write, never from scanning the user's
// folders: source(none) turns Tailwind's own detection off.
const inputCSS = `@import "tailwindcss" source(none);
@source "./candidates.txt";
@plugin "@tailwindcss/typography";
@plugin "@tailwindcss/forms" { strategy: "class"; }
@plugin "./ephemeral.js";
`

var defaultCandidates = []string{
	"hidden", "block", "flex",
	"invisible", "visible",
	"sr-only", "not-sr-only",
	"fixed", "absolute",
}

var classAttr = regexp.MustCompile(`(?i)class\s*=\s*["']([^"']+)["']`)

// ExtractCandidates matches tailwind-hyperclay's extractCandidates: every
// whitespace-separated token inside a class="..." value, first-seen order,
// no duplicates.
func ExtractCandidates(html []byte) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range classAttr.FindAllSubmatch(html, -1) {
		for _, c := range strings.Fields(string(m[1])) {
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	return out
}

// Candidates is what one compile builds: the defaults, then the document's.
func Candidates(html []byte) []string {
	return append(append([]string{}, defaultCandidates...), ExtractCandidates(html)...)
}

// Compile runs the Tailwind CLI at bin over html and returns the stylesheet.
func Compile(ctx context.Context, bin string, html []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	dir, err := os.MkdirTemp("", "htmlclay-tailwind-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	files := map[string][]byte{
		"in.css":         []byte(inputCSS),
		"candidates.txt": []byte(strings.Join(Candidates(html), "\n") + "\n"),
		"ephemeral.js":   ephemeralPlugin,
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			return nil, err
		}
	}

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, "-i", "in.css", "-o", "out.css")
	cmd.Dir = dir
	cmd.Stderr = &stderr
	cmd.SysProcAttr = sysProcAttr()
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("tailwind: compile timed out after %s", Timeout)
		}
		return nil, fmt.Errorf("tailwind: %w: %s", err, tail(stderr.String(), 2000))
	}
	return os.ReadFile(filepath.Join(dir, "out.css"))
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
