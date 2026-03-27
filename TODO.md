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

## Existing Assets
- [x] `tray/icon.png` — System tray icon (126 bytes, embedded via `//go:embed`)
- [x] `dist/macos/htmlclay.icns` — macOS app icon (bundled into HTMLClay.app)

## Assets to Generate

### Source Master Icon
- [ ] **Master app icon** (1024x1024 SVG or PNG) — Single source design to derive all other formats from

### macOS
- [ ] `dist/macos/htmlclay.icns` — Regenerate from master icon at proper resolution (current file is only 8 bytes, likely a placeholder). Must contain 16x16, 32x32, 128x128, 256x256, 512x512, 1024x1024 variants.
- [ ] `dist/macos/doc.icns` — Document icon for `.clayhtml` files. Referenced in `Info.plist` (`CFBundleTypeIconFile`) and conditionally copied in `dist/macos/build.sh`. Should visually represent a Clay HTML document (e.g., the app icon on a document shape).

### Linux
- [ ] `dist/linux/htmlclay.png` — 256x256 PNG app icon. Referenced in `dist/linux/install-icon.sh` and `dist/linux/htmlclay.desktop` (`Icon=htmlclay`). Installed to `~/.local/share/icons/hicolor/256x256/apps/`.
- [ ] `dist/linux/htmlclay.svg` — SVG version for scalable icon support. Optional but recommended for HiDPI Linux desktops.

### Windows
- [ ] `dist/windows/htmlclay.ico` — Multi-resolution `.ico` file for taskbar/explorer. Should contain 16x16, 32x32, 48x48, 256x256. Used for embedding into the Windows binary via `goversioninfo`.

### System Tray
- [ ] `tray/icon.png` — Regenerate at proper resolution if the current 126-byte file is a placeholder. Used on all platforms via `fyne.io/systray`. Recommended: 22x22 or 32x32 PNG with transparency.

### Web / Misc
- [ ] `dist/favicon.ico` — Favicon for the local web UI served by the HTML Clay server
- [ ] `dist/favicon.svg` — SVG favicon for modern browsers

## Icon Notes
- Generate all raster formats from a single master SVG to keep everything consistent.
- macOS `.icns` can be created with `iconutil` from a `.iconset` folder of PNGs.
- Windows `.ico` can be created with ImageMagick: `convert icon-256.png icon-48.png icon-32.png icon-16.png htmlclay.ico`
- The tray icon should be simple and legible at small sizes (22-32px). Avoid fine detail.
