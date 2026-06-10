package browser

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindChromiumEnvOverride(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "fake-chrome")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HTMLCLAY_BROWSER", fake)

	result := FindChromium()
	if result != fake {
		t.Errorf("expected %q, got %q", fake, result)
	}
}

func TestFindChromiumEnvOverrideNotExists(t *testing.T) {
	t.Setenv("HTMLCLAY_BROWSER", filepath.Join(t.TempDir(), "nonexistent"))

	result := FindChromium()
	if result != "" && result == os.Getenv("HTMLCLAY_BROWSER") {
		t.Error("should not return nonexistent path")
	}
}

func TestFindChromiumReturnsString(t *testing.T) {
	result := FindChromium()
	_ = result
}
