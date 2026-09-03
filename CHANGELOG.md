# Changelog

Releases before 1.9.0 are recorded in the ecosystem changelog at
[changelog.hyperclay.com](https://changelog.hyperclay.com).

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
  unfixable; the shared stream under Added is the fix.

### Added

- **One live-sync stream for every tab.** The `/_/sync` stream now carries any
  number of documents and lanes at once, listed as `s=lane:since:document-url`, each
  resuming from its own position, and the host serves a small worker at
  `/_/sync/worker.js` through which every tab on the origin shares that one stream.
  A browser allows six connections per origin and each tab used to hold one for the
  life of the page, so the seventh tab of one project never loaded. With ClayJS
  1.3.0 the tabs hold none. The one-document spelling still works for documents that
  vendor an older runtime.
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
