# Build Plan: Malleable (macOS only)

Module path: `github.com/panphora/malleable`

## Step 1: Housekeeping
- Move existing files (`blog-post.md`, `brainstorm.md`, `implementation-plan.md`, `kbtd.sh`, `lopecode.md`, `malleable-app-spec.md`, `potential-solution.md`, `research-report.md`) into `brainstorm/`

## Step 2: Project bootstrap
- `go mod init github.com/panphora/malleable`
- Create directory structure: `server/`, `session/`, `htmlutil/`, `browser/`, `config/`, `logging/`, `platform/`, `tray/`, `update/`, `dist/macos/`, `testdata/`

## Step 3: Phase 1 — Core Server (36 files total, macOS-only)
Build bottom-up so each layer compiles as we go:

1. `config/config.go` + `config/config_test.go` — config load/save, port resolution
2. `session/session.go` + `session/session_test.go` — token generation, file registration
3. `htmlutil/htmlutil.go` + `htmlutil/htmlutil_test.go` — inject/strip appname
4. `server/security.go` + `server/security_test.go` — host validation, path traversal checks
5. `server/handlers.go` — serve file, read, save, meta handlers
6. `server/server.go` + `server/server_test.go` + `server/handlers_test.go` — HTTP server, router, middleware
7. `main.go` — CLI entry point, wires everything together
8. `testdata/minimal.malleable`, `testdata/with-appname.malleable`, `testdata/traversal.malleable`

## Step 4: Phase 2 — Browser Launch
1. `browser/chrome.go` + `browser/chrome_test.go` — Chromium detection + app mode
2. `browser/browser.go` — interface
3. `browser/browser_darwin.go` — macOS `open` command
4. Update `main.go` with browser launch logic

## Step 5: Phase 3 — Logging
1. `logging/logging.go` + `logging/logging_test.go` — file logger with 10MB rotation
2. Update `server/server.go` to use custom logger + request logging middleware
3. Update `main.go` to use file-based logger

## Step 6: Phase 4 — System Tray
1. `go get github.com/getlantern/systray`
2. `tray/tray.go` + `tray/tray_test.go` + `tray/icon.png`
3. Update `main.go` — restructure so `systray.Run` blocks main goroutine

## Step 7: Phase 5 — Single Instance
1. `platform/singleinstance.go` — interface
2. `platform/singleinstance_darwin.go` — Unix socket implementation
3. Update `main.go` with single-instance logic + file forwarding

## Step 8: Phase 6 — Login Item + Update Check
1. `platform/loginitem.go` — interface
2. `platform/loginitem_darwin.go` — LaunchAgent plist
3. `update/update.go` + `update/update_test.go` — version check
4. Wire into tray

## Step 9: Phase 7 — macOS App Bundle
1. `dist/macos/Info.plist`
2. `dist/macos/malleable.icns` (placeholder)
3. `dist/macos/build.sh`
4. `Makefile`

## Step 10: Verify
- `go test ./... -race -count=1`
- `go build -o malleable .`
- Manual smoke test with `curl`

## Files created (36 + icon)

```
malleable/
├── main.go
├── go.mod
├── go.sum
├── Makefile
├── server/
│   ├── server.go
│   ├── server_test.go
│   ├── handlers.go
│   ├── handlers_test.go
│   ├── security.go
│   └── security_test.go
├── session/
│   ├── session.go
│   └── session_test.go
├── htmlutil/
│   ├── htmlutil.go
│   └── htmlutil_test.go
├── browser/
│   ├── browser.go
│   ├── browser_darwin.go
│   ├── chrome.go
│   └── chrome_test.go
├── config/
│   ├── config.go
│   └── config_test.go
├── logging/
│   ├── logging.go
│   └── logging_test.go
├── platform/
│   ├── singleinstance.go
│   ├── singleinstance_darwin.go
│   ├── loginitem.go
│   └── loginitem_darwin.go
├── tray/
│   ├── tray.go
│   ├── tray_test.go
│   └── icon.png
├── update/
│   ├── update.go
│   └── update_test.go
├── dist/
│   └── macos/
│       ├── Info.plist
│       ├── malleable.icns
│       └── build.sh
└── testdata/
    ├── minimal.malleable
    ├── with-appname.malleable
    └── traversal.malleable
```
