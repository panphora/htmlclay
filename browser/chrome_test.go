package browser

import (
	"os"
	"testing"
)

func TestFindChromiumEnvOverride(t *testing.T) {
	fake := "/tmp/malleable-test-fake-chrome"
	os.WriteFile(fake, []byte("#!/bin/sh\n"), 0755)
	defer os.Remove(fake)

	os.Setenv("HTMLCLAY_BROWSER", fake)
	defer os.Unsetenv("HTMLCLAY_BROWSER")

	result := FindChromium()
	if result != fake {
		t.Errorf("expected %q, got %q", fake, result)
	}
}

func TestFindChromiumEnvOverrideNotExists(t *testing.T) {
	os.Setenv("HTMLCLAY_BROWSER", "/nonexistent/browser")
	defer os.Unsetenv("HTMLCLAY_BROWSER")

	result := FindChromium()
	if result == "/nonexistent/browser" {
		t.Error("should not return nonexistent path")
	}
}

func TestFindChromiumReturnsString(t *testing.T) {
	result := FindChromium()
	_ = result
}
