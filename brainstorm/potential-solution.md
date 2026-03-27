# Potential Solution: The KBTD Approach

## What KBTD Does

1. You run a script and point it at a file on your computer.
2. Behind the scenes, a small web server is already running on your machine (localhost).
3. The script opens Chrome in "app mode" — which means it looks like its own application, no address bar, no tabs, just a clean window.
4. Chrome loads the page from the local server, and the server knows which file you want to edit because the file path is passed in the URL.

### How It Works Technically

The launcher (`kbtd.sh`) does four things:

**1. Resolves the file path to an absolute path.**
The script takes a relative or absolute path, creates the file if it doesn't exist, and resolves it to an absolute path using `realpath` (Linux), a `cd`/`pwd` fallback (macOS), or `readlink -f` (other). This absolute path becomes the file identifier passed to the server.

**2. Finds a Chromium-based browser.**
It searches in this order:
- `$BROWSER` environment variable (if it contains "chrome", "chromium", or "edge")
- Common CLI names in `$PATH`: `google-chrome`, `chromium`, `chromium-browser`, `microsoft-edge`
- macOS `.app` bundle paths under `/Applications/`
- WSL path: `/mnt/c/Program Files/Google/Chrome/Application/chrome.exe`

It requires a Chromium-based browser because app mode (`--app`) is a Chromium feature. Safari and Firefox don't support it.

**3. Constructs the URL.**
The server's base URL comes from `$KBTD_URL` (defaults to `http://localhost:8000`). The file path is appended as a query parameter:

```
http://localhost:8000/?file=/Users/david/Documents/my-file.md
```

The server reads this parameter to know which file to load and where to write changes back.

**4. Launches Chrome in app mode.**
Two Chrome flags make this work:

- `--app="URL"` — Opens the URL in a standalone window with no address bar, tabs, bookmarks, or browser UI. It looks and feels like a native application.
- `--user-data-dir="./.kbtd-chrome-data"` — Creates an isolated Chrome profile in the current directory. This prevents the app window from sharing cookies, extensions, and session data with the user's normal browser. It also avoids conflicts if Chrome is already running.

The process is launched in the background (`&`) so the terminal returns immediately.

## The Architecture

```
┌─────────────────────────────────────────────────┐
│  Chrome in App Mode (--app)                     │
│  ┌───────────────────────────────────────────┐  │
│  │  HTML/JS UI served from localhost         │  │
│  │                                           │  │
│  │  User edits content visually              │  │
│  │  JS calls fetch("POST /api/save", body)   │  │
│  └──────────────────┬────────────────────────┘  │
│                     │ HTTP                       │
└─────────────────────┼───────────────────────────┘
                      │
┌─────────────────────┼───────────────────────────┐
│  Localhost Server    │  (http://localhost:8000)   │
│                     ▼                            │
│  GET  /?file=/path  → Serve the editor UI        │
│  GET  /api/read     → Read file, return contents │
│  POST /api/save     → Write contents back to     │
│                       the same file on disk       │
│                                                  │
│  The server has full filesystem access via        │
│  Node.js fs / Python os — this is the bridge     │
│  that browsers refuse to provide.                │
└──────────────────────────────────────────────────┘
                      │
                      ▼
┌──────────────────────────────────────────────────┐
│  The File on Disk                                │
│  /Users/david/Documents/my-file.html             │
│                                                  │
│  This is the single source of truth.             │
│  It gets overwritten in place.                   │
│  It can be emailed, Dropboxed, or uploaded.      │
└──────────────────────────────────────────────────┘
```

**Why localhost is the key:**
- `http://localhost` is treated as a secure context by all major browsers, unlocking APIs that `file://` blocks (Service Workers, File System Access API, etc.).
- The server process runs with the user's OS-level permissions, so it can read and write any file the user can.
- The browser communicates with the server over standard HTTP — no special extensions, flags, or hacks required.

## Why This Gets Close to the Wish List

- The localhost server has full access to your filesystem — so it *can* read the file and write changes back to the same file. That's the key piece browsers won't do on their own.
- Chrome in app mode makes it *feel* like a native application, not a browser tab.
- The file on disk is the single source of truth. Edit it, save it, email it to someone — it's just a file.

## What's Missing

- **The user has to start a server.** KBTD assumes `localhost:8000` is already running. A normal person isn't going to do that.
- **There's a script to run.** You have to open a terminal and type a command. Non-technical users won't do this.
- **Chromium-only.** App mode is a Chromium feature. Safari and Firefox users would get a regular browser tab at best.

## How to Close the Gap

Package the whole thing as a single installable app — something the user installs once, like any other application. That app would:

1. **Contain the tiny localhost server bundled inside it.**
2. **Register itself as the handler for `.malleable` files** (or whatever extension you pick).
3. **When the user double-clicks a `.malleable` file**, the app starts the server automatically, passes the file path, and opens Chrome in app mode.

From the user's perspective: they double-click a file, a window opens, they edit, they hit save, the file on disk is updated. They close the window, email the file, done.

### Packaging Options

| Approach | What It Is | Binary Size | Includes Browser? | Filesystem Access |
|---|---|---|---|---|
| **Electron** | Node.js + bundled Chromium | ~150–300 MB | Yes (Chromium) | Full (Node.js `fs`) |
| **Tauri** | Rust backend + system WebView | ~5–10 MB | No (uses system WebView) | Full (Rust `std::fs`) |
| **Neutralinojs** | Lightweight runtime + system browser | ~2–5 MB | No (uses system browser) | Full (native API) |
| **Custom binary + system Chrome** | Tiny Go/Rust/Node binary that starts an HTTP server and shells out to Chrome `--app` | ~5–15 MB | No (uses installed Chrome) | Full |

**The "custom binary + system Chrome" approach is closest to what KBTD already does** — it's essentially KBTD's bash script compiled into an installable application. The binary would:

1. Start an embedded HTTP server on a random available port.
2. Detect Chrome/Edge on the system (same logic as `kbtd.sh`).
3. Launch it with `--app=http://localhost:{port}/?file={path}`.
4. Shut down the server when the browser window closes.

### File Association (How Double-Click Works)

**macOS:**
The app bundle's `Info.plist` declares `CFBundleDocumentTypes` with a custom UTI for `.malleable` files. Once installed, macOS knows to open `.malleable` files with this app. The file path is passed to the app as a launch argument.

**Windows:**
A standard installer writes registry keys under `HKEY_CLASSES_ROOT` associating `.malleable` with the app's executable. Double-clicking passes the file path as `argv[1]`.

**Linux:**
A `.desktop` file declares `MimeType=application/x-malleable` and a mime database entry maps `.malleable` to that type. The file path is passed as an argument.

In all cases, the flow is: OS passes the file path to the app → app starts the server → app opens Chrome with the file path in the URL.

### The Save Mechanism (How the HTML File Overwrites Itself)

The HTML file contains JavaScript that talks to the localhost server:

```js
// Inside the malleable HTML file
async function save() {
  const html = document.documentElement.outerHTML;
  await fetch('/api/save', {
    method: 'POST',
    headers: { 'Content-Type': 'text/html' },
    body: html
  });
}
```

The server receives this and writes it back to the same file path:

```js
// Server-side (Node.js example)
app.post('/api/save', async (req, res) => {
  const filePath = req.query.file;
  await fs.writeFile(filePath, req.body, 'utf-8');
  res.json({ ok: true });
});
```

The file on disk is now updated. The HTML file has effectively overwritten itself.

### How the Server Has File Access (No Root Required)

The server doesn't run as root and doesn't need elevated permissions. It runs as a **child process of the app, which runs as the current user** — the same way VS Code, Figma, or any other application runs when you open it.

Your user account already has read/write access to your own files (Documents, Desktop, Downloads, etc.). The server inherits those permissions automatically. There's nothing special happening — it's just a process running as you, calling `fs.writeFile()` on a path you own.

The only case where a save would fail is if the `.malleable` file lives somewhere the user doesn't have write access (e.g., `/usr/local/`, another user's home directory). That's the same behavior as any other application.

### Security Considerations

- **The server only listens on `127.0.0.1`** (loopback), so it's not reachable from the network.
- **The server should validate file paths** — restrict writes to the specific file that was opened, not arbitrary paths. Without this, a malicious HTML file could write anywhere the user has permission.
- **The server should bind to a random port** and pass it to the browser, avoiding collisions with other services.
- **A per-session token** (generated at launch, passed in the URL fragment or a cookie) prevents other local pages from calling the save API.

## Bottom Line

KBTD proves the core mechanism works — localhost server + Chrome app mode + file path as a parameter. The remaining work is packaging it so the user never sees the terminal, never starts a server, and just double-clicks a file. The most pragmatic path is a small compiled binary (~5–15 MB) that bundles an HTTP server, detects the user's installed Chromium browser, and registers itself as the `.malleable` file handler.
