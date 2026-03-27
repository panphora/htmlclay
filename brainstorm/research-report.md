# Self-Saving Single-File HTML: Complete Technical Research

## Baseline Constraints

### `file://` origins are broken by design

Browsers treat `file:///` documents as **opaque origins**. Same-origin assumptions break even between files in the same folder. This matters because storage, permissions, and security decisions are all origin-keyed.

**localStorage**: Can throw `SecurityError` on `file://`. MDN states behavior for `file:` URLs is **undefined and varies among browsers**. In current browsers it appears partitioned per individual `file:` URL with no guarantee. Treat as opportunistic, not foundational.

**IndexedDB**: Spec requires throwing `SecurityError` on opaque origins. If your `file://` page gets an opaque origin (common), IndexedDB fails outright.

**Secure context**: The W3C spec encourages treating `file:` as potentially trustworthy but says user agents may exclude it, and opaque origins are explicitly not trustworthy. MDN recommends feature-detecting with `window.isSecureContext`. Always plan for feature detection + fallback.

**Navigation restrictions**: Chromium blocks navigation to `file://` from non-file contexts. This breaks designs that bounce between local files or open them from web contexts.

### Chrome flags are a dead end

`--app=file:///path` opens a chromeless window but grants **zero** extra write privileges. `--allow-file-access-from-files` only relaxes CORS for reading (lets one local file fetch another) — it does not enable writing. `--disable-web-security` has no file-write effect. DevTools Workspaces/Overrides map network resources to local files but are manual developer features, not user-facing save mechanisms. **No combination of Chrome flags allows a webpage to save over itself.**

---

## Approaches

### 1. Blob Download (No Overwrite)

The HTML file embeds an editor (`contenteditable` for WYSIWYG or `<textarea>` for plain text) and a Save button that serializes the page and triggers a download via `Blob` + `<a download>`.

```js
const downloadHtml = (filename, html) => {
  const blob = new Blob([html], { type: 'text/html;charset=utf-8' });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  a.style.display = 'none';
  document.body.appendChild(a);
  a.click();
  setTimeout(() => { URL.revokeObjectURL(url); a.remove(); }, 1000);
};

document.getElementById('save').addEventListener('click', () => {
  downloadHtml('updated.html', document.documentElement.outerHTML);
});
```

**Variant — State embedding**: Instead of serializing the DOM, keep a stable app shell and embed user edits as JSON inside a `<script type="application/json">` tag. Export generates a new HTML with that embedded state. Produces cleaner, more consistent output.

| | |
|---|---|
| Works from `file://` | Yes |
| Overwrites same file | No — creates a new file in Downloads |
| Browser support | All major browsers |
| Offline | Yes |
| User friction | Low (edit → click Save) |
| Complexity | Low–medium |

**Limitations**: Download behavior varies (prompt vs auto-save, folder, filename). User may reopen the original and think changes were lost. `contenteditable` produces messy HTML from pasted content. `document.execCommand()` is deprecated.

### 2. File System Access API (Chromium Only, True Overwrite)

Use `showOpenFilePicker()` to get a `FileSystemFileHandle`, then write back via `createWritable()`. This is the closest browser-native equivalent to desktop Open/Save semantics.

```js
// Open
const [handle] = await window.showOpenFilePicker({
  types: [{ description: 'HTML', accept: { 'text/html': ['.html', '.htm'] } }]
});

// Save (overwrite)
const writable = await handle.createWritable();
await writable.write(document.documentElement.outerHTML);
await writable.close();
```

| | |
|---|---|
| Works from `file://` | Only if treated as secure context (varies) |
| Overwrites same file | Yes |
| Browser support | Chrome/Edge only. No Safari, no Firefox (as of March 2026) |
| Offline | Yes |
| User friction | Low–medium (must click to open picker, may see permission prompt) |
| Complexity | Medium–high (permissions UX, error handling, fallback logic) |

**Constraints**: Requires secure context. Must be triggered by transient user activation (a click) — lose activation and you get `SecurityError`. Permission persists only while tabs for the origin remain open. `showSaveFilePicker()` can clear an existing file before returning a handle — handle cancellation carefully. Writes use temporary files + security checks before replacing the original.

**Best practice**: Progressive enhancement — detect `showOpenFilePicker`, use it when available, fall back to Blob download.

### 3. localStorage / IndexedDB / OPFS Autosave + Export

Add autosave to protect against accidental tab close, but treat it as best-effort. Export still required to produce the transferable file.

```js
const safeSet = (key, val) => {
  try { localStorage.setItem(key, val); return true; } catch { return false; }
};
```

| | |
|---|---|
| Works from `file://` | localStorage: undefined/varies. IndexedDB: throws on opaque origins. OPFS: requires secure context. |
| Overwrites same file | No — drafts live in browser profile, not the file |
| Browser support | Varies — unreliable on `file://` |
| Portable | No — storage doesn't travel with the file |

**OPFS** (Origin Private File System): High-performance, origin-private storage. No per-file permission prompts, but requires secure context + stable origin (localhost or HTTPS). Subject to quota. Clearing site data deletes it. Safari has proactive eviction. `navigator.storage.persist()` can request persistence but may fail on opaque origins.

### 4. Browser Extension + Native Messaging (Cross-Browser, Proven)

A browser extension alone cannot write files. But paired with a **native host** (small installed binary), the extension relays save requests via `chrome.runtime.sendNativeMessage` to the host, which writes to disk.

| | |
|---|---|
| Works from `file://` | Yes (extension must be permitted on file:// pages) |
| Overwrites same file | Yes |
| Browser support | Chrome, Edge, Firefox (WebExtension native messaging). No mobile. |
| User friction | Medium (install extension + run one-time native host installer) |
| Complexity | Medium–high |

**Security model**: Split trust — extension follows browser rules, helper runs with OS permissions. Browser limits communication to the registered host. Extension must be explicitly enabled for `file://` URLs.

**Prior art**: **Timimi** — a working TiddlyWiki saver using this exact pattern. Cross-browser. The native host is installed once and stays idle until a save is requested. Older: **TiddlyFox** (Firefox XUL add-on, now defunct — modern browsers killed XUL).

### 5. Desktop App Wrappers (Electron, Tauri, Neutralinojs, NW.js)

Package the HTML inside a lightweight desktop runtime with native file I/O.

| Framework | Binary Size | Includes Browser? | File Access |
|---|---|---|---|
| Electron / NW.js | ~150–300 MB | Yes (Chromium) | Node.js `fs` |
| Tauri | ~5–10 MB | No (system WebView) | Rust `std::fs` |
| Neutralinojs | ~2–5 MB | No (system browser) | Native API |

| | |
|---|---|
| Works from `file://` | N/A — runs in its own context |
| Overwrites same file | Yes |
| Browser support | N/A — brings its own engine or uses system WebView |
| Cross-platform | Windows, macOS, Linux |
| User friction | Install the wrapper app once, then open/drag files |
| Complexity | Medium |

The app can register as the handler for a custom file extension. User double-clicks the file, wrapper opens it, JS calls the backend to save. The backend calls `fs.writeFile()` on the original path.

**Prior art**: **TiddlyDesktop** (NW.js), **TiddlyBob** (Electron) — dedicated single-file wiki wrappers. No generic "malleable HTML runner" exists yet.

### 6. Localhost Server Bridge (Any Browser, True Overwrite)

A tiny local server (`http://localhost:PORT`) serves the HTML and exposes a save API. The HTML's JS calls `fetch('POST /api/save', { body: html })` and the server writes back to the file. `http://localhost` is treated as a secure context by all major browsers, unlocking APIs that `file://` blocks.

| | |
|---|---|
| Works from `file://` | No — requires `http://localhost` |
| Overwrites same file | Yes |
| Browser support | Any browser (it's just HTTP) |
| User friction | Must start a server (high for non-technical users) |
| Complexity | Low (server) — high (packaging for non-technical users) |

**Packaged version**: Bundle the server into an installable app. Register as file handler for `.malleable` extension. User double-clicks → app starts server on random port → opens Chrome in app mode (`--app=http://localhost:{port}/?file={path}`) → server writes saves back to disk → shuts down when window closes. This is the KBTD pattern.

**Security**: Server binds to `127.0.0.1` only (not reachable from network). Must validate file paths (restrict writes to the opened file, not arbitrary paths). Use a per-session token to prevent other local pages from calling the save API. Bind to a random port to avoid collisions.

### 7. PWA + File Handling API (Most Promising Future Path)

A PWA declares `file_handlers` in its manifest for a custom extension. Once installed, double-clicking the file launches the PWA. JS accesses the file via `launchQueue` API, gets a `FileSystemFileHandle`, and uses `createWritable()` to save.

| | |
|---|---|
| Works from `file://` | No — PWA must be installed from a served origin |
| Overwrites same file | Yes |
| Browser support | Chrome/Edge only (as of March 2026). No Safari, no Firefox. |
| User friction | Install PWA once (one-time). Then double-click files. |
| Complexity | Medium |

**This is the closest to the wish list using only web standards**: install once, double-click to open, edit, save in place. The PWA can get persistent write permission until the app is closed. But it's bleeding-edge and Chromium-only.

### 8. Custom Protocol Handlers

Register a custom URI scheme (`malleable://`) at the OS level. When invoked, the OS launches a designated app with the file path as an argument. The app can start a localhost server, open the browser, and provide save-back.

- **macOS**: LaunchServices / `Info.plist` with `CFBundleDocumentTypes`
- **Windows**: Registry keys under `HKEY_CLASSES_ROOT`
- **Linux**: `.desktop` file with `MimeType` + `xdg-mime`

Browser warns user on external protocol launch ("External Protocol Request"). The handler runs as a local program with OS permissions. Must be carefully scoped to avoid arbitrary file access.

### 9. WebDAV / Cloud Sync

Put the HTML file on SharePoint/OneDrive, rename to `.aspx`, open through SharePoint. The browser saves via HTTP PUT / WebDAV. Works in any browser since it's standard HTTP. TiddlyWiki users in enterprise environments use this pattern. Essentially the "localhost server" pattern but with a remote server and cloud sync.

### 10. Legacy: Windows HTA

Microsoft's "HTML Applications" (`.hta`) ran local HTML with full privileges via IE's engine. Could use ActiveX / `Scripting.FileSystemObject` to read/write any file. A true self-writing HTML. Obsolete, IE-only, massive security risk. Proves the concept once existed natively.

---

## Comparison

| Approach | Overwrites Same File | Cross-Browser | No Install | Works Offline | User Friction |
|---|---|---|---|---|---|
| Blob download | No | All | Yes | Yes | Low |
| File System Access API | Yes | Chrome/Edge only | Yes | Yes | Low–Med |
| Extension + native messaging | Yes | Chrome/Edge/Firefox | No | Yes | Medium |
| Desktop wrapper (Electron/Tauri) | Yes | N/A (own engine) | No | Yes | Low (after install) |
| Localhost server bridge | Yes | All | No | Yes | Med–High |
| PWA + File Handling | Yes | Chrome/Edge only | No (PWA install) | Yes | Low (after install) |
| Custom protocol handler | Yes | All (launches app) | No | Yes | Medium |
| WebDAV / cloud sync | Yes (remote) | All | No (server needed) | No | Medium |

---

## Prior Art

**TiddlyWiki**: The canonical single-file HTML app. Originally used Java applets (Chrome) and XUL add-ons (Firefox) for saving — both now dead. TW5 uses the File System Access API on Chrome/Edge. Fallback is Blob download. Ecosystem includes TiddlyDesktop (NW.js wrapper), TiddlyBob (Electron), Timimi (extension + native messaging), TiddlyStow (bookmarklet using FS API), and the SharePoint/WebDAV trick.

**Timimi**: Fully working cross-browser saver for TiddlyWiki using extension + native messaging. Proves the pattern works in production.

**Windows HTA**: Historical proof that browser-native self-writing HTML once existed. Killed by security concerns.

**No generic "malleable HTML runner" exists.** Every solution is either TiddlyWiki-specific or requires building your own.

---

## Security

A self-editing HTML is analogous to a document with active macros. The risk: a malicious HTML could corrupt files or exfiltrate data. Self-writing HTML files (like TiddlyWiki) already trigger antivirus scanners.

**The risk isn't fundamentally greater than opening a desktop app.** Photoshop, Figma, Office — all execute code, all have had vulnerabilities, all are networked. The difference: HTML is an open standard with a vastly larger attack surface, and open standards bodies can't ship patches as fast as Adobe or Microsoft.

**Responsible permission model**: Any self-writing capability must involve explicit user consent. The FS Access API gets this right — every overwrite needs a user gesture + prompt, with a persistent indicator. Native wrappers should restrict writes to the opened file's path. Localhost servers need per-session tokens and path validation.
