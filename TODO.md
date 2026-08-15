# Features

- [x] Folder trust: read-only banner + "open this file for editing" (single-use nonce, native
      dialog), trusted folders (declared folders whose `.htmlclay` files auto-register editable,
      device+inode fingerprints, real revocation), same-origin gate on every mutating route,
      `Sec-Fetch-Dest` gate on token injection, volume-probed case folding.
      Plan: `hyper/plans/htmlclay/archive/workspace-trust-plan.md`

- [x] Simplification, shipped in v1.3.0: one concept (Trusted Folders, granting read and write) in
      place of four, a lasting port per folder so a bookmarked URL survives a restart, a recovery
      page on a remembered port that is no longer served, and app mode deleted.
      Plan: `hyper/plans/htmlclay/archive/simplification-plan.md`

---

# HTMLClay Release Setup TODO

GitHub secrets to add to `panphora/htmlclay` (same values as hyperclay-local unless noted):

## Azure Trusted Signing (Windows)
- [x] `AZURE_TENANT_ID`
- [x] `AZURE_CLIENT_ID`
- [x] `AZURE_CLIENT_SECRET`

## Apple Notarization (macOS)
- [x] `APPLE_ID`
- [x] `APPLE_TEAM_ID`
- [x] `APPLE_APP_SPECIFIC_PASSWORD`

## Cloudflare R2 (CDN upload)
- [x] `R2_ACCOUNT_ID`
- [x] `R2_ACCESS_KEY`
- [x] `R2_SECRET_KEY`
- [x] `R2_BUCKET`

## Azure certificate profile
- [x] Check if `HyperclayLocalPublicCertProfile` can sign HTMLClay too, or create a new profile in Azure Trusted Signing

---

# Icon & Image Assets

**Re-verified against the filesystem 2026-07-29.** Every item below except the two favicons is done.
The unchecked boxes and the "likely a placeholder" notes were stale: the placeholders they describe
(an 8-byte `.icns`, a 126-byte tray PNG) were replaced some time ago and every file is now real.

### Source Master Icon
- [x] **Master app icon** — `dist/icons/` holds the source set (`blob.svg`, `clay-app.svg`, a `source/`
      folder) plus `generate.sh` to derive the rest

### macOS
- [x] `dist/macos/htmlclay.icns` — real Mac OS X icon resource, 144 KB (was an 8-byte placeholder)
- [x] `dist/macos/doc.icns` — document icon for `.htmlclay` files, 116 KB. Referenced by
      `Info.plist` (`CFBundleTypeIconFile`) and copied in `dist/macos/build.sh`

### Linux
- [x] `dist/linux/htmlclay.png` — verified 256x256
- [x] `dist/linux/htmlclay.svg` — 13 KB scalable version

### Windows
- [x] `dist/windows/htmlclay.ico` — verified multi-resolution: 6 icons up to 256x256

### System Tray
- [x] `tray/icon.png` — real 128x128 PNG (was the 126-byte placeholder). Larger than the 22-32px this
      file originally recommended; systray downscales it, so this is a nitpick, not a bug

### Web / Misc
- [ ] `dist/favicon.ico` — Favicon for the local web UI served by the HTML Clay server. **Confirmed
      still missing:** nothing matches `dist/favicon.*`
- [ ] `dist/favicon.svg` — SVG favicon for modern browsers. **Confirmed still missing**

## Icon Notes
- Generate all raster formats from a single master SVG to keep everything consistent.
- macOS `.icns` can be created with `iconutil` from a `.iconset` folder of PNGs.
- Windows `.ico` can be created with ImageMagick: `convert icon-256.png icon-48.png icon-32.png icon-16.png htmlclay.ico`
- The tray icon should be simple and legible at small sizes (22-32px). Avoid fine detail.
