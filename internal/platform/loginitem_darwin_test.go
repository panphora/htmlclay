//go:build darwin

package platform

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetLoginItemWritesValidPlist(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	execPath := "/Applications/Tom & Jerry's <Clay>.app/Contents/MacOS/htmlclay"

	if err := SetLoginItem(true, execPath); err != nil {
		t.Fatal(err)
	}
	path, err := launchAgentPath()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "<?xml") {
		t.Fatalf("plist does not start with an XML declaration: %q", data[:20])
	}

	out, err := exec.Command("plutil", "-extract", "ProgramArguments.0", "raw", path).CombinedOutput()
	if err != nil {
		t.Fatalf("plutil rejected the plist: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != execPath {
		t.Fatalf("ProgramArguments.0 = %q, want %q", got, execPath)
	}
	if filepath.Base(path) != "com.htmlclay.plist" {
		t.Fatalf("unexpected plist name %s", path)
	}
}
