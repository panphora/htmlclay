# File Sync Architecture: Unique IDs & Live Cross-Platform Syncing

## Problem

We want a user to update a `.htmlclay` file on their system and have a friend receive
a live update of that change on their own copy. The frontend code for live syncing
already exists. We need:

1. A unique, stable ID per file so the sync system knows which files correspond
2. A safe, cross-platform way to store that ID
3. A backend architecture that connects the local Go server to a sync relay

---

## Where to Store the File ID

| Approach | Survives copy/email/git? | Cross-platform? | Risk of loss |
|---|---|---|---|
| `<meta>` tag in `<head>` | Yes | Yes | User could delete it |
| `data-` attr on `<html>` tag | Yes | Yes | Same |
| HTML comment `<!-- id: ... -->` | Yes | Yes | Stripped by minifiers |
| OS extended attributes (xattr/ADS) | No — stripped by zip, email, git, cloud | Inconsistent | High |
| Sidecar file (`.htmlclay.meta`) | No — easily separated | Yes | High |
| Central registry in config dir | No — path-dependent, breaks on move | Yes | Medium |

### Recommendation: `<meta>` tag

The ID lives inside the file, travels with it everywhere, and the frontend can read it
directly from the DOM with zero backend involvement:

```html
<head>
  <meta name="htmlclay-id" content="b3f7a2e1-9c04-4d8b-af12-8e3b5c6d7f90">
</head>
```

The frontend reads it with:

```js
document.querySelector('meta[name="htmlclay-id"]').content
```

This is the same pattern used by Google Docs (stores doc ID in the URL/metadata),
Notion (block IDs in the data model), and Figma (node IDs in the file). The principle
is: **the ID should be intrinsic to the content, not to where it's stored.**

### Why not the other options?

- **OS extended attributes (xattr on macOS/Linux, NTFS Alternate Data Streams on
  Windows):** These are invisible metadata attached to the file at the OS level. They
  get silently stripped by zip archives, email attachments, git, most cloud storage
  (Dropbox, Google Drive), and many file copy operations. You'd lose the ID constantly.

- **Sidecar file (`.htmlclay.meta`):** Creates clutter in the user's directory. Easy
  to accidentally delete or forget to copy. If someone moves the `.htmlclay` file
  without the `.meta` file, the ID is gone.

- **Central registry in config dir:** A database/JSON file that maps file paths to
  IDs. Breaks when files are renamed or moved. Doesn't work across machines at all
  (the whole point of syncing).

- **HTML comment:** Works but is fragile. HTML minifiers strip comments. Template
  engines strip comments. Copy-pasting content between files could duplicate or lose
  the comment.

- **`data-` attribute on `<html>`:** Technically fine, but `<meta>` tags are the
  standard HTML mechanism for document-level metadata. Using `<meta>` means any
  HTML-aware tool understands it's metadata, not content.

---

## Backend Architecture

Since the frontend already handles live syncing, the Go local server's role is
minimal — it just needs to manage IDs and serve files. The sync traffic flows
browser-to-relay, not through the Go backend.

```
┌─────────────────────────────────────────────┐
│  User A's machine                           │
│                                             │
│  .htmlclay file ◄──► Go local server ──► Browser
│  (has <meta> ID)     (read/write file)      │ │
└─────────────────────────────────────────────┘ │
                                                │ WebSocket
                                                ▼
                                         ┌──────────────┐
                                         │ Relay server  │
                                         │ (rooms by ID) │
                                         └──────────────┘
                                                ▲
                                                │ WebSocket
┌─────────────────────────────────────────────┐ │
│  User B's machine                           │ │
│                                             │ │
│  .htmlclay file ◄──► Go local server ──► Browser
│  (same <meta> ID)    (read/write file)      │
└─────────────────────────────────────────────┘
```

### What each piece does

**Go local server (already exists, small changes needed):**
- Reads/writes `.htmlclay` files from disk
- Auto-assigns a UUID to files that don't have one
- Exposes the file ID to the frontend via the `/meta/{token}` endpoint
- Does NOT handle sync traffic — that's browser ↔ relay directly

**Browser/frontend (already built):**
- Reads the file ID from the DOM (`<meta name="htmlclay-id">`)
- Opens a WebSocket connection to the relay server
- Subscribes to a room using the file ID
- Sends local changes to the relay, receives remote changes from the relay
- Applies changes to the DOM and triggers saves back to the Go server

**Relay server (needs to be built or hosted):**
- Simple WebSocket server
- Each file ID is a "room"
- When a client sends a message, broadcast it to all other clients in the same room
- Stateless — doesn't need to understand HTML or store file contents
- Could be ~100 lines of Go, or use a hosted service (Ably, Pusher, Liveblocks)

---

## ID Injection: How It Works

### Flow

```
User opens file
  → Go reads file from disk
  → Does it have <meta name="htmlclay-id">?
     YES → serve as-is (with appname injected as usual)
     NO  → generate UUID v4
         → inject <meta name="htmlclay-id" content="uuid"> into <head>
         → write the modified file back to disk (one-time)
         → serve it
```

Writing the ID to disk immediately (not just in memory) is important — if the app
restarts, the ID is still there. And if the user copies the file to another machine,
the ID travels with it.

### Implementation in the Go codebase

This follows the same pattern as the existing `appname` injection in `htmlutil/`, but
with key differences:

**Similarities to `appname`:**
- Parse the HTML to find the right injection point
- Inject an attribute/tag if not present

**Differences from `appname`:**
- Targets `<meta>` tag inside `<head>`, not an attribute on `<html>`
- Only injects if absent (idempotent — never overwrites an existing ID)
- **Persists to disk** — the ID is written into the actual file permanently. Unlike
  `appname` (which is injected on serve and stripped on save), the `htmlclay-id` is
  part of the file's content.

### Handling edge cases

**File has a `<head>` tag:**

Inject the `<meta>` tag as the first child of `<head>`:

```html
<!-- Before: -->
<head>
  <title>My Page</title>
</head>

<!-- After: -->
<head>
<meta name="htmlclay-id" content="b3f7a2e1-9c04-4d8b-af12-8e3b5c6d7f90">
  <title>My Page</title>
</head>
```

**File has `<html>` but no `<head>`:**

Inject a `<head>` block after the `<html>` tag:

```html
<!-- Before: -->
<html lang="en">
<body>...</body>
</html>

<!-- After: -->
<html lang="en">
<head><meta name="htmlclay-id" content="b3f7a2e1-..."></head>
<body>...</body>
</html>
```

**File is a bare HTML fragment (no `<html>` or `<head>`):**

Prepend a `<head>` block:

```html
<!-- Before: -->
<div>my content</div>

<!-- After: -->
<head><meta name="htmlclay-id" content="b3f7a2e1-..."></head>
<div>my content</div>
```

Since we control the `.htmlclay` format, we could also enforce that all files have a
`<head>`. But handling the edge case is cheap.

### Changes to existing code

**`htmlutil/` package — add two new functions:**

- `ReadSyncID(html []byte) string` — extracts the UUID from the `<meta>` tag, returns
  empty string if not present
- `InjectSyncID(html []byte, id string) []byte` — injects the `<meta>` tag into
  `<head>` (creates `<head>` if needed), returns the HTML unchanged if ID already
  present

**`server/handlers.go` — modify `handleServeFile`:**

After reading the file from disk, check for a sync ID. If absent, generate one,
inject it, and write it back to disk before serving.

```go
func (s *Server) handleServeFile(w http.ResponseWriter, r *http.Request) {
    // ... existing path validation and file reading ...

    data, err := os.ReadFile(f.AbsPath)
    // ... error handling ...

    // Ensure file has a sync ID (one-time write)
    if htmlutil.ReadSyncID(data) == "" {
        id := uuid.New().String()
        data = htmlutil.InjectSyncID(data, id)
        atomicWriteFile(f.AbsPath, data) // persist to disk
    }

    data = htmlutil.InjectAppName(data, f.Token)
    // ... serve response ...
}
```

**`server/handlers.go` — modify `handleMeta`:**

Add the sync ID to the metadata response so the frontend can access it without
parsing HTML:

```go
type fileMeta struct {
    Path         string `json:"path"`
    AbsolutePath string `json:"absolutePath"`
    Name         string `json:"name"`
    Size         int64  `json:"size"`
    LastModified string `json:"lastModified"`
    SyncID       string `json:"syncID"`
}
```

Read the sync ID from the file when building the meta response:

```go
data, err := os.ReadFile(f.AbsPath)
// ... error handling ...
syncID := htmlutil.ReadSyncID(data)

meta := fileMeta{
    // ... existing fields ...
    SyncID: syncID,
}
```

**`server/handlers.go` — `handleSave` stays mostly the same:**

The `StripAppName` call already strips the `appname` attribute before saving. The
`htmlclay-id` meta tag is NOT stripped — it stays in the saved file permanently.
No changes needed to the save handler for the ID.

### UUID generation

Add `github.com/google/uuid` to dependencies:

```bash
go get github.com/google/uuid
```

Or use `crypto/rand` from the standard library to avoid the dependency:

```go
import "crypto/rand"

func generateSyncID() string {
    var b [16]byte
    rand.Read(b[:])
    b[6] = (b[6] & 0x0f) | 0x40 // version 4
    b[8] = (b[8] & 0x3f) | 0x80 // variant 1
    return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
        b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
```

The stdlib approach avoids adding a dependency for a single function. The app already
uses `crypto/rand` in `session/session.go` for generating tokens, so this is
consistent.

---

## Relay Server Design

The relay is a separate service (not part of the local Go app). It can be as simple
as a WebSocket echo server with room support.

### Minimal implementation (~100 lines of Go)

```
Client connects → sends: {"type":"join", "room":"uuid-here"}
Client sends change → server broadcasts to all other clients in that room
Client disconnects → removed from room
```

The server doesn't need to:
- Understand HTML
- Store file contents
- Handle authentication (initially — add later)
- Persist anything (stateless)

### Protocol sketch

```json
// Client → Server: join a room
{"type": "join", "room": "b3f7a2e1-9c04-4d8b-af12-8e3b5c6d7f90"}

// Client → Server: send a change
{"type": "change", "room": "b3f7a2e1-...", "data": "<opaque change payload>"}

// Server → Client: broadcast a change from another client
{"type": "change", "room": "b3f7a2e1-...", "data": "<opaque change payload>"}

// Client → Server: leave a room
{"type": "leave", "room": "b3f7a2e1-..."}
```

The `data` field is opaque to the server — the frontend decides what format to use
(full HTML, diffs, CRDT operations, etc.). The server just relays it.

### Hosting options

**Self-hosted:**
- Deploy the relay as a single Go binary on a VPS (Fly.io, Railway, DigitalOcean)
- Use `nhooyr.io/websocket` (modern, stdlib-aligned) or `gorilla/websocket` (battle-tested)
- Put it behind nginx or Caddy for TLS

**Managed services (if you don't want to run infra):**
- **Ably** — WebSocket rooms as a service, generous free tier
- **Pusher** — similar to Ably
- **Liveblocks** — specifically designed for collaborative apps
- **PartyKit** — Cloudflare-based, good for room-based real-time apps

**Scaling considerations:**
- A single Go relay server can handle ~10k concurrent WebSocket connections easily
- Beyond that, use Redis pub/sub to fan out across multiple relay instances
- For initial launch, a single $5/mo VPS is more than enough

---

## When Does the ID Get Assigned?

### Option A: On first open (recommended)

Every file gets an ID the first time HTML Clay opens it. Simple, automatic, no user
interaction required.

**Tradeoff:** If two users independently create the same file, they get different
IDs. Sync treats them as different files. To link them, one user shares a sync URL
(e.g., `htmlclay.com/sync/b3f7a2e1-...`) and the other opens it. The frontend
replaces the local file's ID with the shared one.

### Option B: On first share

The file has no ID until the user explicitly clicks "Share" or "Start Syncing". The
ID is assigned at that point.

**Tradeoff:** Cleaner for files that are never synced (no unnecessary metadata). But
adds a user-facing step and the codebase needs to handle ID-less files in more places.

### Recommendation

**Go with Option A.** A UUID in a `<meta>` tag is 80 bytes — invisible and harmless.
The simplicity of "every file always has an ID" eliminates an entire class of
null-check edge cases in both the frontend and backend.

---

## Conflict Resolution

When two users edit the same file simultaneously, changes can conflict. This is a
frontend concern (the Go backend just reads/writes files), but the choice affects the
architecture:

### Option 1: Last write wins

Simplest. The most recent save overwrites the previous one. Fine for casual use, bad
for simultaneous editing.

### Option 2: Operational Transform (OT)

The same approach Google Docs uses. Each change is a sequence of operations
(insert, delete, retain). The server transforms operations against each other to
maintain consistency. Complex to implement correctly.

### Option 3: CRDTs (Conflict-free Replicated Data Types)

Libraries like **Yjs** or **Automerge** handle this automatically on the frontend.
Each client maintains a CRDT document. Changes are merged without conflicts by
mathematical guarantee. The relay server just passes CRDT update messages between
clients — no transformation needed.

### Recommendation

**Use Yjs on the frontend.** It's the most popular CRDT library for web apps, handles
rich text and DOM structures, and the relay server stays dead simple (just a message
forwarder). The Go backend doesn't need to understand the sync protocol at all.

---

## Security Considerations

### File ID privacy

The file ID is a UUID — random and unguessable. But anyone who can read the HTML file
can see the ID. If the relay server accepts connections by ID alone, anyone with the
ID can subscribe to changes.

**For initial version:** This is fine. The ID acts as a shared secret (like a Google
Docs link with "anyone with the link can edit").

**For later:** Add authentication. The relay server verifies that a connecting client
is authorized to access a given room. This could be:
- A short-lived token generated by the app and passed to the relay
- User accounts with ACLs on the relay
- End-to-end encryption (clients encrypt changes, relay can't read them)

### Local server security

No changes needed. The Go local server already binds to `127.0.0.1` (localhost only)
and uses token-based authentication for file access. The sync ID doesn't affect local
security — it's just metadata in the file.

---

## Summary of Go Codebase Changes

| Area | Change | Size |
|---|---|---|
| `htmlutil/` | Add `ReadSyncID()` and `InjectSyncID()` functions | ~60 lines |
| `server/handlers.go` | Auto-assign ID on first serve, add `syncID` to meta response | ~15 lines |
| `go.mod` | Optionally add `github.com/google/uuid` (or use stdlib crypto/rand) | 1 line |
| `htmlutil/htmlutil_test.go` | Tests for ID injection edge cases | ~50 lines |

Total: ~125 lines of Go code for the ID management layer. The relay server is a
separate project.
