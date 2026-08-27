# The Malleable HTML File

A single HTML file is the simplest way to distribute small personal software. No install, no runtime, no dependencies. Double-click and it runs on any OS, any browser.

Sharing is just as easy — email it, AirDrop it, drop it in a folder. It works on the other end. Whether you wrote it by hand, generated it with an LLM, or exported it from a tool, the distribution story is the same: one file, universal.

But the recipient can't *edit* it. They can use the interface, but they can't save changes back to the file. Their edits live in browser memory or nowhere. The file can run a full application but can't write a single byte back to itself.

## The Problem

Every other document format has this figured out. Photoshop files open, edit, save. Word documents open, edit, save. Even macro-laden Excel spreadsheets — which execute arbitrary code — open, edit, save.

HTML is the most powerful document format ever created. It can render rich interfaces, run JavaScript, play media, make network requests. But it's the only format that can't overwrite itself.

Why? Partly because HTML runs in a browser sandbox — designed to protect users from the web. But `file://` isn't the web. You downloaded the file. You double-clicked it. You trust it the same way you trust a `.docx`. The browser doesn't care. It applies the same restrictions either way.

## The Solutions

### Level 0: Text editor

The obvious answer. Open the HTML file in a text editor, change what you want, save. It works — if you can read and write code. For anyone else, this isn't an option.

### Level 1: Blob download

The file can serialize its own DOM and trigger a download — `new Blob([html])` piped through an `<a download>` click. TiddlyWiki has used this for years.

The problem: it doesn't overwrite the original. It drops a new copy in your Downloads folder. Now you have two files and you're not sure which is current. Do this a few times and you have `notes.html`, `notes (1).html`, `notes (2).html`.

### Level 2: File System Access API

The browser actually has an API for this — `showOpenFilePicker()` returns a file handle, `createWritable()` writes back to it. True overwrite, no download duplicates. The user clicks a button, picks the file from a system dialog, and from that point the page can write back to it. VS Code for the Web, Excalidraw, TiddlyWiki, Photopea, and StackBlitz all use it.

The catches: Chromium only — no Firefox, no Safari. Requires a secure context, which `file://` may or may not be depending on the browser. Every write needs a user gesture. And the permission resets when you close the tab. It's the right idea with the wrong reach.

### Level 3: localStorage / IndexedDB

Autosave state in the browser between sessions. The file loads, checks for saved state, restores it.

The fatal flaw: the state lives in the browser profile, not in the file. Email the file to someone and your edits don't come with it. Clear your browser data and they're gone. The whole point of a single HTML file is portability — browser storage breaks that contract.

### Level 4: The TiddlyWiki / Lopecode approach

TiddlyWiki is the canonical single-file HTML app — a full wiki in one `.html` file, actively developed since 2004. Lopecode takes a different angle: a self-contained programming environment built on the Observable runtime that packages code, editor, and dependencies into one file.

Both are impressive engineering. They work on `file://` by avoiding its limitations rather than overcoming them. The runtime lives entirely in memory. Edits happen inside the browser's JS runtime. Persistence is either browser-local (IndexedDB, Dexie) or export — serialize the current state into a new HTML file and download it.

The result is the same as Level 1 with more sophistication. The user still ends up with a new file in their Downloads folder. The original doesn't change. TiddlyWiki has spent two decades building workarounds for this — browser extensions, native messaging hosts, Java applets (now dead), the File System Access API (Level 2) — and there's still no single solution that works everywhere.

### Level 5: Browser extension + native host

A browser extension alone can't write to disk. But paired with a native host — a small binary installed separately — the extension relays save requests to the host via `chrome.runtime.sendNativeMessage`, and the host writes the file. Timimi does exactly this for TiddlyWiki and it works.

The problem is the setup. The user installs an extension, then runs a separate installer for the native host, then enables the extension for `file://` URLs in browser settings. It's two installs and a config step. If the native host binary moves, gets cleaned up by an antivirus, or the extension updates with a breaking change, the connection silently breaks. It works — but it's fragile in the way that matters most: the user can't debug it when it stops.

### Level 6: Localhost server bridge

A tiny HTTP server runs on your machine, serves the HTML file at `http://localhost:{port}`, and exposes a save endpoint. The file's JavaScript calls `fetch('POST /save', { body: html })` and the server writes it back to disk.

`localhost` is a secure context in every browser — APIs that break on `file://` work here. The server runs as the current user, so it has the same file permissions you do. No extension, no native messaging, no browser-specific API. Any browser, true overwrite.

The catch: someone has to start the server. For a developer, that's nothing. For anyone else, it's a non-starter.

### Level 7: Package it

Take the localhost server from Level 6 and bundle it into an installable app. Register a custom file extension — `.malleable`, `.htmlapp`, whatever. The OS associates it with your app. User double-clicks the file, the app starts the server, opens the browser, and handles save requests. The user never sees a terminal, a port number, or a URL.

This is what TiddlyDesktop (NW.js) and TiddlyBob (Electron) do for TiddlyWiki. But they're TiddlyWiki-specific. A generic "self-saving HTML runner" — one that works with any HTML file — doesn't exist yet.

## The Landscape

| Approach | Overwrites original | Cross-browser | No install | Works offline |
|---|---|---|---|---|
| Text editor | Yes | N/A | Yes | Yes |
| Blob download | No | All | Yes | Yes |
| File System Access API | Yes | Chromium only | Yes | Yes |
| localStorage / IndexedDB | No (state in browser) | Unreliable on file:// | Yes | Yes |
| TiddlyWiki / Lopecode | No (export) | All | Yes | Yes |
| Extension + native host | Yes | Chrome/Edge/Firefox | No | Yes |
| Localhost server | Yes | All | No | Yes |
| Packaged app | Yes | All | No (one-time) | Yes |

What exists today is mostly the TiddlyWiki ecosystem: TiddlyDesktop, TiddlyBob, Timimi, TiddlyStow, and the various save adapters the community has built over twenty years. Lopecode takes a different architectural approach but lands in the same place. Windows had HTA files in the early 2000s — literally self-saving HTML with full system access — but they were killed for the obvious security reasons.

No one has built the generic version. Every solution is either wiki-specific or requires assembling your own stack.
