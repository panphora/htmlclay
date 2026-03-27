# Phase 1 Implementation Plan

## Important: Cookie vs Attribute for Auth Token

The brainstorm doc says "auth via cookie." But cookies on localhost collide
when multiple files are open — this is the exact problem that caused the
original move from `currentResource` cookie to `appname` attribute.

HTML Clay supports multiple files simultaneously. Each file is served at a
unique path (`/f/path/to/file.htmlclay`) but they all share the same origin
(`127.0.0.1:{port}`). A cookie set for one file would be overwritten when
the next file is served.

Path-scoped cookies (`Path=/f/path/to/file.htmlclay`) would work for the
serve endpoint but not for `/save`, `/read`, or `/meta` which are at
different paths.

**Decision: keep the token as an attribute on `<html>`, but rename it from
`appname` to `htmlclaytoken`.** Same proven mechanism, just separated from
the name concern. Strip on save, inject on serve. htmlclay doesn't touch
`appname`, hyperclay doesn't touch `htmlclaytoken`.

---

## Task 1: Rename .clayhtml → .htmlclay

### Files to modify
- `server/handlers.go` — temp file pattern
- `server/handlers_test.go` — test filenames
- `server/server_test.go` — test path
- `server/security_test.go` — test paths
- `session/session_test.go` — test filenames
- `dist/macos/Info.plist` — extension registration
- `dist/linux/htmlclay.desktop` — comment and MIME type
- `dist/linux/htmlclay-mime.xml` — MIME type and glob
- `dist/windows/register.bat` — registry keys

### Files to rename
- `testdata/minimal.clayhtml` → `testdata/minimal.htmlclay`
- `testdata/with-appname.clayhtml` → `testdata/with-appname.htmlclay`
- `testdata/traversal.clayhtml` → `testdata/traversal.htmlclay`

---

## Task 2: Auth appname → htmlclaytoken

### htmlutil/htmlutil.go
- Rename `appnameAttr` regex → `tokenAttr` (matching `htmlclaytoken`)
- Rename `InjectAppName` → `InjectToken`
- Rename `StripAppName` → `StripToken`
- Attribute name changes from `appname` to `htmlclaytoken`

### server/handlers.go
- `handleServeFile`: call `InjectToken` instead of `InjectAppName`
- `handleSave`: call `StripToken` instead of `StripAppName`

### htmlutil/htmlutil_test.go
- Update all test cases for the renamed functions and attribute

### server/handlers_test.go
- Update `TestServeFile` to check for `htmlclaytoken` instead of `appname`
- Update `TestSaveValid` to check stripping of `htmlclaytoken`

### testdata/minimal.htmlclay
- Update JS to read `htmlclaytoken` instead of `appname`

### testdata/with-appname.htmlclay
- Rename to `testdata/with-token.htmlclay`
- Update content to use `htmlclaytoken` instead of `appname`

---

## Task 3: Add htmlclayid persistent UUID

### htmlutil/htmlutil.go
- Add `htmlclayidAttr` regex (matching `htmlclayid`)
- Add `ReadHTMLClayID(data []byte) string` — extract UUID from attribute
- Add `InjectHTMLClayID(data []byte, id string) []byte` — inject if absent

### htmlutil/htmlutil_test.go
- Tests for ReadHTMLClayID, InjectHTMLClayID, idempotency, coexistence
  with htmlclaytoken

### server/handlers.go
- Add UUID generation function (using crypto/rand, same pattern as session)
- `handleServeFile`: check for htmlclayid, inject + write to disk if missing
- `fileMeta` struct: add `HTMLClayID` field
- `handleMeta`: read htmlclayid from file, include in response

### server/handlers_test.go
- Test that served file gets htmlclayid injected
- Test that htmlclayid persists across serves
- Test that meta endpoint includes htmlclayid

---

## Task 4: hyperclay-local .htmlclay support

### src/main/server.js
- File serving: recognize .htmlclay alongside .html
- Appname extraction: handle .htmlclay extension

### src/sync-engine/index.js
- Chokidar watcher: add `**/*.htmlclay` to glob (or `**/*.{html,htmlclay}`)
- File matching: recognize .htmlclay in extension checks
