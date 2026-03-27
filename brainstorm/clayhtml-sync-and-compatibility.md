# HTML Clay: Sync, Compatibility, and the Vision

## Original Prompt

> Look at htmlclay/ and hyperclay-local/, I would like them to be compatible
> in some ways, although they serve different purposes. The main issue right
> now is that appname is used differently by both systems. I think appname
> attribute should be used consistently to just be the canonical name of the
> app. It would be nice if I could put a .htmlclay file inside the
> hyperclay-local sync folder and have it do everything a regular html file
> would do there (be served by the server, do live sync, save, etc). I also
> was brainstorming something new here (file-sync-architecture.md), where
> basically I'd want each .htmlclay file to be able to sync with all other
> .htmlclay files that have the same name. I'd prefer to not have to use
> hyperclay-local for this, it should be its own system. The idea is an
> internet-connected document that is malleable, that you can share with
> other people, and that you update it it changes for everyone. It can use
> the same front-end code that livesync uses across hyperclay/ and
> hyperclay-local/ but it doesn't need to sync to the hyperclay/ platform,
> it can just sync with all other documents. So we'll need some server with
> some source of truth and do conflict resolution of LWW like I think we
> already do. HTML Clay is like a lighter-weight version of hyperclay-local
> for non-technical folks. They install it and forget about it. Then they can
> open their friends' .htmlclay files and they're automatically synced across
> machines. Pretty cool, huh?

**Decisions made during brainstorm:**

1. **Name-based sync rejected.** The original prompt mentions syncing "files
   that have the same name." Too finicky — people rename files, collisions,
   etc. Sync is purely UUID-based via `htmlclayid`.

2. **No universal ID.** We considered a single `clayid` that all three
   systems would share. Rejected — hyperclay and hyperclay-local already
   have working identity systems (`nodeId`, `appname`). No benefit to
   replacing them. Each system owns its own concerns.

3. **Systems are compatible by not interfering.** htmlclay uses `htmlclayid`
   on `<html>` and ignores `appname`. hyperclay/hyperclay-local use
   `appname` and ignore `htmlclayid`. If a `.htmlclay` file ends up in
   hyperclay-local, it gets `appname` injected like any HTML file, and
   `htmlclayid` just sits there harmlessly.

4. **htmlclay auth via cookie, not HTML attribute.** The session token goes
   in a cookie, not on `<html>`. No `htmlclaytoken` attribute needed.

5. **No changes to hyperclay or hyperclayjs.** hyperclay-local just needs to
   recognize the `.htmlclay` extension.

6. **Cross-system sync doesn't need backend integration.** If a file on
   hyperclay.com wants to sync via the htmlclay relay, the file's own
   frontend code connects to the relay directly. hyperclay's backend
   doesn't need to know the relay exists.

7. **File extension**: `.clayhtml` → `.htmlclay`. Repo renamed to match.
   Everything is `htmlclay*`.

---

## The Three Systems Today

### htmlclay (Go desktop app)

A minimal desktop app that makes `.htmlclay` files self-saving. User
double-clicks a file, it opens in Chrome app mode via a localhost server,
and the file can save itself back to disk via `fetch('/save/...')`.

- Written in Go
- No cloud, no accounts, no sync
- Runs as a system tray app with single-instance enforcement
- Atomic file writes, session tokens, localhost-only security

### hyperclay-local (Electron app)

A heavier desktop app that serves HTML files from a local folder and syncs
them bidirectionally with hyperclay.com.

- Written in Node.js (Electron + Express)
- Full cloud sync engine with LWW conflict resolution
- File watching (Chokidar), SSE streams, queue-based sync
- Tailwind compilation, backup system, directory listings
- Depends on a hyperclay.com account and API key

### hyperclay (the platform)

The hosted platform at hyperclay.com. Stores files in a database, serves
them on subdomains, handles user accounts, and acts as the relay for
livesync between browsers and between the platform and hyperclay-local.

---

## `appname` History and the Current Approach

### Background

`appname` was originally `currentResource`, set via a cookie. It was moved
to an `<html>` attribute because cookies on localhost would overwrite each
other across files — the attribute approach was a hardening.

On **hyperclay/hyperclay-local**, `appname` is the canonical filename. It's
used for routing (including custom domains), save endpoints, cookies, and
app identity. It works well. No reason to change it.

On **htmlclay**, `appname` was repurposed as a session auth token — a
256-bit random string injected on serve and stripped on save. This is a
completely different use. But since htmlclay has its own Go server and its
own save mechanism, the two systems never actually share this attribute at
runtime. There's no real incompatibility in practice.

### The fix: htmlclay stops using `appname`

htmlclay moves its auth token to a cookie. It doesn't inject `appname` at
all. The Go server sets the cookie when serving the file, and the browser
sends it back on save requests automatically.

```
Before:  <html appname="a8Kx3m...random-token...">
After:   <html htmlclayid="b3f7a2e1-...">
         + Set-Cookie: htmlclaytoken=a8Kx3m...; Path=/; SameSite=Strict
```

htmlclay doesn't read or write `appname`. hyperclay/hyperclay-local don't
read or write `htmlclayid`. They coexist on the same `<html>` tag without
interfering:

```html
<!-- A .htmlclay file opened in hyperclay-local -->
<html appname="my-notes" htmlclayid="b3f7a2e1-...">
```

hyperclay-local injected `appname`. `htmlclayid` was already in the file
from a previous htmlclay session. Neither system touches the other's
attributes.

### No changes to hyperclay, hyperclay-local, or hyperclayjs

These systems keep working exactly as they do today. `appname` stays.
`savePageCore.js` stays. The only hyperclay-local change: recognize the
`.htmlclay` file extension so these files can be dropped into the sync
folder and work like any `.html` file.

- File serving: add `.htmlclay` to recognized extensions
- File watching (Chokidar): add `**/*.htmlclay` to the glob
- Sync engine: treat `.htmlclay` as HTML for upload/download

---

## Problem 2: .htmlclay Files That Sync Across Machines (Without Hyperclay)

This is the bigger vision. A standalone sync system that doesn't depend on
hyperclay.com or hyperclay-local.

### The concept

A `.htmlclay` file is an internet-connected document. You send it to a
friend. They open it with HTML Clay (the Go app). When either of you edits
it, the change appears on the other person's copy. No accounts, no platform,
no configuration. Install HTML Clay, open the file, it syncs.

It's like Google Docs but:
- It's a real file on your disk
- You own it completely
- The app is invisible — it's just a system tray icon
- The file format is plain HTML
- Anyone can build their own editor/app as a `.htmlclay` file

### How it works: the user's perspective

1. Alice creates a `todo-list.htmlclay` file (or downloads one)
2. She opens it — HTML Clay serves it in Chrome, she edits it
3. She sends the file to Bob via email/AirDrop/whatever
4. Bob opens it — HTML Clay serves it, he sees Alice's content
5. Alice makes a change — Bob's copy updates in seconds
6. Bob makes a change — Alice's copy updates in seconds
7. They're both editing the same logical document

### Architecture

The existing `livesync-hyperclay` library and `hyperclayjs` frontend already
implement the browser-side sync protocol:

- **SSE stream** for receiving changes (`/live-sync/stream?file=name`)
- **POST endpoint** for sending changes (`/live-sync/save`)
- **HyperMorph** for DOM diffing (merge changes without losing focus/state)
- **Client IDs** so you don't receive your own changes back
- **Debounced sends** (150ms) to avoid spamming

The missing piece is a **relay server** that connects browsers across
machines. Currently the relay is either:
- The hyperclay-local Express server (for browsers on the same machine)
- The hyperclay.com platform (for browsers across machines, but requires an
  account)

We need a third option: a lightweight relay that requires nothing but a file
ID.

### File identity: `htmlclayid`

Each `.htmlclay` file gets a UUID stored as the `htmlclayid` attribute on
`<html>`:

```html
<html htmlclayid="b3f7a2e1-9c04-4d8b-af12-8e3b5c6d7f90">
```

- Assigned on first open (auto-generated by HTML Clay)
- Persisted to disk (never stripped on save)
- Travels with the file (email, git, zip, AirDrop — the ID is in the HTML)
- Read by the frontend: `document.documentElement.getAttribute('htmlclayid')`
- This is htmlclay's own identity system. It is not related to hyperclay's
  `nodeId` or `appname`. Those systems ignore it.

When Alice sends Bob the file, the UUID comes with it. Both copies have the
same ID. The relay server uses this ID to connect them.

### The relay server

A simple server (could be Go, Node, or a managed service) that:

1. Accepts WebSocket connections
2. Clients join a "room" by file ID
3. When a client sends a change, broadcast to all other clients in that room
4. Doesn't store anything — it's stateless
5. Doesn't understand HTML — it just relays opaque messages

```
Alice's browser ──WebSocket──→ Relay (room: b3f7a2e1...) ──WebSocket──→ Bob's browser
                                     ↕
                              Other clients with
                              the same file ID
```

The relay is NOT a source of truth. The source of truth is the combination
of all connected clients. When a new client connects:

- If other clients are already in the room, one of them sends the current
  state (a "full sync" message)
- If no one else is connected, the client just uses its local file

### Source of truth and conflict resolution

This is the interesting design question. There are two layers:

**Layer 1: Real-time sync (when multiple clients are connected)**

This is what livesync already does — SSE-based broadcast with DOM morphing.
The relay replaces the hyperclay.com server as the broadcast hub. Changes
are applied in real-time via HyperMorph. No explicit conflict resolution
needed because changes are small and frequent (keystroke-level or
save-level).

However, we should think about what format to relay:

- **Full HTML snapshots** (current livesync approach): Simple but heavy.
  Works fine for small files. Bandwidth-intensive for large ones.
- **DOM diffs**: Lighter but requires a diffing protocol. HyperMorph
  already does diffing on the receiving end, so sending the full HTML and
  letting the receiver diff is actually reasonable.
- **CRDT updates (Yjs)**: The gold standard for real-time collaboration.
  Each client maintains a CRDT document. Changes merge automatically without
  conflicts. The relay just passes opaque CRDT messages. Heavier initial
  setup but mathematically conflict-free.

**For v1: use full HTML snapshots** (same as current livesync). It works, the
code exists, and it's simple. Upgrade to Yjs later if needed.

**Layer 2: Offline reconciliation (when a client reconnects after being offline)**

This is where LWW (Last Write Wins) comes in. Scenario:

1. Alice and Bob are both editing, then Bob goes offline
2. Alice makes changes, her file is newer
3. Bob comes back online with his (older) local copy

Resolution:
- When Bob reconnects, the relay (or another connected client) sends the
  current state with a timestamp
- Bob's HTML Clay compares timestamps
- If the remote version is newer, replace local (Bob sees Alice's changes)
- If local is newer (Bob edited offline after Alice stopped), send local
  to the room (Alice sees Bob's changes)
- If both edited at similar times (within a buffer), the more recent
  timestamp wins (LWW) — same as hyperclay-local's `isLocalNewer()` with
  a 10-second buffer

This is simple and good enough for v1. True concurrent offline editing
(both Alice and Bob edit different parts while offline and want to merge)
would require CRDTs. But that's a v2 problem.

### What the relay server needs to persist

Wait — I said the relay is stateless. But for offline reconciliation, we
need a source of truth. Options:

**Option A: Relay stores the latest version**
- The relay keeps the most recent HTML snapshot per room
- When a new client connects, the relay sends the stored version
- Client compares with local, takes the newer one
- Simple, but now the relay is stateful (needs storage, backups, etc.)

**Option B: Relay is stateless, clients negotiate**
- New client connects to room
- If others are online: they send their current state
- If no one else is online: client uses local file (it's all they have)
- No server storage needed
- But: if Alice edits, closes her laptop, and Bob opens the file for the
  first time on a new machine, Bob has no way to get Alice's changes until
  Alice comes back online

**Option C: Relay stores a lightweight manifest, not content**
- The relay stores `{ fileId, lastModified, checksum }` per room
- When a client connects, relay says "the latest version was modified at
  X with checksum Y"
- Client compares with local and either requests the full content from
  another online client or knows their copy is current

**Recommendation: Option A** for v1. Keep it simple. The relay stores the
latest HTML per file ID. Storage is cheap (each file is <1MB typically).
A SQLite database or even a key-value store (Redis, file-based) works fine.
You can always make it lighter later.

This also solves the "first-time recipient" problem: Bob opens a file he
got from Alice. He connects to the relay. The relay already has Alice's
latest version. Bob gets it immediately, even if Alice is offline.

### Where does HTML Clay fit in this?

HTML Clay (the Go desktop app) becomes the invisible bridge:

```
.htmlclay file on disk
       ↕ (read/write via localhost server)
Browser (Chrome app mode)
       ↕ (WebSocket)
Relay server (rooms by file ID)
       ↕ (WebSocket)
Other browsers viewing files with the same ID
       ↕ (read/write via their local HTML Clay)
.htmlclay file on their disk
```

HTML Clay's responsibilities:
1. Serve the file locally (already does this)
2. Set auth cookie (`htmlclaytoken`) on serve
3. Inject `htmlclayid` on `<html>` if missing (persistent, write to disk)
4. Does NOT inject or touch `appname` (that's hyperclay's concern)
5. The frontend handles the relay connection
6. When changes arrive from the relay, the frontend triggers a save
7. HTML Clay writes the updated file to disk (already does this)

### What about hyperclay-local?

hyperclay-local stays as-is. It's the power-user tool for people who want
cloud sync with hyperclay.com, file watching, backup versioning, etc. The
two systems serve different audiences:

| | HTML Clay | hyperclay-local |
|---|---|---|
| Audience | Non-technical users | Developers |
| Install | Double-click installer, forget about it | Electron app, needs API key |
| Identity | `htmlclayid` (in-file UUID) | `appname` + `nodeId` |
| Auth | Cookie | N/A (localhost trust) |
| Sync target | Other .htmlclay files via relay | hyperclay.com platform |
| File format | .htmlclay only | .html (and .htmlclay after compat fix) |
| Features | Open, edit, save, sync | Edit, save, sync, backup, Tailwind, etc. |
| Cloud dependency | Relay server (minimal) | hyperclay.com (full platform) |
| Setup | Zero config | API key, folder selection |

A `.htmlclay` file works in both systems. Each system injects/reads its own
attributes and ignores the other's.

---

## Shared Frontend Code

### No changes to hyperclayjs

hyperclayjs (`savePageCore.js`, `live-sync.js`, `snapshot.js`) stays as-is.
htmlclay `.htmlclay` files have their own save mechanism (the Go server) and
don't use hyperclayjs for saving.

### What htmlclay can reuse later (for the relay)

When the relay is built, htmlclay's frontend sync code can borrow patterns
from the existing livesync system:

- **HyperMorph**: DOM diffing/morphing. Fully reusable.
- **Snapshot capture**: The pattern of capturing `documentElement.outerHTML`
  and broadcasting it. Same concept, different transport.
- **Client IDs, debouncing, dedup**: Same patterns apply.

The transport will be different (WebSocket to relay vs. SSE to
hyperclay.com), but the sync logic is the same. This can be new code in the
htmlclay repo that borrows ideas from livesync without depending on it.

### What stays separate

- **hyperclayjs** — hyperclay/hyperclay-local's frontend. Not touched.
- **livesync-hyperclay** — Server-side SSE pub/sub. hyperclay-specific.
- **Sync engine** — hyperclay-local's cloud sync. Not needed by htmlclay.
- **File watching** — htmlclay doesn't watch files; it only knows about
  files that are currently open.

---

## Implementation Roadmap

### Phase 1: Groundwork (current scope)

Changes to htmlclay only. No changes to hyperclay, hyperclay-local, or
hyperclayjs.

1. **Rename `.clayhtml` → `.htmlclay`** across the htmlclay repo
   - File extension references in Go code, tests, docs, build scripts
   - MIME type registrations (macOS plist, Linux desktop, Windows registry)
   - Test fixtures and sample files

2. **Move auth from `appname` to cookie**
   - Stop injecting `appname` as a token on `<html>`
   - Set `htmlclaytoken` cookie on serve instead
   - Update save handler to read token from cookie
   - Strip function no longer needs to strip `appname`

3. **Add `htmlclayid` (persistent UUID)**
   - `ReadSyncID()` and `InjectSyncID()` in `htmlutil/`
   - Inject on `<html>` if absent, write back to disk (one-time)
   - Never strip on save
   - Expose in `/meta/{token}` response

4. **hyperclay-local: recognize `.htmlclay` extension**
   - File serving: add `.htmlclay` to recognized extensions
   - File watching (Chokidar): add `**/*.htmlclay` to glob
   - Sync engine: treat `.htmlclay` as HTML

### Phase 2: Relay server (future)

Build the sync relay. Not in current scope.

1. Simple WebSocket server with room-based routing by `htmlclayid`
2. Store latest HTML snapshot per room (SQLite or flat files)
3. On connect: send stored version, client reconciles with local via LWW
4. On change: broadcast to room, update stored version
5. Deploy on a cheap VPS (Fly.io, Railway, etc.)

### Phase 3: Frontend sync integration (future)

Connect htmlclay's frontend to the relay. Not in current scope.

1. Build WebSocket sync client (borrows patterns from livesync)
2. Auto-connect to relay when `htmlclayid` is present on `<html>`
3. Handle online/offline transitions gracefully
4. Test with two machines editing the same file

### Phase 4: Polish (future)

- Tray menu: show sync status ("2 people viewing this file")
- Conflict indicator (if LWW overwrites local changes)
- Share button: copy a link that lets someone download the file + connect
  to the relay
- Optional: encryption (relay can't read the content)

---

## Open Questions (for future phases)

1. **Relay hosting**: Self-hosted Go server vs. managed service (Ably,
   Pusher, PartyKit)? Self-hosted is more control and cheaper at scale.
   Managed is faster to ship.

2. **Storage limits**: If the relay stores snapshots, what's the limit per
   file? Per user? Free tier?

3. **Authentication**: v1 is "anyone with the file ID can sync." Is that
   safe enough? The UUID is unguessable (122 bits of entropy), so it's
   similar to a Google Docs "anyone with the link" share.

4. **Offline duration**: How long does the relay keep a snapshot? Forever?
   30 days? Configurable?

5. **CRDT vs LWW**: LWW is fine for v1, but if two people are genuinely
   typing at the same time in different parts of the document, LWW will
   lose one person's changes. Yjs solves this. Worth building on Yjs from
   the start to avoid a painful migration later?

6. **Cross-system sync**: If a file lives on hyperclay.com and also syncs
   via the htmlclay relay, the file's own frontend code handles the relay
   connection directly. hyperclay's backend doesn't need to know. But is
   there a scenario where tighter integration would be valuable?

### Resolved

- **File discovery**: UUID is the only sync key. Name-based sync rejected.
- **Universal ID**: Rejected. Each system keeps its own identity. No need
  to pollute hyperclay/hyperclay-local with htmlclay concerns.
- **appname**: htmlclay stops using it. hyperclay keeps using it. They
  don't interfere.
