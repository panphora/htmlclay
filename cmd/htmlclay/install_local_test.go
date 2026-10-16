//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// installFakes stand in for every command install-local.sh reaches outside
// the fixture folder: curl, pkill, hdiutil and open never run for real. mv and
// ditto run for real but can stop at a step (FAKE_PAUSE) so the test can kill
// the script there, and like rm they refuse any path under /Applications.
var installFakes = map[string]string{
	"curl": `#!/bin/bash
[ "$FAKE_PAUSE" = curl ] && { touch "$W/paused"; exec sleep 120; }
[ -n "$FAKE_CURL_CODE" ] && exit "$FAKE_CURL_CODE"
while [ $# -gt 0 ]; do [ "$1" = -o ] && { echo dmg > "$2"; shift; }; shift; done
exit 0
`,
	"pkill": `#!/bin/bash
echo "pkill $*" >> "$W/calls"
exit 1
`,
	"open": `#!/bin/bash
echo "open $*" >> "$W/calls"
`,
	"hdiutil": `#!/bin/bash
echo "hdiutil $1" >> "$W/calls"
[ "$1" = attach ] && printf '/dev/disk99s1\tApple_HFS\t/Volumes/..%s\n' "$W/volume"
exit 0
`,
	"ditto": `#!/bin/bash
for a in "$@"; do case "$a" in /Applications*) echo "test touched $a" >&2; exit 99 ;; esac; done
if [ "$FAKE_PAUSE" = ditto ]; then
  mkdir -p "$2/Contents" && cp "$1/Contents/Info.plist" "$2/Contents/Info.plist"
  touch "$W/paused"; exec sleep 120
fi
exec /usr/bin/ditto "$@"
`,
	"mv": `#!/bin/bash
for a in "$@"; do case "$a" in /Applications*) echo "test touched $a" >&2; exit 99 ;; esac; done
echo x >> "$W/mvs"
/bin/mv "$@" || exit
n=$(wc -l < "$W/mvs" | tr -d ' ')
[ "$FAKE_PAUSE" = "mv$n" ] && { touch "$W/paused"; exec sleep 120; }
exit 0
`,
	"rm": `#!/bin/bash
for a in "$@"; do case "$a" in /Applications*) echo "test touched $a" >&2; exit 99 ;; esac; done
exec %q "$@"
`,
}

type installWork struct {
	dir, apps, script, path string
}

func newInstallWork(t *testing.T) installWork {
	t.Helper()
	script, err := filepath.Abs("../../scripts/install-local.sh")
	if err != nil {
		t.Fatal(err)
	}
	realRm, err := exec.LookPath("rm")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	w := installWork{dir: dir, apps: filepath.Join(dir, "apps"), script: script}
	bin := filepath.Join(dir, "bin")
	for _, d := range []string{bin, w.apps} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range installFakes {
		if name == "rm" {
			body = fmt.Sprintf(body, realRm)
		}
		writeCanaryFile(t, filepath.Join(bin, name), body, 0o755)
	}
	w.path = bin + ":/usr/bin:/bin"
	writeInstallApp(t, filepath.Join(dir, "volume", "HTMLClay.app"), "2.0.0")
	return w
}

// writeInstallApp makes a fake bundle. A complete one has its plist, its
// binary and a payload file that names the same version.
func writeInstallApp(t *testing.T, app, version string) {
	t.Helper()
	for _, d := range []string{"Contents/MacOS", "Contents/Resources"} {
		if err := os.MkdirAll(filepath.Join(app, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeCanaryFile(t, filepath.Join(app, "Contents/Info.plist"), fmt.Sprintf(canaryPlist, version, ""), 0o644)
	writeCanaryFile(t, filepath.Join(app, "Contents/MacOS/htmlclay"), "#!/bin/sh\n", 0o755)
	writeCanaryFile(t, filepath.Join(app, "Contents/Resources/payload"), version, 0o644)
}

func (w installWork) start(t *testing.T, env ...string) (*exec.Cmd, chan error) {
	t.Helper()
	os.Remove(filepath.Join(w.dir, "paused"))
	os.Remove(filepath.Join(w.dir, "mvs"))
	cmd := exec.Command("bash", w.script, "2.0.0")
	cmd.Env = append([]string{"PATH=" + w.path, "HOME=" + w.dir, "TMPDIR=" + w.dir, "W=" + w.dir, "HTMLCLAY_INSTALL_APPS_DIR=" + w.apps}, env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if t.Failed() {
			t.Logf("install-local.sh output:\n%s", out.String())
		}
	})
	return cmd, done
}

func (w installWork) run(t *testing.T, env ...string) int {
	t.Helper()
	cmd, done := w.start(t, env...)
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("install-local.sh did not finish in 20 s")
	}
	return cmd.ProcessState.ExitCode()
}

// installedVersion reports the version of the complete app at path, or "" when
// the bundle is missing or only partly there.
func installedVersion(path string) string {
	plist, err := exec.Command("plutil", "-extract", "CFBundleShortVersionString", "raw", "-o", "-", filepath.Join(path, "Contents/Info.plist")).Output()
	if err != nil {
		return ""
	}
	version := strings.TrimSpace(string(plist))
	payload, err := os.ReadFile(filepath.Join(path, "Contents/Resources/payload"))
	if err != nil || string(payload) != version {
		return ""
	}
	if _, err := os.Stat(filepath.Join(path, "Contents/MacOS/htmlclay")); err != nil {
		return ""
	}
	return version
}

func (w installWork) leftovers(t *testing.T) []string {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(w.apps, ".HTMLClay.app.*"))
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func (w installWork) expect(t *testing.T, version string) {
	t.Helper()
	if got := installedVersion(filepath.Join(w.apps, "HTMLClay.app")); got != version {
		t.Errorf("complete app at the destination: %q, want %q", got, version)
	}
	if left := w.leftovers(t); len(left) != 0 {
		t.Errorf("staging or aside copies left behind: %v", left)
	}
}

func TestInstallLocalSwap(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("install-local.sh runs on macOS only and reads plists with plutil")
	}

	t.Run("installs and relaunches", func(t *testing.T) {
		w := newInstallWork(t)
		writeInstallApp(t, filepath.Join(w.apps, "HTMLClay.app"), "1.0.0")
		if code := w.run(t); code != 0 {
			t.Fatalf("exit %d, want 0", code)
		}
		w.expect(t, "2.0.0")
		calls, _ := os.ReadFile(filepath.Join(w.dir, "calls"))
		if !strings.Contains(string(calls), "open -a "+filepath.Join(w.apps, "HTMLClay.app")) {
			t.Errorf("not relaunched from the destination; calls:\n%s", calls)
		}
	})

	t.Run("installs where nothing was installed", func(t *testing.T) {
		w := newInstallWork(t)
		if code := w.run(t); code != 0 {
			t.Fatalf("exit %d, want 0", code)
		}
		w.expect(t, "2.0.0")
	})

	t.Run("cleans up an earlier run's copies", func(t *testing.T) {
		w := newInstallWork(t)
		writeInstallApp(t, filepath.Join(w.apps, "HTMLClay.app"), "1.0.0")
		writeInstallApp(t, filepath.Join(w.apps, ".HTMLClay.app.new-99999"), "2.0.0")
		writeInstallApp(t, filepath.Join(w.apps, ".HTMLClay.app.old-99998"), "0.9.0")
		if code := w.run(t, "FAKE_CURL_CODE=22"); code == 0 {
			t.Fatal("exit 0 with the download failing")
		}
		w.expect(t, "1.0.0")
	})

	// For each step: where TERM leaves the destination, and where the next run
	// (whose download fails, so it only recovers) leaves it after a KILL there.
	steps := []struct {
		pause, afterTerm, afterKill string
	}{
		{"curl", "1.0.0", "1.0.0"},
		{"ditto", "1.0.0", "1.0.0"},
		{"mv1", "2.0.0", "1.0.0"},
		{"mv2", "2.0.0", "2.0.0"},
	}
	for _, step := range steps {
		for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
			step, sig := step, sig
			t.Run(fmt.Sprintf("%v at %s", sig, step.pause), func(t *testing.T) {
				w := newInstallWork(t)
				writeInstallApp(t, filepath.Join(w.apps, "HTMLClay.app"), "1.0.0")
				cmd, done := w.start(t, "FAKE_PAUSE="+step.pause)
				paused := filepath.Join(w.dir, "paused")
				deadline := time.Now().Add(60 * time.Second)
				for {
					if _, err := os.Stat(paused); err == nil {
						break
					}
					select {
					case <-done:
						t.Fatalf("install-local.sh exited before reaching %s", step.pause)
					default:
					}
					if time.Now().After(deadline) {
						t.Fatalf("install-local.sh never reached %s", step.pause)
					}
					time.Sleep(20 * time.Millisecond)
				}
				// hypersave stops the canary this way: the signal goes to the
				// whole process group.
				syscall.Kill(-cmd.Process.Pid, sig)
				select {
				case <-done:
				case <-time.After(60 * time.Second):
					t.Fatalf("install-local.sh did not stop after %v", sig)
				}
				if sig == syscall.SIGTERM {
					w.expect(t, step.afterTerm)
					return
				}
				dest := installedVersion(filepath.Join(w.apps, "HTMLClay.app"))
				aside, _ := filepath.Glob(filepath.Join(w.apps, ".HTMLClay.app.old-*"))
				if dest == "" && (len(aside) != 1 || installedVersion(aside[0]) == "") {
					t.Fatalf("no complete app at the destination or aside after KILL; aside copies %v", aside)
				}
				if code := w.run(t, "FAKE_CURL_CODE=22"); code == 0 {
					t.Fatal("next run exited 0 with the download failing")
				}
				w.expect(t, step.afterKill)
			})
		}
	}
}
