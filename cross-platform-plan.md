# Cross-Platform Plan: Windows + Linux Support

## How To Use This Plan

Work through each phase in order — each builds on the previous. Every step has an
exact verification command. Do not move to the next step until the verification passes.

If a step says "create file", give the file exactly the content shown. If it says
"edit file", only change the lines called out.

---

## Phase 1: Dependency Modernization

### 1.1 — Replace `getlantern/systray` with `fyne.io/systray`

**Why:** `getlantern/systray` v1.2.2 is unmaintained (last commit 2022) and pulls in
8 transitive dependencies from the getlantern ecosystem. `fyne.io/systray` is the
actively maintained fork by the Fyne team. It's a direct fork with the exact same API
— same function names, same `ClickedCh` channel on `MenuItem`, same everything. Only
the import path changes.

**Step 1 — Edit `tray/tray.go` line 8:**

Change:
```go
"github.com/getlantern/systray"
```
To:
```go
"fyne.io/systray"
```

No other code changes. Every function call, type reference, and channel usage stays
identical because `fyne.io/systray` is API-compatible.

**Step 2 — Update Go module:**

```bash
# Remove the old dependency and add the new one
go get -u fyne.io/systray
go mod tidy
```

After `go mod tidy`, the `go.mod` file should:
- Have `fyne.io/systray` in the `require` block
- NOT have `github.com/getlantern/systray` anywhere
- NOT have the getlantern transitive deps (context, errors, golog, hex, hidden, ops)

**Step 3 — Verify:**

```bash
go build ./...
go test ./...
```

Both must pass with zero errors. The tray test (`tray/tray_test.go`) validates the
PNG icon embedding still works.

**Manual check:** Run `go build -o htmlclay . && ./htmlclay --no-tray` on macOS. The
app should start and print `[htmlclay] Starting up...`. Ctrl+C to quit.

---

### 1.2 — Bump Go minimum to 1.23

**Why:** Go 1.22 is past its support window. 1.23 is the oldest currently supported
release. It gives us better stdlib support and keeps us on a version that receives
security patches.

**Edit `go.mod` line 3:**

Change:
```
go 1.22.2
```
To:
```
go 1.23
```

**Verify:**

```bash
go build ./...
go test ./...
```

---

## Phase 2: Platform-Correct Paths

### 2.1 — Use OS-native config directory

**Why:** Right now the config lives at `~/.htmlclay` on all platforms. This is wrong:
- **Windows** expects `%APPDATA%\HTMLClay` (e.g., `C:\Users\You\AppData\Roaming\HTMLClay`)
- **Linux** expects `$XDG_CONFIG_HOME/htmlclay` (defaults to `~/.config/htmlclay`)
- **macOS** should use `~/Library/Application Support/htmlclay` (but `~/.htmlclay`
  exists for current users, so we migrate)

Go's `os.UserConfigDir()` returns the right base on each platform:
- macOS: `~/Library/Application Support`
- Linux: `~/.config`
- Windows: `C:\Users\You\AppData\Roaming`

We append `htmlclay` to that.

#### Step 1 — Edit `config/config.go`

Replace the `defaultBaseDir` function and `Dir` function. The key change: instead of
`os.UserHomeDir()` + `.htmlclay`, we use `os.UserConfigDir()` + `htmlclay`.

**Replace lines 18-32 (the `defaultBaseDir` and `Dir` functions) with:**

```go
func defaultConfigDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine config directory: %w", err)
	}
	return base, nil
}

func Dir() (string, error) {
	base, err := defaultConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "htmlclay"), nil
}
```

**Replace line 34-36 (`DirFrom`) — remove the dot prefix:**

```go
func DirFrom(baseDir string) string {
	return filepath.Join(baseDir, "htmlclay")
}
```

**Replace lines 54-60 (`Load`) to use the new function name:**

```go
func Load() (*Config, error) {
	base, err := defaultConfigDir()
	if err != nil {
		return nil, err
	}
	return LoadFrom(base)
}
```

Everything else in `config.go` stays the same. `LoadFrom`, `Save`, `ResolvePort`,
`EnsureDir`, `Path` — they all go through `Dir()` or `DirFrom()`, so they
automatically pick up the new path.

#### Step 2 — Add migration for existing macOS users

Existing macOS users have their config at `~/.htmlclay`. On first run after this
change, we move it to the new location.

**Edit `main.go` — add this function anywhere after the imports:**

```go
func migrateConfigDir() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	oldDir := filepath.Join(home, ".htmlclay")

	newDir, err := config.Dir()
	if err != nil {
		return
	}

	if oldDir == newDir {
		return
	}

	oldInfo, err := os.Stat(oldDir)
	if err != nil || !oldInfo.IsDir() {
		return
	}

	if _, err := os.Stat(newDir); err == nil {
		return
	}

	if err := os.MkdirAll(filepath.Dir(newDir), 0755); err != nil {
		return
	}

	if err := os.Rename(oldDir, newDir); err != nil {
		fmt.Fprintf(os.Stderr, "[htmlclay] Could not migrate config from %s to %s: %v\n", oldDir, newDir, err)
	} else {
		fmt.Fprintf(os.Stderr, "[htmlclay] Migrated config from %s to %s\n", oldDir, newDir)
	}
}
```

**Then in `main()`, call it as the very first thing — add one line after `flag.Parse()`
(after line 54, before the "Starting up" print):**

```go
migrateConfigDir()
```

**How this works:**
1. Checks if old `~/.htmlclay` directory exists
2. Checks if new directory (e.g., `~/Library/Application Support/htmlclay`) already exists
3. If old exists and new doesn't, moves old → new via `os.Rename`
4. If either check fails, silently does nothing (safe fallback)

`os.Rename` is atomic on the same filesystem, so this won't leave a partial state.
On Windows and Linux there is no old directory, so the function returns immediately
at the `os.Stat(oldDir)` check.

#### Step 3 — Update tests

The tests in `config/config_test.go` use `LoadFrom(t.TempDir())` and `DirFrom()`.
These call `DirFrom` which now joins with `htmlclay` instead of `.htmlclay`. The tests
don't hardcode the directory name, so **no test changes are needed**. Run them to
verify:

```bash
go test ./config/ -v
```

All 5 tests should pass.

#### Step 4 — Full verification:

```bash
go test ./... -count=1
```

---

### 2.2 — Add Windows Chrome/Edge detection

**Why:** On Windows, Chrome and Edge are installed in `Program Files` and
`%LOCALAPPDATA%`, not on PATH. The current `FindChromium()` only searches PATH and
macOS app bundles, so it will never find Chrome on a default Windows install. Without
this, App Mode silently falls back to the default browser.

**Edit `browser/chrome.go` — add a new path list and finder function.**

Add this variable after the existing `macOSChromePaths` (after line 15):

```go
var windowsChromePaths = []string{
	filepath.Join(os.Getenv("PROGRAMFILES"), "Google", "Chrome", "Application", "chrome.exe"),
	filepath.Join(os.Getenv("PROGRAMFILES(X86)"), "Google", "Chrome", "Application", "chrome.exe"),
	filepath.Join(os.Getenv("LOCALAPPDATA"), "Google", "Chrome", "Application", "chrome.exe"),
	filepath.Join(os.Getenv("PROGRAMFILES"), "Microsoft", "Edge", "Application", "msedge.exe"),
	filepath.Join(os.Getenv("PROGRAMFILES(X86)"), "Microsoft", "Edge", "Application", "msedge.exe"),
}
```

Add this import at the top (it's already imported, but make sure `path/filepath` is
there):

```go
"path/filepath"
```

Add this function after `findFromMacOSPaths` (after line 86):

```go
func findFromWindowsPaths() string {
	for _, path := range windowsChromePaths {
		if path == "" {
			continue
		}
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}
```

The `path == ""` guard handles the case where an environment variable is empty (e.g.,
`PROGRAMFILES(X86)` is empty on ARM Windows). `filepath.Join("", ...)` would produce a
relative path, which `os.Stat` would still check but would never match a real Chrome
install.

**Then edit the `FindChromium()` function to include it — add a Windows block after
the darwin block (after line 33):**

```go
	if runtime.GOOS == "windows" {
		finders = append(finders, findFromWindowsPaths)
	}
```

The full `FindChromium` function should now look like:

```go
func FindChromium() string {
	finders := []func() string{
		findFromEnv,
		findFromBrowserEnv,
		findFromPath,
	}
	if runtime.GOOS == "darwin" {
		finders = append(finders, findFromMacOSPaths)
	}
	if runtime.GOOS == "windows" {
		finders = append(finders, findFromWindowsPaths)
	}
	for _, fn := range finders {
		if path := fn(); path != "" {
			return path
		}
	}
	return ""
}
```

**Verify:**

```bash
go test ./browser/ -v
go build ./...
GOOS=windows GOARCH=amd64 go build ./browser/
```

The last command verifies the Windows code compiles. You can't test the actual path
detection without a Windows machine, but the compilation check catches import or
syntax errors.

---

### 2.3 — Add Linux Chrome detection paths

**Why:** On Linux, Chrome is usually on PATH (handled by `findFromPath`), but
snap/flatpak installs put binaries in non-PATH locations.

**Edit `browser/chrome.go` — add after `windowsChromePaths`:**

```go
var linuxChromePaths = []string{
	"/snap/bin/chromium",
	"/snap/bin/google-chrome",
}
```

**Add the finder function after `findFromWindowsPaths`:**

```go
func findFromLinuxPaths() string {
	for _, path := range linuxChromePaths {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}
```

**Edit `FindChromium()` — add a Linux block after the Windows block:**

```go
	if runtime.GOOS == "linux" {
		finders = append(finders, findFromLinuxPaths)
	}
```

**Verify:**

```bash
go test ./browser/ -v
GOOS=linux GOARCH=amd64 go build ./browser/
```

---

## Phase 3: Build & Release Pipeline

### 3.1 — GitHub Actions CI

**Why:** Every push and PR should be tested on all three platforms. CGO is required
(for the systray library), so we use native runners for each OS rather than
cross-compiling.

**Create `.github/workflows/ci.yml`:**

```yaml
name: CI

on:
  push:
    branches: [main]
  pull_request:
    branches: [main]

jobs:
  test:
    strategy:
      matrix:
        os: [macos-latest, ubuntu-latest, windows-latest]
    runs-on: ${{ matrix.os }}

    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version: "1.23"

      # Linux needs GTK and appindicator headers for the systray CGO build.
      # macOS and Windows have the needed system libraries pre-installed.
      - name: Install Linux dependencies
        if: runner.os == 'Linux'
        run: |
          sudo apt-get update
          sudo apt-get install -y gcc libayatana-appindicator3-dev libgtk-3-dev

      - name: Test
        run: go test ./... -count=1

      - name: Build
        run: go build -o htmlclay .
        env:
          CGO_ENABLED: "1"
```

**What each part does:**
- `strategy.matrix.os` — runs the job three times, once per OS
- `actions/setup-go@v5` — installs Go 1.23 on the runner
- The Linux `apt-get` step — installs C headers that the systray library needs.
  Without this, `go build` fails on Linux with "undefined: nativeLoop" errors.
  `libayatana-appindicator3-dev` is the system tray library, `libgtk-3-dev` is its
  dependency. macOS uses Cocoa (built in) and Windows uses Win32 (built in), so they
  don't need extra packages.
- `CGO_ENABLED: "1"` — ensures the C compiler is invoked. This is the default on
  native builds but being explicit prevents surprises.

**Verify:** Push a branch and check the Actions tab on GitHub. All three jobs should
pass green.

---

### 3.2 — GitHub Actions Release

**Why:** When you tag a release (e.g., `git tag v1.1`), CI should automatically build
binaries for all platforms and create a GitHub Release with downloadable archives.

**Create `.github/workflows/release.yml`:**

```yaml
name: Release

on:
  push:
    tags: ["v*"]

permissions:
  contents: write

jobs:
  build:
    strategy:
      matrix:
        include:
          - os: macos-latest
            goos: darwin
            goarch: amd64
            artifact: htmlclay-darwin-amd64.tar.gz
          - os: macos-latest
            goos: darwin
            goarch: arm64
            artifact: htmlclay-darwin-arm64.tar.gz
          - os: ubuntu-latest
            goos: linux
            goarch: amd64
            artifact: htmlclay-linux-amd64.tar.gz
          - os: ubuntu-latest
            goos: linux
            goarch: arm64
            artifact: htmlclay-linux-arm64.tar.gz
          - os: windows-latest
            goos: windows
            goarch: amd64
            artifact: htmlclay-windows-amd64.zip
    runs-on: ${{ matrix.os }}

    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version: "1.23"

      - name: Install Linux dependencies
        if: runner.os == 'Linux'
        run: |
          sudo apt-get update
          sudo apt-get install -y gcc libayatana-appindicator3-dev libgtk-3-dev

      # For Linux ARM64, we need a cross-compiler since the runner is x86_64.
      # Install the arm64 cross-compilation toolchain.
      - name: Install Linux ARM64 cross-compiler
        if: matrix.goos == 'linux' && matrix.goarch == 'arm64'
        run: |
          sudo apt-get install -y gcc-aarch64-linux-gnu
          sudo dpkg --add-architecture arm64
          sudo sed -i 's/^deb /deb [arch=amd64] /' /etc/apt/sources.list
          echo "deb [arch=arm64] http://ports.ubuntu.com/ $(lsb_release -cs) main restricted universe" | sudo tee /etc/apt/sources.list.d/arm64.list
          sudo apt-get update
          sudo apt-get install -y libayatana-appindicator3-dev:arm64 libgtk-3-dev:arm64

      - name: Build
        run: go build -trimpath -ldflags="-s -w" -o htmlclay${{ matrix.goos == 'windows' && '.exe' || '' }} .
        env:
          CGO_ENABLED: "1"
          GOOS: ${{ matrix.goos }}
          GOARCH: ${{ matrix.goarch }}
          CC: ${{ matrix.goos == 'linux' && matrix.goarch == 'arm64' && 'aarch64-linux-gnu-gcc' || '' }}

      # tar.gz for macOS/Linux, zip for Windows
      - name: Package (tar.gz)
        if: matrix.goos != 'windows'
        run: tar czf ${{ matrix.artifact }} htmlclay

      - name: Package (zip)
        if: matrix.goos == 'windows'
        run: Compress-Archive -Path htmlclay.exe -DestinationPath ${{ matrix.artifact }}

      - uses: actions/upload-artifact@v4
        with:
          name: ${{ matrix.artifact }}
          path: ${{ matrix.artifact }}

  release:
    needs: build
    runs-on: ubuntu-latest
    steps:
      - uses: actions/download-artifact@v4
        with:
          path: artifacts
          merge-multiple: true

      - name: Create GitHub Release
        env:
          GH_TOKEN: ${{ github.token }}
        run: |
          gh release create ${{ github.ref_name }} \
            artifacts/* \
            --repo ${{ github.repository }} \
            --title "${{ github.ref_name }}" \
            --generate-notes
```

**What each part does:**
- `on: push: tags: ["v*"]` — only runs when you push a tag like `v1.1`
- `matrix.include` — five build targets. Each specifies its OS runner, Go target, and
  output archive name.
- `-trimpath` — removes local filesystem paths from the binary (cleaner, more
  reproducible)
- `-ldflags="-s -w"` — strips debug symbols and DWARF info, reducing binary size by
  ~30%
- `actions/upload-artifact` — saves each platform's archive so the `release` job can
  collect them
- The `release` job runs last, downloads all artifacts, and creates a GitHub Release
  using `gh release create` with auto-generated release notes

**Note on arm64 cross-compilation:**
- macOS arm64: `macos-latest` runners are already arm64 on GitHub Actions, so no
  cross-compilation needed. If you need BOTH amd64 and arm64, use `macos-13` for
  amd64 and `macos-latest` for arm64.
- Linux arm64: the `ubuntu-latest` runner is x86_64. The workflow installs
  `gcc-aarch64-linux-gnu` and the arm64 versions of the system libraries. The `CC`
  env var tells Go to use the cross-compiler. If this proves too fragile, an
  alternative is to use a self-hosted ARM runner or drop the Linux arm64 target
  initially.
- Windows arm64: not included — Windows ARM is still rare. Add it later if needed.

**How to use:** After merging, create a release:

```bash
git tag v1.1
git push origin v1.1
```

Then check the Actions tab — the release workflow builds all platforms and publishes
them to the Releases page.

---

### 3.3 — Platform build scripts

These are for local development and manual builds. CI uses them too (optionally).

#### macOS — already exists at `dist/macos/build.sh`

No changes needed. It creates `HTMLClay.app` with the correct bundle structure.

#### Linux — create `dist/linux/build.sh`

```bash
#!/bin/bash
set -euo pipefail

# Build the binary
CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o htmlclay .

echo "Built htmlclay"
echo ""
echo "To install:"
echo "  sudo cp htmlclay /usr/local/bin/"
echo "  cp dist/linux/htmlclay.desktop ~/.local/share/applications/"
echo "  cp dist/linux/htmlclay-mime.xml ~/.local/share/mime/packages/"
echo "  update-mime-database ~/.local/share/mime"
```

**What this does:**
- Builds the binary with the same flags as the release workflow
- Prints install instructions (we don't auto-install because that needs sudo)

#### Windows — create `dist/windows/build.ps1`

```powershell
$ErrorActionPreference = "Stop"

$env:CGO_ENABLED = "1"
go build -trimpath -ldflags="-s -w" -o htmlclay.exe .

Write-Host "Built htmlclay.exe"
```

**Verify:**

```bash
# On macOS (you can only run the macOS script locally):
bash dist/macos/build.sh

# Check the Linux script at least parses:
bash -n dist/linux/build.sh
```

---

### 3.4 — File association registration

Users need to double-click `.clayhtml` files and have them open in HTML Clay.

#### macOS — already done

`dist/macos/Info.plist` already declares `CFBundleDocumentTypes` with the `clayhtml`
extension. macOS reads this from the `.app` bundle automatically.

#### Linux — create `dist/linux/htmlclay.desktop`

This file tells Linux desktop environments (GNOME, KDE, etc.) that HTML Clay exists
and can open `.clayhtml` files.

```ini
[Desktop Entry]
Type=Application
Name=HTML Clay
Comment=Edit .clayhtml files
Exec=htmlclay %f
Icon=htmlclay
Terminal=false
Categories=Development;WebDevelopment;
MimeType=application/x-clayhtml;
```

**What each field means:**
- `Exec=htmlclay %f` — `%f` is replaced by the file path when the user double-clicks
  a `.clayhtml` file. The binary must be on PATH (e.g., in `/usr/local/bin/`).
- `MimeType=application/x-clayhtml;` — declares that this app handles the
  `application/x-clayhtml` MIME type
- `Terminal=false` — don't open a terminal window
- `Icon=htmlclay` — looks for `htmlclay.png` in the system icon dirs

**Install location:** `~/.local/share/applications/htmlclay.desktop`

#### Linux — create `dist/linux/htmlclay-mime.xml`

This file registers the `.clayhtml` extension with the system MIME database so the
OS knows what type of file it is.

```xml
<?xml version="1.0" encoding="UTF-8"?>
<mime-info xmlns="http://www.freedesktop.org/standards/shared-mime-info">
  <mime-type type="application/x-clayhtml">
    <comment>Clay HTML File</comment>
    <glob pattern="*.clayhtml"/>
  </mime-type>
</mime-info>
```

**Install location:** `~/.local/share/mime/packages/htmlclay-mime.xml`

**After installing both files, run:**

```bash
update-mime-database ~/.local/share/mime
xdg-mime default htmlclay.desktop application/x-clayhtml
```

The first command rebuilds the MIME database. The second sets HTML Clay as the default
app for `.clayhtml` files.

#### Windows — create `dist/windows/register.bat`

This script registers the `.clayhtml` file association in the Windows registry. Run
it once after installing the binary.

```batch
@echo off
setlocal

set "EXE=%~dp0htmlclay.exe"

:: Register the file type
reg add "HKCU\Software\Classes\.clayhtml" /ve /d "HTMLClay.Document" /f
reg add "HKCU\Software\Classes\HTMLClay.Document" /ve /d "Clay HTML File" /f
reg add "HKCU\Software\Classes\HTMLClay.Document\shell\open\command" /ve /d "\"%EXE%\" \"%%1\"" /f

echo File association registered for .clayhtml
echo Restart Explorer or log out/in for changes to take effect.
```

**What this does:**
- `HKCU\Software\Classes\.clayhtml` — maps the `.clayhtml` extension to a file type
  ID (`HTMLClay.Document`)
- `HTMLClay.Document` — the file type ID, with a human-readable name
- `HTMLClay.Document\shell\open\command` — tells Windows what command to run when the
  file is double-clicked. `%1` is replaced by the file path.
- Uses `HKCU` (current user) not `HKLM` (all users), so no admin rights needed.
- `%~dp0` expands to the directory where the `.bat` file lives, so the binary and
  `.bat` must be in the same folder.

**Verify (on a Windows machine):** Double-click the `.bat`, then double-click a
`.clayhtml` file — it should open in HTML Clay.

---

## Phase 4: Polish

### 4.1 — Fix signal handling for Windows

**Why:** `main.go` line 243 uses `syscall.SIGTERM` which compiles on Windows but is
never actually sent by the OS. In `--no-tray` mode, only Ctrl+C works. This isn't a
bug (tray mode is the default and handles shutdown via menu), but the signal list
should be correct.

**Edit `main.go` line 243:**

Change:
```go
signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
```
To:
```go
signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
```

`os.Interrupt` maps to `SIGINT` on Unix and to the console Ctrl+C event on Windows.
`syscall.SIGTERM` is still useful on Unix (sent by `kill`, `docker stop`, systemd,
etc.) and is harmlessly ignored on Windows.

Then remove the `"syscall"` import from line 11 — wait, check if `syscall` is used
anywhere else in `main.go`. Looking at the file: no, `syscall` is only used on line
243. So remove it from the imports. The file already imports `"os"` and `"os/signal"`.

**Updated imports (lines 4-23) — remove `"syscall"`:**

```go
import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/panphora/htmlclay/browser"
	...
)
```

Wait — double check: `syscall.SIGTERM` requires the `"syscall"` import. But
`os.Interrupt` comes from `"os"`, which is already imported. To keep `SIGTERM`, we
still need `"syscall"`. So **keep the `"syscall"` import** and just change `SIGINT`:

```go
signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
```

**Verify:**

```bash
go build ./...
GOOS=windows GOARCH=amd64 go vet ./...
```

---

### 4.2 — Platform icons

The system tray icon (`tray/icon.png`, embedded via `//go:embed`) works on all
platforms via the systray library — **no changes needed for the tray icon**.

For distribution (file explorer icons, taskbar, etc.):

**macOS:** Already handled — `dist/macos/htmlclay.icns` is bundled into the `.app`.

**Linux:** Ship the PNG icon for desktop integration.

Create `dist/linux/install-icon.sh`:

```bash
#!/bin/bash
set -euo pipefail

ICON_DIR="$HOME/.local/share/icons/hicolor/256x256/apps"
mkdir -p "$ICON_DIR"
cp dist/linux/htmlclay.png "$ICON_DIR/htmlclay.png"
gtk-update-icon-cache -f -t "$HOME/.local/share/icons/hicolor" 2>/dev/null || true

echo "Icon installed"
```

You need to provide a `dist/linux/htmlclay.png` (256x256 PNG). You can export the
existing `tray/icon.png` at a higher resolution, or create a dedicated one.

**Windows:** To show an icon in the taskbar and file explorer, embed a `.ico` file
into the Windows binary using `github.com/josephspurrier/goversioninfo`.

This is optional for initial release. Without it, Windows shows a generic icon. To
add it later:

1. Create `dist/windows/htmlclay.ico` (256x256 multi-resolution `.ico`)
2. Create `versioninfo.json` in the repo root (goversioninfo config)
3. Run `go generate` before building to embed the icon
4. Add a `go:generate` directive to `main.go`

This is low priority — ship without it first.

---

### 4.3 — Update Makefile

The current Makefile only works on macOS (`codesign`). Make it work on all platforms.

**Replace `Makefile` with:**

```makefile
.PHONY: build test clean dist-macos dist-linux dist-windows

BINARY = htmlclay
ifeq ($(OS),Windows_NT)
	BINARY = htmlclay.exe
endif

build:
	CGO_ENABLED=1 go build -o $(BINARY) .
ifeq ($(shell uname -s),Darwin)
	codesign -f -s - $(BINARY)
endif

test:
	go test ./... -count=1

clean:
	rm -f htmlclay htmlclay.exe
	rm -rf HTMLClay.app

dist-macos:
	bash dist/macos/build.sh

dist-linux:
	bash dist/linux/build.sh

dist-windows:
	powershell -File dist/windows/build.ps1
```

**What changed:**
- `BINARY` variable handles `.exe` on Windows
- `codesign` only runs on macOS (detected via `uname -s`)
- Added `dist-linux` and `dist-windows` targets
- `clean` removes both `htmlclay` and `htmlclay.exe`

**Verify:**

```bash
make build
make test
```

---

## Phase Summary & Checklist

Use this as a checklist. Check off each item after completing it.

### Phase 1: Dependencies (~2 hours)
- [ ] 1.1 Replace systray import in `tray/tray.go`, run `go get` + `go mod tidy`
- [ ] 1.2 Bump `go 1.23` in `go.mod`
- [ ] Verify: `go test ./...` passes

### Phase 2: Platform paths (~3 hours)
- [ ] 2.1 Change `config/config.go` to use `os.UserConfigDir()`
- [ ] 2.1 Add `migrateConfigDir()` to `main.go`
- [ ] 2.2 Add `windowsChromePaths` and `findFromWindowsPaths()` to `browser/chrome.go`
- [ ] 2.3 Add `linuxChromePaths` and `findFromLinuxPaths()` to `browser/chrome.go`
- [ ] Verify: `go test ./...` passes, `GOOS=windows go build ./browser/` works

### Phase 3: Build pipeline (~2-3 days)
- [ ] 3.1 Create `.github/workflows/ci.yml`
- [ ] 3.2 Create `.github/workflows/release.yml`
- [ ] 3.3 Create `dist/linux/build.sh` and `dist/windows/build.ps1`
- [ ] 3.4 Create `dist/linux/htmlclay.desktop` and `dist/linux/htmlclay-mime.xml`
- [ ] 3.4 Create `dist/windows/register.bat`
- [ ] Verify: push branch, CI passes on all 3 OS runners

### Phase 4: Polish (~3 hours)
- [ ] 4.1 Fix signal handling in `main.go`
- [ ] 4.2 Add Linux icon and install script
- [ ] 4.3 Update Makefile for cross-platform
- [ ] Verify: `make build && make test` passes

### Final validation
- [ ] `go test ./...` passes on macOS
- [ ] CI passes on all three platforms (GitHub Actions)
- [ ] Tag a test release, verify artifacts are published
- [ ] Test on a real Windows machine: install, launch, open `.clayhtml`, tray menu works
- [ ] Test on a real Linux machine (Ubuntu): install, launch, open `.clayhtml`, tray icon shows

---

## Files Created/Modified (Complete List)

**Modified:**
- `go.mod` — Go version bump + new systray dep
- `tray/tray.go` — import path change (1 line)
- `config/config.go` — `os.UserConfigDir()` instead of `os.UserHomeDir()`
- `browser/chrome.go` — Windows + Linux path detection
- `main.go` — migration function + signal fix
- `Makefile` — cross-platform targets

**Created:**
- `.github/workflows/ci.yml`
- `.github/workflows/release.yml`
- `dist/linux/build.sh`
- `dist/linux/htmlclay.desktop`
- `dist/linux/htmlclay-mime.xml`
- `dist/linux/htmlclay.png` (you provide this asset)
- `dist/linux/install-icon.sh`
- `dist/windows/build.ps1`
- `dist/windows/register.bat`

---

## What We're NOT Doing (and why)

- **GoReleaser**: Adds complexity for a project this size. The CI workflow above does
  the same thing with plain `go build` + `gh release create`. Consider adding
  GoReleaser later if you need more packaging options (homebrew, scoop, etc.).
- **Flatpak/Snap**: Adds ongoing maintenance. A tarball + `.desktop` file covers
  modern Linux. Add these if users request them.
- **Windows Store / Mac App Store**: Requires paid signing certificates and store
  review. Ship direct downloads first.
- **32-bit builds**: All modern systems are 64-bit. No i386 builds.
- **Windows ARM64**: Too niche. Add when demand appears.
- **Non-CGO systray**: No mature Go tray library avoids CGO. The CGO requirement is
  managed by using native CI runners.
