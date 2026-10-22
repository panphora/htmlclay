# Changelog

Releases before 1.9.0 are recorded in the ecosystem changelog at
[changelog.hyperclay.com](https://changelog.hyperclay.com).

## [Unreleased]

## [1.13.7] - 2026-10-22

### Added
- HTML Clay advertises the page extension, so editors mount a writable Yjs page on this host.

### Changed
- Saving a document that holds a converted page with a hybrid, mixed or missing page capture is refused with the same error codes as hosted Hyperclay, and nothing is written.
- A hybrid page converts only with a valid `source=file` Page-Conversion header and one current `If-Match` stamp.
- Saves and data writes no longer store the editor's `merge="page"` tag. Open tabs keep sharing the page through their tab-hosted room, and a fresh open rebuilds the page from its article.
- A save without the `merge="page"` tag counts as an outside edit, and a stored page document saved with its tag is an ordinary save rather than a first conversion.

## [1.13.6] - 2026-10-17

### Added
- **The data API reads rules that start with `:scope`.** A nested rule such as
  `:scope > section.checklist` now matches the rows directly inside its context, as ClayJS reads it,
  so the Kanban template's data opens in HTML Clay instead of failing with "Invalid selector".

### Fixed
- **An empty attribute reads as `""`, and writing `null` removes it.** The data API matches
  ClayJS again: a present but empty attribute such as Kanban's `data-done=""` used to read as `null`
  and could not be written back.

## [1.13.5] - 2026-10-15

### Added
- **`htmlclay update-now` asks the running copy of HTML Clay to update now.** It answers only the app that is the same install, on macOS it starts a Sparkle check and install, and the app advertises the command with `HCUpdateNowCommand`.

### Changed
- **A running app is never replaced by an update it refused, after a rollback, or by an older version.** An install interrupted halfway leaves the next run able to finish the swap.

## [1.13.4] - 2026-10-15

### Added
- **HTML Clay updates itself on Windows and Linux.** A background check verifies a signed update feed, downloads and stages the new version, and the tray row becomes Restart to Update.
- **On macOS, HTML Clay updates itself through Sparkle.** The tray offers Restart to Update once the update is ready, and falls back to the download link when Sparkle is missing or cannot update this copy.
- **An update restart keeps saves and open tabs.** Every open document comes back at the address it had, with its save token and grants, and a write that arrives during the restart is answered with a retry.
- **An update that fails to start puts the previous version back.** That version is skipped from then on, and the open sessions move back with it.

## [1.13.3] - 2026-10-14

### Changed
- **Folder addresses are htmlclay, htmlclay1, htmlclay2, and the default port is 1212.** The 1.13.1
  addresses, such as `notes-3f2a.localhost:24190`, become `htmlclay.localhost:1212`,
  `htmlclay1.localhost:1212` and so on, in the order folders are first served. The move happens once, on
  the first launch of this version; bookmarks to the 1.13.1 addresses stop working and page data stored in
  the browser under them starts over.
- **If every default port is taken, HTML Clay picks a random free port** between 10000 and 32767 and keeps it,
  instead of quitting. The defaults are 1212, 4545, 9797, 3434 and 1111.

## [1.13.2] - 2026-10-12

### Added
- **A program that is not a browser reaches a folder's data API at `127.0.0.1:<port>` without knowing the folder's hostname.** It names the file by the home-relative path the address bar shows, and a path no open folder serves answers `404` with a message saying no folder is open.
- **`htmlclay wire --port` finds the folder when it is given HTML Clay's own port**, instead of asking for the folder's hostname.

## [1.13.1] - 2026-10-12

### Added
- **One port, an address per folder.** HTML Clay serves every folder from one port, 1212 unless it
  is taken, at an address such as `http://htmlclay.localhost:1212/`. The address is chosen once per folder
  and never changes, so bookmarks survive restarts and opening a file again lands on the same page
  data. If another program holds the port at launch, a dialog names it and offers to quit it, move
  HTML Clay to a new port, or quit.

### Changed
- **Old `127.0.0.1:<port>` bookmarks redirect to the new address while their port is free.** Browser
  storage written under an old address does not carry over; the first launch says so once.
- **`htmlclay wire` takes `--origin` for an HTML Clay address**; `--port` is for Hyperclay Local.
- **Links between folders work under the default referrer policy**; a page that sets `no-referrer`
  cannot link to another folder.
- **The format is called a malleable document** in HTML Clay's own copy, with the old name left in addresses, identifiers and history.

### Note
- **Downgrading to 1.13 drops `port`, `siteSlugs` and `retiredSlugs` from `config.json`** on its next
  save (unknown fields are discarded), so the addresses are chosen afresh on the next upgrade.

### Fixed
- **A double-clicked file or a Start on Login launch no longer opens a terminal window on Windows.** `htmlclay wire` still prints in a terminal.
- **PowerShell dialogs, the browser launch and an AI editing agent no longer open console windows of their own on Windows.**
- **Dialogs and prompts come to the front on Windows**, including the error a forwarded open shows and the Trust this folder prompt, which now uses the real button labels.
- **Trust a Folder on Windows opens the modern Explorer-style picker**, a browser navigation to a path outside the folder gets a plain `403` page instead of a read prompt naming the page, and Stop Trusting labels its refusing button Cancel.
- **A save on Windows replaces a file another program has open.**
- **Exporting a file confirms the zip with an ordinary notice**, and says something only when the Downloads folder cannot be opened.

## [1.13.0] - 2026-10-08

### Added
- **Export an open file and its attachments as a zip.** A tray submenu lists every open document; choosing one writes `<name>.zip` to Downloads, holding the document and its `assets-<name>` folder under one folder, and opens Downloads.
- **Uploads live in `~/htmlclay/uploads` and every site serves them at `/_/uploads/`.** A document links its files by host path, so they stay with it when it moves, and a zip export carries them along.
- **A profile name that travels with your files.** Profile in the tray turns on Use My Profile in Files and sets the name a document reports under `document.people.me` in `/_/meta`, and the id it mints survives renames and toggles.
- **The Programs menu lists every open document's helpers with their state**, such as allowed, approval needed, denied for this open, or a program file that has gone missing.

### Changed
- **A bookmark to a remembered port now lands on the folder's current address.** A navigation to a port that no longer serves the folder redirects to where it is served now, and a folder that is no longer trusted keeps its old address on the recovery page.
- **An uploaded file is never served back as a page.** Uploads refuse web-page and script types, an attachment is sent with `nosniff` and an explicit type, and a page type found inside an `assets-` folder is handed over as a download.
- **The uploads library is never served as a page**, its names are keyed per folder so a name learned in one folder says nothing about another's, and a symlinked `~/htmlclay` no longer exposes it.

### Fixed
- **Start on Login works on macOS.** The LaunchAgent is written as plain text, so launchd accepts it and the app starts at login.
- **A file of exactly the advertised 25 MiB is accepted on upload**, and an upload never lands half written: bytes go to a hidden temporary file and are published in one step inside the document's folder.
- **An upload's token stays out of the access log.**
- **Configure an Open File picks the registration whose name matches** when several share the chosen program.

## [1.12.0] - 2026-10-04

### Added
- **AI editing names the agent it runs and falls back when the default program is missing.** The
  first status names Claude Code, Fable or Codex, and requests that name no agent use the other
  built in program if the built in default is unavailable; explicit agents never fall back.
- **A served document names the version it was served from** in a `documentetag` attribute on `<html>`, so a tab can tell the version it opened from the one on disk now. The attribute is response metadata and is never written to the file.

## [1.11.0] - 2026-10-03

### Added
- **A write can carry its own rules.** Sending JSON to a document's page address with a `data` parameter writes through those rules instead of the document's `api` rules tag, and sending `data` twice is refused with a message asking for one.
- **`/_/meta` announces `data-read` and `data-write`**, so a caller can see the JSON API is available before trying it.

## [1.10.1] - 2026-10-01

### Added
- **AI editing is built in.** Select text in any HTML file that loads ClayJS, press ⌘J, and say
  what you want: HTML Clay runs Claude Code, Fable or Codex on your computer with no tools, and the
  rewrite appears in place until you keep or revert it. It is on by default, needs no declaration
  or approval prompt, and never takes the document's handler slot. Turn it off with **AI Editing**
  in the tray. A request needs the document's `Save-Token`, one runs per document at a time, and
  `@name.ext` context stays inside the document's folder. agy is refused, because it can read files
  without asking.
- **AI editing keeps `@file` context to ordinary files in the document's folder**, refusing dotfiles, dot folders and HTML Clay's own state, by name and after symlinks.
- **An AI editing reply stops at 4 MiB.**

## [1.10.0] - 2026-09-28

### Added
- **`POST /_/api/<file>` writes JSON into a document.** The body is applied through the page's
  own `api` rules tag, the same map `GET /_/api` reads with, and only content is written: text as
  text, and attributes that cannot run script. Only the changed elements are rewritten, so every
  other byte of the file stays as it was. The write is versioned and reaches open tabs like a save.
  A local program needs nothing extra, and a browser page must send the file's `Save-Token`.
- **The data API sends an `ETag`**, and a write carrying `If-Match` is refused with `412` when
  the file changed since it was read.
- **Trusted folders stay trusted across a macOS reboot.** Folders are pinned by volume and inode rather than by the device number macOS renumbers on mount, and a trusted folder whose folder is gone answers with a page saying it needs approving again.

### Changed
- **The document-programs prompt no longer remembers a refusal.** Deny, Return, Escape, closing it or letting it time out applies to that open only, and the next double click asks again. Refusals stored by earlier versions are dropped. If exactly one program with that name is allowed for any document, a document that refused it now uses it without asking. The prompt shows the program path behind each name and asks which one when several share a name.
- **`no-data` and `editor-ui` regions are invisible to the data API**, on reads as well as writes,
  matching hyper-html-api.
- **A browser page proves it holds the file's `Save-Token` before the data API looks the file up**, and every failure answers the same way, so a page cannot tell a missing file from one it may not write. A write against a read-only rule answers `400`.

### Fixed
- **Data API writes land in documents that start with a byte order mark, and in SVG or MathML markup**, whose camelCase names now pair case-insensitively.

## [1.9.1] - 2026-09-24

### Changed
- **`htmlclay wire` reads the folders Hyperclay Local serves from where that app actually publishes them**, and looks past a remembered folder that does not serve the file, so a file still reaches the app that serves it.

## [1.9.0] - 2026-08-30

This release makes HTML Clay speak the Malleable HTML File protocol: it announces what
it can do, stamps what it stores, and refuses a save that would land on top of someone
else's. It is also the compatibility half of a change in the client libraries, so read
the first entry below before upgrading either side.

### Changed

- **The save token is injected under both names, `savetoken` and `htmlclaytoken`.**
  `savetoken` is the name spec §9 gives it and the only name ClayJS 1.2.0 and
  HyperclayJS 1.38.0 read. 1.8.0 and earlier injected only `htmlclaytoken`, so a
  document served by an older HTML Clay looks editable to a current client and every
  save fails. **Upgrading to 1.9.0 is the fix for every document at once**, and serving
  both names means a document opened in an older client keeps working too.
- **A document's durable identity is written as `documentid`.** `htmlclayid` was a
  product name in a file format that outlives the product. The old spelling is still
  recognised and removed on save, so a document written by an earlier version is not
  left carrying two identities.
- **The 415 on a save that is not a document answers `unsupported-type`.** It was
  `unsupported-media-type`, which is in the spec's registry nowhere; the upload route
  already answered the registered name.
- **The six-tab limit is gone.** It was a browser limit the host documented as
  unfixable; the shared stream under Added and the matching ClayJS client integration are the fix.

### Added

- **One live-sync stream for every tab.** The `/_/sync` stream now carries any
  number of documents and lanes at once, listed as `s=lane:since:document-url`, each
  resuming from its own position, and the host serves a small worker at
  `/_/sync/worker.js` through which every tab on the origin shares that one stream.
  A browser allows six connections per origin and each tab used to hold one for the
  life of the page, so the seventh tab of one project never loaded. With ClayJS
  shared-worker support, tabs share one connection. The one-document spelling still works for documents that
  vendor an older runtime.
- The `sync-worker` compatibility marker identifies workers that preserve separate peer and disk updates across notifications and invalidate cached content after a replay gap. Clients can keep the direct transport on older hosts.
- **The wire stream opens with a cursor for pages**, so a page whose stream drops
  before any frame arrives can still resume from a position rather than from nothing.
  Processes tailing the wire receive no cursor.
- **`/_/meta` discovery.** A page can ask what the host supports rather than probing
  for it: the protocol version, the two scopes, and the capability list, which on this
  host is `conditional`, `receipts`, `sync` and `upload`. `format` is deliberately absent: this host
  stores the bytes it was sent, so it ignores `formathtml` entirely, and §4 says a host
  that does not declare `format` is telling every client exactly that. The `document`
  block is withheld by omission rather than by an error, per §5.
- **Save receipts.** A save may carry a `Save-ID`, an opaque id a client mints for that
  one attempt, and this host remembers which id belongs to the bytes it currently stores.
  It reports that id as `saveId` in `/_/meta`, on every accepted save, and on every 412.
  A client whose save timed out can then ask the one question it cannot answer alone,
  whether its own write is what landed, instead of guessing and either overwriting
  somebody else's work or showing a conflict notice over a few seconds of bad wifi.

  The id is remembered beside the stamp of the bytes that save wrote and is reported
  only while that stamp still matches what is on disk. So a write from anywhere else, a
  text editor, a git checkout, an agent on the filesystem, invalidates it by moving the
  bytes, with nothing having to notice. An id is never minted here and never accepted as
  authorization for anything; the host only ever echoes what a client sent.
- **Conditional saves.** Every save answer carries an `etag`, and a save sent with
  `If-Match` is refused with 412 and a `conflict` code, nothing written, when the
  document changed underneath it. The stamp is computed over the bytes on disk rather
  than over the bytes the host was sent, since those differ whenever a host normalises
  and stamping what was sent would tell a client its disk holds something it does not.
- **`changedBy` on the conflict refusal**, naming what moved the file when the host
  actually knows (`another-tab`, `another-person`, `an-agent`) and omitted when it does
  not. A host must not guess: a plain filesystem write has no author.
- **The live-sync relay forwards `etag` on the editor lane.** The saving browser sends
  the stamp along with the bytes it just saved, so a receiving tab adopts a stamp only
  as part of applying the content that stamp describes. The viewer lane drops it, since
  a viewer holds a whole document rather than a pre-strip snapshot and has no save to
  condition.
- **A disk change carries the stamp of the bytes it delivers.** When the watcher sees
  a file edited outside the browser, the notification it sends the editors now carries
  that version's `etag` alongside the content. A tab used to apply the change and then
  ask `/_/meta` for a replacement stamp, which answers about whatever is stored at that
  later moment: if a second write landed in between, the tab adopted a stamp for bytes
  it had never seen and overwrote them on its next save. The stamp is taken from the
  bytes on disk rather than from the rendering sent to the browser, so it matches what
  a later `If-Match` is compared against. The viewer lane still carries no stamp.
- **A favicon**, so an opened document is identifiable in a row of tabs.
- **The protocol conformance page ships with the host** (`testdata/conformance/`) and
  runs in CI on every push, so a release cannot quietly stop conforming.
- **`website/llms.txt`**, a full reference for agents reading the site.

### Fixed

- A refusal the host answers itself carries a readable `msg`, and invents no `code`
  outside the spec's registry. The 409 on a truncated write used to answer a `conflict`
  code, which reads to a client as a stale-stamp conflict it can retry past, rather
  than as a write it must not repeat.
