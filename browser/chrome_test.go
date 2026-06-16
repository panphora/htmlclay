package browser

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
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

func TestChromeCommandsFromBrowserEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("$BROWSER list convention is Unix-only")
	}
	cases := []struct {
		env  string
		want []string
	}{
		{"chromium %s:firefox %s", []string{"chromium"}},
		{"firefox:google-chrome-stable", []string{"google-chrome-stable"}},
		{"chromium --incognito %s:microsoft-edge", []string{"chromium", "microsoft-edge"}},
		{"firefox", nil},
		{"google-chrome", []string{"google-chrome"}},
		{"/usr/bin/chromium %s", []string{"/usr/bin/chromium"}},
	}
	for _, c := range cases {
		if got := chromeCommandsFromBrowserEnv(c.env); !slices.Equal(got, c.want) {
			t.Errorf("chromeCommandsFromBrowserEnv(%q) = %v, want %v", c.env, got, c.want)
		}
	}
}

func TestFindFromBrowserEnvPicksChromeFromList(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("$BROWSER list convention is Unix-only")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "my-chromium")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BROWSER", "firefox %s:my-chromium %s")

	if got := findFromBrowserEnv(); got != fake {
		t.Errorf("findFromBrowserEnv() = %q, want %q (should skip firefox, pick the chromium entry)", got, fake)
	}
}
