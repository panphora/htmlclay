// Command helper stands in for an htmlclay binary in the update tests. The tests
// build it several times with a different version and mode linked in, so the old
// and new binaries differ in content as well as in behavior.
package main

import (
	"os"
	"path/filepath"
	"time"

	"github.com/panphora/htmlclay/internal/update"
)

var (
	version = "0.0.0"
	// mode is serve (reports started and serving), crash (exits 1), hang
	// (reports started, never serving) or idle (only records the launch).
	mode = "idle"
)

func main() {
	dir := os.Getenv("HTMLCLAY_TEST_UPDATES_DIR")
	if len(os.Args) > 1 && os.Args[1] == "--finish-update" {
		if err := update.FinishUpdate(dir, update.SupervisorOptions{}); err != nil {
			os.Exit(1)
		}
		return
	}
	os.WriteFile(filepath.Join(os.Getenv("HTMLCLAY_TEST_MARKERS"), "launched-"+version), []byte(mode), 0o644)
	switch mode {
	case "serve":
		update.MarkHealth(dir, version, update.HealthStarted)
		update.MarkHealth(dir, version, update.HealthServing)
		time.Sleep(300 * time.Millisecond)
	case "crash":
		os.Exit(1)
	case "hang":
		update.MarkHealth(dir, version, update.HealthStarted)
		time.Sleep(60 * time.Second)
	}
}
