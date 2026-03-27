# Malleable: A Self-Saving HTML File Runtime

## Concept

A small installable application called **Malleable** that lets any HTML file save itself back to disk. The user installs it once. After that, they double-click a `.malleable` file, edit it visually in a browser window, and their changes write back to the same file. The file is the database. Email it, Dropbox it, upload it — it's just an HTML file with a different extension.

Under the hood: the app spins up a tiny localhost server, opens the file in Chrome's app mode, and the server handles reads and writes. The user never sees any of this.

## User Experience

### For the recipient (non-technical user)

1. Receive a `.malleable` file (email, Dropbox, AirDrop, whatever).
2. Install Malleable if they haven't already (one-time, like any app).
3. Double-click the file.
4. A clean window opens — no address bar, no tabs. Looks like an app.
5. Edit using the page's own interface.
6. Hit Save. The file on disk is updated.
7. Close the window. Send the file back.

### For the developer

1. Build an HTML file with whatever editor, framework, or tools you want.
2. Include a small JS snippet that calls the Malleable save API.
3. Rename it to `.malleable`.
4. Send it to someone.

In a text editor, a `.malleable` file is just HTML. Associate the extension with HTML syntax highlighting and you're done.

## The `.malleable` File

It's literally just an HTML file. The only reason for the custom extension is to associate it with the Malleable app so the OS knows what to open it with. The contents are standard HTML, CSS, and JavaScript — no proprietary format, no special headers, no build step.

A minimal `.malleable` file:

```html
<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8">
  <title>My Malleable File</title>
</head>
<body>
  <div id="editor" contenteditable="true">
    <h1>Edit me</h1>
    <p>This content will save back to disk.</p>
  </div>
  <button id="save">Save</button>

  <script>
    const name = document.documentElement.getAttribute('appname');

    document.getElementById('save').addEventListener('click', async () => {
      const html = '<!DOCTYPE html>\n' + document.documentElement.outerHTML;
      const res = await fetch('/save/' + name, {
        method: 'POST',
        headers: { 'Content-Type': 'text/html' },
        body: html
      });
      if (res.ok) console.log('Saved.');
    });
  </script>
</body>
</html>
```

That's it. Any HTML author can make a malleable file.

## Architecture

A single server handles all open files. Files are identified by their path relative to the user's home directory. Only files inside the home directory are supported.

```
User double-clicks ~/Documents/file.malleable
          │
          ▼
┌──────────────────────────────────────────────────┐
│  Malleable App (tray app, already running)       │
│                                                  │
│  1. Receives file path from OS                   │
│  2. Validates file is inside home directory       │
│  3. Registers the file with the running server   │
│  4. Generates a session token for this file      │
│  5. Opens browser to:                            │
│     127.0.0.1:{port}/f/Documents/file.malleable │
│  6. Token lives until app quits                   │
│                                                  │
│  One server, one port, multiple files.           │
└──────────────┬───────────────────────────────────┘
               │
               ▼
┌──────────────────────────────────────────────────┐
│  Chrome/Edge in App Mode (--app)                 │
│  ┌────────────────────────────────────────────┐  │
│  │  The .malleable file served from localhost │  │
│  │                                            │  │
│  │  User sees and interacts with this.        │  │
│  │  JS calls fetch('/save/' + name, { body })│  │
│  └─────────────────┬──────────────────────────┘  │
│                    │ HTTP (localhost only)         │
└────────────────────┼─────────────────────────────┘
                     │
┌────────────────────┼─────────────────────────────┐
│  Localhost Server   │  http://127.0.0.1:{port}    │
│                    ▼                              │
│  GET  /f/{path}    → Serve the .malleable file   │
│  GET  /read/{token}  → Return file contents        │
│  POST /save/{token}  → Overwrite file on disk      │
│  GET  /meta/{token}  → Return file path, name, size│
│                                                  │
│  {path} is relative to ~/                        │
│  e.g. /f/Documents/file.malleable                │
│  →  /Users/david/Documents/file.malleable        │
│                                                  │
│  Validates session token on every request.       │
│  Only allows writes to registered files.         │
│  Rejects paths outside the home directory.       │
│  Runs as the current user (no root).             │
└──────────────────────────────────────────────────┘
                     │
                     ▼
┌──────────────────────────────────────────────────┐
│  The File on Disk                                │
│  ~/Documents/file.malleable                      │
│                                                  │
│  Overwritten in place on every save.             │
│  Portable. Transferable. It's just HTML.         │
└──────────────────────────────────────────────────┘
```

## URL Structure

Files are served at paths relative to the user's home directory, prefixed with `/f/`:

```
http://127.0.0.1:{port}/f/Documents/file-a.malleable
http://127.0.0.1:{port}/f/Desktop/notes.malleable
http://127.0.0.1:{port}/f/Projects/app/dashboard.malleable
```

The `/f/` prefix keeps file paths separate from API routes. The server resolves the path after `/f/` relative to `~/`. In Browser Mode, the address bar shows the full path — you immediately know which file you're looking at.

Files outside the home directory are not supported. The server rejects any path that resolves outside `~/` (including `../` traversal attempts).

## Server API

One server runs for the lifetime of the tray app on a single port. It handles all open files. When a `.malleable` file is opened, the app registers that file path with the server and generates a session token for it. The token identifies which file a request is for.

Four endpoints:

### `GET /f/{path}`

Serves the `.malleable` file as `text/html`. `{path}` is relative to `~/`. This is what the browser loads as the page. The server adds an `appname` attribute to the `<html>` element containing the file's session token (see Authentication). The server only serves files that have been registered (opened via double-click).

### `GET /read/{token}`

Returns the current file contents as `text/html`. The token in the URL identifies which file. Useful if the page wants to reload its own source or diff against disk.

### `POST /save/{token}`

Accepts the full HTML as the request body. The token in the URL identifies which file to write. Overwrites the original. The server strips the `appname` attribute from the `<html>` element before writing, so the file on disk stays clean. Maximum body size: **50 MB**. Requests larger than this are rejected.

**Success** (200):
```json
{ "ok": true }
```

**Error** (4xx/5xx):
```json
{ "ok": false, "error": "permission denied" }
```

Status codes:
- `401` — missing or invalid token
- `403` — permission denied (file not writable)
- `413` — body too large (over 50 MB)
- `500` — disk error (full, I/O failure)

### `GET /meta/{token}`

Returns metadata about the file identified by the token:

```json
{
  "path": "Documents/file.malleable",
  "absolutePath": "/Users/david/Documents/file.malleable",
  "name": "file.malleable",
  "size": 4823,
  "lastModified": "2026-03-24T10:30:00Z"
}
```

### Authentication

Each open file gets its own session token, generated when that file is opened — 32 bytes from `crypto/rand`, encoded as base64url (43 characters, 256 bits of entropy). The token is included in the URL path of every API request (`/save/{token}`, `/read/{token}`, `/meta/{token}`).

The token is delivered via the `appname` attribute on the `<html>` element. When serving `GET /f/{path}`, the server adds this attribute to the document before sending it to the browser:

```html
<html appname="a1b2c3d4e5f6...">
```

The file's JS reads the token from the DOM:

```js
const name = document.documentElement.getAttribute('appname');
```

This is per-tab safe — each page has its own DOM, so multiple open files cannot interfere with each other. On save, the server strips the `appname` attribute before writing to disk, so the file stays clean.

The server rejects any request without a valid token. The token maps to a specific registered file, so the server knows exactly which file to read or write without the browser needing to specify a path.

## CLI

```
malleable <file>           # Open in Browser Mode (default)
malleable --app <file>     # Open in App Mode
malleable --browser <file> # Open in Browser Mode (explicit)
```

The CLI accepts both `.malleable` and `.html` files. The `.malleable` extension is for OS file association (double-click to open); the CLI doesn't enforce it.

The CLI defaults to **Browser Mode** because the developer running a terminal command likely wants DevTools access. The tray app (double-click) defaults to **App Mode** because the end user wants a clean window.

## Launch Modes

The app supports two launch modes:

### App Mode

Opens in a chromeless standalone window — no address bar, no tabs, no bookmarks. Looks like a native application. This is the mode for end users. Default when opening files via double-click (tray app).

Requires a Chromium-based browser for `--app` flag. If none is found, falls back to Browser Mode automatically.

### Browser Mode

Opens the localhost URL in the user's default browser as a regular tab. Address bar shows the full file path: `http://127.0.0.1:49821/f/Documents/file.malleable`. Full DevTools access, ability to inspect the page, view network requests, edit in the console. Default when opening files via the CLI.

Useful for:
- Debugging save/load behavior
- Inspecting the HTML structure
- Testing with DevTools open
- Working in Safari or Firefox (which don't support app mode)

### How each mode launches

**App Mode:**

Detects a Chromium browser in this order:
1. `$MALLEABLE_BROWSER` environment variable (explicit override)
2. `$BROWSER` if it contains "chrome", "chromium", or "edge"
3. Common CLI names in `$PATH`: `google-chrome`, `chromium`, `chromium-browser`, `microsoft-edge`
4. **macOS**: `/Applications/Google Chrome.app`, `/Applications/Chromium.app`, `/Applications/Microsoft Edge.app`
5. **Windows**: `%ProgramFiles%\Google\Chrome\Application\chrome.exe`, Edge equivalent
6. **WSL**: `/mnt/c/Program Files/Google/Chrome/Application/chrome.exe`

Then launches with:

```
"{browser}" --app="http://127.0.0.1:{port}/f/{relative_path}" --user-data-dir="{profile_dir}"
```

- `--app` — chromeless window, no address bar or tabs
- `--user-data-dir` — persistent profile at `~/.malleable/chrome-profile/`, shared across all app-mode windows. This lets Chrome reuse one process for multiple files (lower RAM) while staying isolated from the user's normal browser session

If no Chromium browser is found, falls back to Browser Mode.

**Browser Mode:**

Opens the URL in the system default browser:
- macOS: `open "http://127.0.0.1:{port}/..."`
- Linux: `xdg-open "http://127.0.0.1:{port}/..."`
- Windows: `start "http://127.0.0.1:{port}/..."`

Works with any browser. The save mechanism is the same — only the window chrome differs.

## System Tray

The app runs as a persistent menu bar / system tray icon. This keeps it ready to handle `.malleable` file opens instantly (no cold start) and gives the user a single place to control settings.

### Menu

```
  Update to v1.2 ↓    ← only shown when an update is available
  ─────────────
✓ App Mode
  Browser Mode
  ─────────────
✓ Start on Login
  ─────────────
  Quit
```

**Update to vX.Y**: Shown only when a newer version is available. On startup, the app fetches a version check endpoint (e.g., `GET https://malleable.app/version.json` → `{ "latest": "1.2", "url": "https://malleable.app/download" }`). If the response indicates a newer version, this menu item appears. Clicking it opens the download URL in the default browser. The check is fire-and-forget — if the request fails or the server is unreachable, the menu item simply doesn't appear.

**App Mode / Browser Mode**: Radio toggle. Selecting one deselects the other. Controls how `.malleable` files open going forward — doesn't affect already-open files. Persisted to a config file (e.g., `~/.malleable/config.json` or platform equivalent) so the preference survives restarts.

**Start on Login**: Checkbox. Registers/unregisters the app as a login item so it launches automatically when the user logs in. This ensures `.malleable` files open instantly on double-click without waiting for the app to start.

- macOS: `SMAppService.register()` (modern) or a LaunchAgent plist
- Windows: Registry key under `HKEY_CURRENT_USER\Software\Microsoft\Windows\CurrentVersion\Run`
- Linux: `.desktop` file in `~/.config/autostart/`

**Quit**: Shuts down the server, revokes all tokens, and exits the tray app.

### Behavior

- **Double-clicking a `.malleable` file** while the tray app is running: the already-running app handles it immediately (registers the file, opens the browser). No second app instance is spawned.
- **Double-clicking a `.malleable` file** while the tray app is not running: the OS launches the app, the app starts the tray icon and server, then handles the file.
- **Multiple files**: Each open file gets its own token on the same server. The tray app manages all of them. When the last browser window closes, the tray app stays resident (waiting for the next file).

### Single-Instance Enforcement

Only one instance of the app should ever run. When a second invocation is triggered (e.g., double-clicking another `.malleable` file), it must hand the file path to the running instance and exit.

**macOS:** Handled automatically by the OS. macOS sends `open` events to the running app bundle instance via Apple Events / `NSApplicationDelegate`. No extra work needed.

**Linux:** On startup, the app creates a lock file at `~/.malleable/lock` (containing the PID and port) and listens on a Unix socket at `~/.malleable/sock`. A second invocation checks the lock file, sends the file path to the socket, and exits. The running instance receives the path and handles it. If the lock file exists but the PID is dead, the new instance takes over.

**Windows:** On startup, the app creates a named mutex (`Global\MalleableApp`). A second invocation detects the existing mutex, sends the file path to the running instance via a named pipe (`\\.\pipe\MalleableApp`), and exits. The running instance reads the path from the pipe and handles it.

### Config

Stored in `~/.malleable/config.json`:

```json
{
  "mode": "app",
  "startOnLogin": true,
  "port": 49821
}
```

The port is persisted so URLs stay stable across restarts (useful for Browser Mode bookmarks and DevTools). On first launch, the app picks a random available port and saves it. On subsequent launches, it reuses the saved port. If the port is taken, it picks a new one and updates the config.

### Logging

Server logs are written to `~/.malleable/malleable.log`. Logs include server start/stop, file registrations, save requests, and errors. Logs are rotated when they exceed 10 MB (keep one previous log as `malleable.log.1`).

### Crash Recovery

If the server crashes or is killed, tokens and file registrations are lost — they exist only in memory. The user reopens their files. No persistent session state to recover or clean up.

## File Association

How the OS knows to open `.malleable` files with the Malleable app.

### macOS

The app bundle's `Info.plist`:

```xml
<key>CFBundleDocumentTypes</key>
<array>
  <dict>
    <key>CFBundleTypeName</key>
    <string>Malleable HTML File</string>
    <key>CFBundleTypeExtensions</key>
    <array>
      <string>malleable</string>
    </array>
    <key>CFBundleTypeRole</key>
    <string>Editor</string>
    <key>LSHandlerRank</key>
    <string>Owner</string>
  </dict>
</array>
```

macOS will also prompt for Documents/Desktop/Downloads access (TCC) the first time the app touches a protected folder. One-time, per folder.

### Windows

The installer writes registry entries:

```
HKEY_CLASSES_ROOT\.malleable → "MalleableFile"
HKEY_CLASSES_ROOT\MalleableFile\shell\open\command → "C:\Program Files\Malleable\malleable.exe" "%1"
```

### Linux

A `.desktop` file:

```ini
[Desktop Entry]
Name=Malleable
Exec=malleable %f
MimeType=application/x-malleable;
Type=Application
```

Plus a mime database entry mapping `.malleable` → `application/x-malleable`.

## Lifecycle

### First launch (or not yet running)

1. **OS invokes app** with file path as argument.
2. **App starts** the tray icon and an HTTP server on `127.0.0.1` using the saved port from config (or a random available port on first launch).
3. **App validates** the file is inside `~/`. Rejects if not.
4. **App registers** the file with the server and generates a session token for it.
5. **App opens** the browser (App Mode or Browser Mode per config) to `http://127.0.0.1:{port}/f/{relative_path}`.
6. **Browser loads** the `.malleable` file from the server.
7. **User edits** and saves. Each save is a `POST /save/{token}` — the server identifies the file by token and overwrites it.

### Subsequent file opens (tray app already running)

1. **OS sends** the file path to the already-running app (single instance).
2. **If the file is already open**, focus the existing window (App Mode) or open the existing URL (Browser Mode) instead of creating a duplicate. This prevents conflicting edits to the same file.
3. **If the file is new**, register it with the existing server, generate a new token, and open a new browser window to the file's URL.

### Closing files

- **Tokens live for the lifetime of the tray app.** Closing a browser window doesn't revoke the token or unregister the file. This avoids unreliable tab-close detection (especially in Browser Mode). The token is useless once the tab is gone — nobody else has it.
- **All browser windows closed** → the server and tray app stay running, waiting for the next file.
- **User clicks Quit** in the tray menu → server shuts down, all tokens revoked, app exits.

## Security Model

**The server only writes to registered files.** When a file is opened, the app registers its path with the server. The save endpoint identifies the file by token — it doesn't accept arbitrary paths. Tokens live for the lifetime of the tray app and are all revoked on Quit.

**Home directory only.** The server rejects any path that resolves outside `~/`, including `../` traversal attempts. This bounds the blast radius.

**Loopback only.** The server binds to `127.0.0.1`, not `0.0.0.0`. It's unreachable from the network.

**Host header validation.** Every request must have a `Host` header of `127.0.0.1:{port}` or `localhost:{port}`. Reject anything else. This prevents DNS rebinding attacks where a remote page resolves a custom domain to `127.0.0.1`.

**Per-file token.** Each open file gets its own cryptographically random token, delivered via the `appname` attribute on the `<html>` element. The token is included in the URL path of every API request. This prevents other local pages or scripts from calling the save endpoint, and ensures one open file can't write to another.

**No root.** The server runs as the current user. It inherits the user's file permissions. If the user can't write to a path, neither can the server.

**Comparison to other apps:** This is the same trust model as VS Code, Figma Desktop, Obsidian, or any Electron app. The user runs an application that reads and writes files on their behalf.

## Trust Model

**Opening a `.malleable` file is equivalent to running an executable.** The file contains JavaScript that runs with full access to the Malleable save API. A malicious file could overwrite itself with arbitrary content, exfiltrate its own token, or rewrite its own scripts to persist across saves. The server does not and cannot sanitize file contents — the file's JS *is* the application.

This is the same trust model as opening a `.html` file locally, with one addition: the file can also write back to disk. Only open `.malleable` files from sources you trust, the same way you'd only run executables from sources you trust.

**The save mechanism is a plain `fetch` call.** The file's own JavaScript calls `POST /save/{token}` with the full HTML as the body. The server writes whatever HTML it receives (after stripping the `appname` attribute) — there is no sanitization or validation of the content. This is by design: the file's JS *is* the application, and stripping or filtering it would break functionality.

## Packaging Options

| Approach | Binary Size | Includes Browser? | Standalone Window | Notes |
|---|---|---|---|---|
| **Custom Go binary** | ~10–15 MB | No (system Chrome) | Only if Chrome/Edge installed | Single static binary. Embed a Go HTTP server. Shell out to detected Chrome in app mode. Simplest to build and distribute. Falls back to default browser (regular tab) if no Chromium browser found. |
| **Tauri** | ~5–10 MB | No (system WebView) | Always — built-in native window | Rust backend, smallest viable option. Always looks like a native app (no address bar, no tabs) regardless of what browsers the user has installed. Rendering engine varies by OS: WebKit on macOS, WebView2 (Edge) on Windows. |
| **Electron** | ~150–300 MB | Yes (Chromium) | Always — bundled Chromium | Heaviest, but guaranteed Chromium availability and standalone window. No browser detection needed. Most mature ecosystem. |
| **Neutralinojs** | ~2–5 MB | No (system browser) | No | Lightest, but less mature. Opens in default browser. |

**Decision: Go.** The app is a simple HTTP server + process launcher — no performance-critical code, no complex memory management, no async runtime. Go's standard library has everything needed (`net/http`, `os/exec`, `path/filepath`) with zero external dependencies. Cross-compilation is a one-liner (`GOOS=windows GOARCH=amd64 go build`). Build times are fast. The binary size difference vs Rust is marginal for this use case. Tauri's standalone window advantage is nice but adds framework complexity and WebView rendering inconsistencies that aren't worth it for v1.

## What a Developer Needs to Know

To make an HTML file "malleable-compatible," read the token from the `appname` attribute and call `POST /save/{token}` with the full HTML as the body. That's the entire contract.

```js
const name = document.documentElement.getAttribute('appname');
const save = () => fetch('/save/' + name, {
  method: 'POST',
  headers: { 'Content-Type': 'text/html' },
  body: '<!DOCTYPE html>\n' + document.documentElement.outerHTML
});
```

The server also provides these endpoints, all authenticated via the same token in the URL path:

- **`GET /read/{token}`** — reload the file from disk (useful for conflict detection)
- **`GET /meta/{token}`** — get file path, size, last modified time

## The Platform Angle

The `.malleable` file is just HTML. That means:

- **Upload to a hosting platform.** Serve it as a web page. Users can view and fork it online.
- **Download and edit locally.** The same file works both on the web and locally via the Malleable app.
- **Send it back.** Changes made locally can be re-uploaded.

The hosting platform would serve the file over HTTPS. Both Malleable and the platform use the same `appname` attribute and `/save/{name}` endpoint pattern, so the same file works in both environments without environment detection:

```js
const name = document.documentElement.getAttribute('appname');

// Same pattern works in both environments — the platform
// sets appname to the site name, Malleable sets it to a token.
await fetch('/save/' + name, { method: 'POST', body: html });
```

Same file, same code, any environment.

## Open Questions

- **Auto-save vs manual save?** Auto-save on every edit could be noisy for large files. Debounced auto-save (e.g., 2 seconds after last edit) is probably the right default, with manual save as an option.
- **Versioning?** The app could keep a `.malleable-history/` folder with timestamped snapshots on each save, giving basic undo/version history.
- **Multiple files?** Could a `.malleable` file reference other local files (images, CSS, data)? The server could serve a whole directory, not just one file. But this complicates the "single file" portability story.
- **What if Chrome isn't installed?** The Go binary falls back to Browser Mode — opens in the default browser as a regular tab. Everything works, just no standalone window.
- **Code signing and notarization?** Required for macOS distribution outside the App Store (Gatekeeper). Required for Windows to avoid SmartScreen warnings. Budget for Apple Developer ($99/year) and Windows code signing certificate.
