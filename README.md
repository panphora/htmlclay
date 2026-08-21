# HTML Clay™

> **License: Clay License.** Free for almost everyone: unless products and services that contain this software, derive from it, or provide its functionality by running it bring you more than $1M in a calendar year, you owe nothing, sign nothing, register nowhere.
>
> | Free under | Above it | Becomes MIT |
> |---|---|---|
> | $1M/year covered revenue | 3% of the excess | 2028-02-20 (this version) |
>
> Plain answers: [hyperclay.com/host-program](https://hyperclay.com/host-program) · binding text in [LICENSE](LICENSE) · questions: license@hyperclay.com

A desktop app that makes self-saving HTML files a native OS feature.

- **Website:** [htmlclay.com](https://htmlclay.com)
- **File extension:** `.htmlclay`
- **Format:** a [malleable HTML file](https://malleablehtmlfile.com)
- **Parent platform:** [Hyperclay](https://hyperclay.com)

### Three names, one idea

Worth stating once, because the three get mixed up:

- **The format** is a **malleable HTML file**, and its real extension is `.html`. It is specified at
  [malleablehtmlfile.com](https://malleablehtmlfile.com) and several programs can host one.
- **HTML Clay** is *this app*. It is one of those programs, not the format.
- **`.htmlclay`** is an operating system convention. It tells your OS to open the file with HTML Clay,
  the same way `.psd` names Photoshop rather than naming the image format.

Rename any `.htmlclay` file to `.html` and it opens in any browser, unchanged. Nothing about the file
depends on this app.

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
5. **A project is one place** — Trust a folder and every `.htmlclay` file in it opens editable at a stable local address that survives a restart, so links between them work and a bookmark keeps working.
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

## Let a program edit the file you're looking at

A page open in HTML Clay can ask a program on your machine to change the file it is running from, and watch the change arrive. You point at a paragraph and say "make this shorter", and an AI agent, a script, or a formatter running in your own terminal does it.

```bash
htmlclay wire serve ~/notes/page.htmlclay -- ./my-agent.sh
```

No HTML travels between the two: the page sends a small request, your program edits the file, and the edit reaches the page as an ordinary file change. The file is the only thing they share, so nothing needs an account, a service, or a protocol you have to trust.

See [`docs/wire.md`](docs/wire.md) for the page API, the handler contract, the CLI, and the security model.

## Why does this need to exist?

Every other document format has self-saving figured out. Photoshop files open, edit, save. Word documents open, edit, save. Even macro-laden Excel spreadsheets — which execute arbitrary code — open, edit, save.

HTML can't do this because browsers run it in a sandbox designed for the web. But a file you downloaded and double-clicked isn't the web — you trust it the same way you trust a `.docx`. HTML Clay bridges this gap safely by running a tiny local server that handles file reads and writes.

For a deeper exploration of the problem and the landscape of existing solutions, see [brainstorm/blog-post.md](brainstorm/blog-post.md).

---

## Security

HTML Clay only saves files you chose: a file you opened yourself, or a file inside a folder you have trusted. A page can only read inside the folder you opened it from; anything outside that pauses and asks. [`SECURITY.md`](SECURITY.md) explains the model, what each protection covers, and the current known limitations.

## Technical deep dive

### Architecture

HTML Clay is a Go application with a simple architecture: a localhost HTTP server that bridges the browser sandbox with the filesystem.

```
User double-clicks .htmlclay file
  → OS launches HTML Clay (registered handler for .htmlclay)
    → App picks the file's origin: the trusted folder containing it, else its own folder
      → App binds that origin's remembered port and mints a session token for the file
        → App opens the default browser
          → Browser loads file from localhost server
            → User edits, hits save
              → JS reads htmlclaytoken, calls POST /_/save/{token}
                → Server writes changes back to disk
```

Every trusted folder's port is bound again at startup, before any file is opened, so an address
bookmarked before the last quit still answers. An address HTML Clay remembers but is not serving
answers with a fixed recovery page that holds no permissions at all.

### Server endpoints

| Method | Route | Purpose |
|--------|-------|---------|
| `GET` | `/{path}` | Serve a `.htmlclay` file with session token injected |
| `GET` | `/_/read/{token}` | Return raw file contents |
| `POST` | `/_/save/{token}` | Write updated HTML back to disk (atomic write) |
| `GET` | `/_/meta/{token}` | Return file metadata (path, size, modification time) |
| `GET` | `/{path}?data={…}` | Extract JSON from the file using rules you supply |
| `GET` | `/_/api/{path}` | Extract JSON using rules the file publishes itself |

Content is served at the top level; actions live under the `/_/` marker, matching the [Hyperclay](https://hyperclay.com) platform convention. The save endpoint takes the document as a plain-text body. That is the one shape the format defines for it, so a JSON body is refused with `415` rather than guessed at, and anything a save needs to say beyond the document travels in a header.

### Reading a file as JSON

Any `.html` or `.htmlclay` file HTML Clay will serve can also be read as JSON, using the same
extraction rules as the [Hyperclay](https://hyperclay.com) platform. Two ways in:

```bash
# You supply the rules
curl 'http://localhost:PORT/notes.htmlclay?data={title:"h1",items:".todo[]"}'

# The file supplies its own, from a tag inside it
curl 'http://localhost:PORT/_/api/notes.htmlclay'
```

The second reads a tag the page publishes:

```html
<script data-rules-name="api" data-rules-version="1">
  {title: "h1", items: [".todo", {text: ".", done: "@data-done"}]}
</script>
```

Rules are relaxed JSON: unquoted keys and bare selectors are fine. A rule is a CSS selector, with
`sel[]` for every match's text, `sel@attr` for an attribute, and `[sel, {…}]` for one object per
match.

**A data request reads exactly what a normal request reads, and nothing more.** It runs the same
permission checks in the same order, asks the same folder-access question, and returns the same
refusal. It never creates an editing session: no save token, no edit-mode cookie, and no version
snapshot. There is no CORS header on either route, so only a program you run yourself, such as
`curl`, or a page on this same site can read it.

#### Where this differs from the platform

Deliberate, and each one measured against the platform's own engine rather than assumed.

| Behavior | HTML Clay | Platform |
|---|---|---|
| A selector the two engines read differently | refused with a reason, e.g. `:is`, `:empty`, `:gt`, `:matches`, CSS comments, the `[a=b s]` flag | answered, sometimes differently |
| A positional that is not last, e.g. `li:first span` | refused | answered |
| Any broken selector | `400` | `400` or `500`, depending on the wording of the parser's message |
| `@type` on an element | the `type` attribute | `"tag"`, the internal node type |
| `@readOnly` | the real value | always `false` |
| `sel@href[]` | refused, naming the array form that works | silently `[]` |
| A repeated `?data=` parameter | the first one wins | `500` |
| Caching | none | five minutes |
| CORS | none | enabled |

The full ledger, with the measurement behind every row, is in
`dataapi/testdata/selector-parity.json`: one list of constructs that must match the platform exactly,
and one of constructs HTML Clay refuses, each recorded with the answer the platform gives so the cost
of the refusal is written down rather than guessed at.

### Package structure

```
main.go              CLI entry point, startup, shutdown
sites.go             The site registry: which origin owns a file, and its lifecycle
folders.go           The declared trusted-folder list and the flows that change it
recovery.go          A remembered port bound with no capability at all
open.go              Opening a file and handing it to the browser
server/              HTTP server, request handlers, security middleware
session/             Cryptographic token generation, file↔token mapping, held read roots
trust/               The rules about which folders may be trusted, and on whose say-so
browser/             Opening a URL in the system's default browser
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

- **Localhost only** — The server binds to `127.0.0.1`, validates the `Host` header, and rejects cross-site requests (`Sec-Fetch-Site: cross-site`). Mutating routes (save, restore, live sync, permission requests) additionally require `Sec-Fetch-Site: same-origin` and a matching `Origin`.
- **Read-only by default, two ways to editable** — A linked or typed `.htmlclay` file serves read-only with a banner offering to trust its folder (a single-use server-minted nonce plus a native dialog). A trusted folder auto-registers its `.htmlclay` files as editable on real navigations, with no prompts. Save tokens are injected only into document navigations, never into background fetches.
- **256-bit session tokens** — Each opened file gets a cryptographically random token. The read, save, and meta endpoints (under `/_/`) require a valid token; the top-level file-serving route only resolves paths that match an already-open file. Tokens are redacted from the log and live for the lifetime of the process (there is no per-file expiry); on a single-user desktop this is fine, since they never leave loopback.
- **Path traversal prevention** — All file paths are validated as relative and within the user's home directory. Symlinks are resolved before validation.
- **Atomic writes** — Files are written to a temp file first, then renamed into place, preventing corruption on crash.
- **Single instance** — A Unix socket (or TCP on Windows) ensures only one server runs at a time. Additional launches forward their file paths to the running instance.
- **Local trust boundary** — The bridge listens on loopback, so any process running as your user can reach `127.0.0.1:<port>`. Saving requires the per-file token, but the top-level serve route returns a currently-open file by path with no token, so a malicious local process (or a page you already have open) can read other open documents. This is inherent to the localhost-bridge model: htmlclay only serves files you have explicitly opened, and nothing is exposed off the machine.

### Configuration

Stored at `<os-config-dir>/htmlclay/config.json` (`~/Library/Application Support` on macOS, `~/.config` on Linux, `%APPDATA%` on Windows):

```json
{
  "startOnLogin": false,
  "sitePorts": {
    "/Users/you/projects/notes": 54321
  },
  "workspaceFolders": [
    { "path": "/Users/you/projects/notes", "identity": "16777229:224754543" }
  ]
}
```

`sitePorts` remembers the port each origin was served on, keyed by its anchor folder, so an address
survives a restart. A port that is taken at startup is given up and the new one recorded instead.

The trusted-folder list keeps the on-disk key `workspaceFolders` from the version that introduced it.
Renaming the key would make older configs fail to parse, and the corrupt-config path would then reset
every other setting. `identity` is the folder's device and inode fingerprint at the moment you trusted
it; a folder replaced since then stops granting anything rather than covering the newcomer.

### System tray

The app lives in the system tray with controls for:
- Trusted Folders: trust one through a folder picker, or click a row to stop trusting it
- Opening the example file and the backups folder
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

### Releasing

```bash
./scripts/release.sh --minor   # or --major, or --patch (the default)
```

That bumps the version in `main.go`, tags and pushes, triggers the CI release workflow (test on three platforms, sign, notarize, upload to R2), publishes the website, and installs the new build into `/Applications`.

Three things about this repo are easy to trip over.

**The website deploys itself on every push to `main`.** htmlclay.com is a Cloudflare Worker fed by a git integration, not by CI and not by a manual `wrangler deploy`. Any push to `main` redeploys `website/` about 25 seconds later. There is no deploy command to run, and adding one only races the integration.

**Download links are stamped after CI, not during the version bump.** `scripts/stamp-website.js` writes the version into `website/index.html`, anchored on the `data-version` and `data-mac-dmg` attributes, so new spots on the page need no change to the script. Stamping during the bump would auto-deploy links to a dmg that CI has not uploaded yet, leaving them broken for the few minutes a build takes.

**`htmlclay-release-info.json` on R2 is not a website file.** It is the feed the in-app update checker polls, and its URL is compiled into every shipped binary (`update/update.go`). Removing it would silently and permanently break update checks for installs already in the wild.

To reinstall the current release locally without cutting a new one:

```bash
bash scripts/install-local.sh          # version from main.go
bash scripts/install-local.sh 1.1.0    # or an explicit version
```
