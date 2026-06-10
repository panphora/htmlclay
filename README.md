# HTML Clay

A desktop app that makes self-saving HTML files a native OS feature.

- **Website:** [htmlclay.com](https://htmlclay.com)
- **File extension:** `.htmlclay`
- **Parent platform:** [Hyperclay](https://hyperclay.com)

## What is it?

HTML Clay lets you double-click an HTML file, edit it visually, and save your changes — just like you would with a Word document or a Photoshop file.

HTML is the most powerful document format ever created. It can render rich interfaces, run code, play media, and work offline. But unlike every other document format, an HTML file can't save changes back to itself. HTML Clay fixes that.

**How it works:**

1. Install HTML Clay (one time)
2. Double-click any `.htmlclay` file
3. It opens in a window — edit it however you like
4. Hit save — your changes write back to the same file on disk

No cloud. No accounts. No build step. The file is just HTML.

## Project goals

1. **Self-saving HTML files** — The file knows where it lives on disk. Edit and save without "Save As" dialogs or cloud storage.
2. **True portability** — A `.htmlclay` file works offline, locally, with no infrastructure. Email it, Dropbox it, AirDrop it, git it.
3. **Low barrier for developers** — Write HTML + a small JS save call + rename to `.htmlclay`. That's it.
4. **One-click OS integration** — File extension associates with the app. Double-click to open. No registration, no login.
5. **App-like experience** — Opens in a chromeless window that looks and feels like a native app, not a browser tab.
6. **Platform potential** — The same `.htmlclay` file could eventually run both locally and on a web hosting platform.

## What does a `.htmlclay` file look like?

It's just HTML. Here's a minimal example — a page with editable text and a save button:

```html
<!DOCTYPE html>
<html lang="en"><head>
  <meta charset="utf-8">
  <title>My Note</title>
</head>
<body>
  <h1 contenteditable>Edit me</h1>
  <button id="save">Save</button>
  <script>
    document.getElementById('save').addEventListener('click', async () => {
      const html = '<!DOCTYPE html>\n' + document.documentElement.outerHTML;
      const token = document.documentElement.getAttribute('htmlclaytoken');
      const res = await fetch('/_/save/' + token, {
        method: 'POST',
        headers: { 'Content-Type': 'text/html' },
        body: html
      });
      if (res.ok) document.title = 'Saved!';
    });
  </script>
</body></html>
```

The key line is `fetch('/_/save/' + token, ...)` — that's the save call. HTML Clay injects an `htmlclaytoken` attribute into the `<html>` tag when serving the file, and the page reads it to know where to save. The token is a cryptographic session identifier that maps to the file on disk.

## What can you build with it?

A `.htmlclay` file can be anything you'd build as a web page that benefits from being opened, edited, and saved like a document. Here are some ideas:

- Flashcard deck with spaced repetition
- Kanban board
- Weekly planner
- Habit tracker grid
- Recipe box
- Reading list with notes and ratings
- Markdown journal with daily entries
- Gratitude journal
- Vocabulary builder
- Meeting notes template
- Invoice generator
- Language phrase book
- Workout log
- Movie/TV watchlist
- Guitar tab editor
- Freelance time tracker
- Screenplay formatter
- Running log
- Blood pressure log
- Song lyrics organizer
- Poetry notebook with syllable counter
- Dream log
- Subscription tracker
- Envelope budgeting
- Bucket list
- SVG icon editor
- Packing list
- Snippet library
- Music practice log
- Podcast episode planner
- Setlist planner
- ASCII art canvas
- Changelog editor
- Gift ideas tracker
- Garage sale inventory
- Choose-your-own-adventure engine
- Moving checklist
- Interactive fiction editor

## Why does this need to exist?

Every other document format has self-saving figured out. Photoshop files open, edit, save. Word documents open, edit, save. Even macro-laden Excel spreadsheets — which execute arbitrary code — open, edit, save.

HTML can't do this because browsers run it in a sandbox designed for the web. But a file you downloaded and double-clicked isn't the web — you trust it the same way you trust a `.docx`. HTML Clay bridges this gap safely by running a tiny local server that handles file reads and writes.

For a deeper exploration of the problem and the landscape of existing solutions, see [brainstorm/blog-post.md](brainstorm/blog-post.md).

---

## Technical deep dive

### Architecture

HTML Clay is a Go application with a simple architecture: a localhost HTTP server that bridges the browser sandbox with the filesystem.

```
User double-clicks .htmlclay file
  → OS launches HTML Clay (registered handler for .htmlclay)
    → App generates a cryptographic session token for the file
      → App opens Chrome in app mode (chromeless window) or default browser
        → Browser loads file from localhost server
          → User edits, hits save
            → JS reads htmlclaytoken, calls POST /_/save/{token}
              → Server writes changes back to disk
```

### Server endpoints

| Method | Route | Purpose |
|--------|-------|---------|
| `GET` | `/{path}` | Serve a `.htmlclay` file with session token injected |
| `GET` | `/_/read/{token}` | Return raw file contents |
| `POST` | `/_/save/{token}` | Write updated HTML back to disk (atomic write) |
| `GET` | `/_/meta/{token}` | Return file metadata (path, size, modification time) |

Content is served at the top level; actions live under the `/_/` marker, matching the [Hyperclay](https://hyperclay.com) platform convention. The save endpoint accepts either a plain-text HTML body or a JSON `{content, snapshotHtml}` body (it persists `content`), so the same [hyperclayjs](https://www.npmjs.com/package/hyperclayjs) save client works against both htmlclay and the platform.

### Package structure

```
main.go              CLI entry point, orchestration
server/              HTTP server, request handlers, security middleware
session/             Cryptographic token generation, file↔token mapping
browser/             Chrome/Chromium detection, app-mode and browser-mode launch
htmlutil/            Inject/strip htmlclaytoken and htmlclayid attributes in <html> tag
config/              Persist settings to OS config dir (~/Library/Application Support, ~/.config, %APPDATA%)
platform/            Single-instance enforcement (Unix socket / TCP on Windows), Start on Login
tray/                System tray icon and menu
logging/             File-based logger with 10MB rotation
update/              Version check against htmlclay.com
dist/macos/          macOS .app bundle build script, Info.plist, codesigning
dist/linux/          Desktop entry, MIME type registration, install scripts
dist/windows/        File association registration script
```

### Security model

- **Localhost only** — The server binds to `127.0.0.1`, validates the `Host` header, and rejects cross-site requests (`Sec-Fetch-Site: cross-site`).
- **256-bit session tokens** — Each opened file gets a cryptographically random token. The read, save, and meta endpoints (under `/_/`) require a valid token; the top-level file-serving route only resolves paths that match an already-open file.
- **Path traversal prevention** — All file paths are validated as relative and within the user's home directory. Symlinks are resolved before validation.
- **Atomic writes** — Files are written to a temp file first, then renamed into place, preventing corruption on crash.
- **Single instance** — A Unix socket (or TCP on Windows) ensures only one server runs at a time. Additional launches forward their file paths to the running instance.

### Browser modes

- **App mode** (default): Opens in Chrome/Chromium with `--app` flag — a chromeless window that looks like a native app, with an isolated user profile.
- **Browser mode** (fallback): Opens in the system's default browser if Chromium isn't available.

### Configuration

Stored at `<os-config-dir>/htmlclay/config.json` (`~/Library/Application Support` on macOS, `~/.config` on Linux, `%APPDATA%` on Windows):

```json
{
  "mode": "app",
  "startOnLogin": false,
  "port": 54321
}
```

The port is auto-selected if the saved one isn't available.

### System tray

The app lives in the system tray with controls for:
- Switching between app mode and browser mode
- Toggling Start on Login (LaunchAgent on macOS, autostart desktop entry on Linux, registry key on Windows)
- A notification when a new version is available (click to open the download page)

### Building from source

Requires Go 1.26+. Linux and Windows build as pure Go (no system libraries needed); macOS builds use cgo for the system tray and Finder integration.

```bash
# Build the binary
make build

# Run tests
make test

# Build macOS .app bundle
make dist-macos

# Build Linux binary (prints install steps; CI assembles the tarball)
make dist-linux

# Build Windows executable
make dist-windows

# Clean build artifacts
make clean
```

### Platform support

Supports macOS, Linux, and Windows. Each platform has build scripts and OS integration assets in `dist/`. The `browser/`, `platform/`, and `tray/` packages use platform-specific build files (`_darwin.go`, `_linux.go`, `_windows.go`).
