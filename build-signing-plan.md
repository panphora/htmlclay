# HTMLClay Build, Signing & Release Plan

Porting the hyperclay-local build/signing/release workflow to htmlclay.

---

## Current State

- **macOS**: Ad-hoc codesigning (`codesign -f -s -`), no notarization. Users get Gatekeeper "damaged app" warnings.
- **Windows**: Bare `.exe`, no signing. Users get SmartScreen warnings.
- **Linux**: Bare binary, no signing needed.
- **Release**: Tag-triggered GitHub Actions (`release.yml`) builds all 5 artifacts and creates a GitHub Release. No CDN, no unified local release script.
- **Version**: Hardcoded `const version = "1.0"` in `main.go`, hardcoded in `Info.plist`.
- **Update checker**: Hits `https://htmlclay.com/version.json` — expects `{"latest":"X.Y","url":"..."}`.

---

## Phase 1: macOS Developer ID Signing + Notarization

### 1a. Create entitlements plist

Create `dist/macos/entitlements.plist`. HTMLClay is a Go binary (not Electron), so it needs minimal entitlements:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>com.apple.security.cs.allow-unsigned-executable-memory</key>
  <true/>
  <key>com.apple.security.cs.disable-library-validation</key>
  <true/>
</dict>
</plist>
```

Go's garbage collector and runtime may need unsigned executable memory. Library validation must be disabled for the systray dependency (fyne.io/systray links against system frameworks). If the app works without these after testing, remove them — fewer entitlements is better.

### 1b. Update `dist/macos/build.sh`

Replace ad-hoc signing with real Developer ID signing + notarization submission:

```bash
#!/bin/bash
set -euo pipefail

# Load credentials
if [ -f .env ]; then
  export $(grep -v '^#' .env | xargs)
fi

IDENTITY="Hyperspace Systems LLC (JC7YGGXYKH)"
APP="HTMLClay.app"
CONTENTS="$APP/Contents"
MACOS="$CONTENTS/MacOS"
RESOURCES="$CONTENTS/Resources"

rm -rf "$APP"
mkdir -p "$MACOS" "$RESOURCES"

CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o "$MACOS/htmlclay" .

cp dist/macos/Info.plist "$CONTENTS/"
echo -n "APPL????" > "$CONTENTS/PkgInfo"
[ -f dist/macos/htmlclay.icns ] && cp dist/macos/htmlclay.icns "$RESOURCES/"
[ -f dist/macos/doc.icns ] && cp dist/macos/doc.icns "$RESOURCES/"

# Real Developer ID signing with hardened runtime
codesign -f -s "$IDENTITY" \
  --options runtime \
  --entitlements dist/macos/entitlements.plist \
  --deep \
  "$APP"

echo "Signed $APP with $IDENTITY"

# Create DMG
VERSION=$(grep 'const version' main.go | sed 's/.*"\(.*\)"/\1/')
ARCH=$(uname -m)
[ "$ARCH" = "x86_64" ] && ARCH="amd64"
DMG_NAME="HTMLClay-${VERSION}-${ARCH}.dmg"

hdiutil create -volname "HTMLClay" \
  -srcfolder "$APP" \
  -ov -format UDZO \
  "$DMG_NAME"

codesign -f -s "$IDENTITY" "$DMG_NAME"
echo "Created $DMG_NAME"

# Submit for notarization (non-blocking)
if [ -n "${APPLE_ID:-}" ] && [ -n "${APPLE_TEAM_ID:-}" ] && [ -n "${APPLE_APP_SPECIFIC_PASSWORD:-}" ]; then
  echo "Submitting $DMG_NAME for notarization..."
  xcrun notarytool submit "$DMG_NAME" \
    --apple-id "$APPLE_ID" \
    --team-id "$APPLE_TEAM_ID" \
    --password "$APPLE_APP_SPECIFIC_PASSWORD" \
    --wait

  xcrun stapler staple "$DMG_NAME"
  echo "Notarized and stapled $DMG_NAME"
else
  echo "Skipping notarization (missing Apple credentials)"
fi
```

Key differences from hyperclay-local: simpler. No need for the async submit/poll/staple dance because htmlclay only builds one arch at a time locally. Using `--wait` blocks until Apple responds (typically 2-5 minutes), then staples immediately.

### 1c. Create `.env.example`

```
APPLE_ID=your-apple-id@example.com
APPLE_APP_SPECIFIC_PASSWORD=xxxx-xxxx-xxxx-xxxx
APPLE_TEAM_ID=JC7YGGXYKH
```

The `.env` already exists in the repo (has Namecheap vars). Add the Apple vars to it.

### 1d. Update Makefile

```makefile
dist-macos:
	bash dist/macos/build.sh

dist-macos-unsigned:
	bash dist/macos/build.sh --unsigned
```

Add an `--unsigned` flag to `build.sh` that falls back to ad-hoc signing for local dev/testing (same purpose as hyperclay-local's `mac-build:local`).

---

## Phase 2: Windows Azure Trusted Signing in CI

### 2a. Update `release.yml` — add signing to the Windows job

The current workflow builds Windows as part of the matrix. Extract Windows into its own job (or add conditional steps) so signing only runs for Windows:

After the Go build step for the `windows-latest` matrix entry, add:

```yaml
- name: Install TrustedSigning module
  if: matrix.goos == 'windows'
  run: Install-Module -Name TrustedSigning -Force -Scope CurrentUser -AcceptLicense
  shell: pwsh

- name: Sign with Azure Trusted Signing
  if: matrix.goos == 'windows'
  env:
    AZURE_TENANT_ID: ${{ secrets.AZURE_TENANT_ID }}
    AZURE_CLIENT_ID: ${{ secrets.AZURE_CLIENT_ID }}
    AZURE_CLIENT_SECRET: ${{ secrets.AZURE_CLIENT_SECRET }}
  run: |
    Invoke-TrustedSigning `
      -Endpoint "https://eus.codesigning.azure.net" `
      -CodeSigningAccountName "Hyperclay" `
      -CertificateProfileName "HyperclayLocalPublicCertProfile" `
      -Files "htmlclay.exe" `
      -FileDigest SHA256 `
      -Verbose
  shell: pwsh

- name: Verify signature
  if: matrix.goos == 'windows'
  run: |
    $sig = Get-AuthenticodeSignature "htmlclay.exe"
    Write-Host "Status: $($sig.Status)"
    Write-Host "Signer: $($sig.SignerCertificate.Subject)"
    if ($sig.Status -ne "Valid") { exit 1 }
  shell: pwsh
```

### 2b. Add GitHub Secrets

Same secrets as hyperclay-local (same Azure account):
- `AZURE_TENANT_ID`
- `AZURE_CLIENT_ID`
- `AZURE_CLIENT_SECRET`

These need to be added to the `panphora/htmlclay` GitHub repo settings.

### 2c. Certificate profile

Reuse the same Azure Trusted Signing account/profile (`Hyperclay` / `HyperclayLocalPublicCertProfile`). The certificate profile name says "HyperclayLocal" — check if Azure allows signing a different product with the same profile. If not, create a second profile `HTMLClayPublicCertProfile` in the same Azure Trusted Signing account. This is a portal-only change, no code impact (just update the profile name string).

---

## Phase 3: Version Injection via ldflags

Instead of editing files like hyperclay-local does, use Go's standard `ldflags` approach.

### 3a. Change `main.go`

```go
var version = "dev"  // overridden at build time via -ldflags
```

Change from `const` to `var`.

### 3b. Update all build commands

Every `go build` invocation gets:

```
-ldflags="-s -w -X main.version=${VERSION}"
```

This applies to:
- `Makefile` (build target)
- `dist/macos/build.sh`
- `dist/linux/build.sh`
- `dist/windows/build.ps1`
- `.github/workflows/release.yml` (extract version from tag: `VERSION=${GITHUB_REF_NAME#v}`)
- `.github/workflows/ci.yml` (optional, can leave as "dev")

### 3c. Update `Info.plist` at build time

In `dist/macos/build.sh`, after copying `Info.plist` into the bundle, sed-replace the version:

```bash
VERSION=$(echo "$GITHUB_REF_NAME" | sed 's/^v//' || echo "dev")
sed -i '' "s/<string>1.0</<string>${VERSION}</" "$CONTENTS/Info.plist"
```

This keeps the source `Info.plist` as a template with `1.0` as placeholder.

---

## Phase 4: R2 CDN Upload

### 4a. Add R2 upload step to `release.yml`

After the release job creates the GitHub Release, add an upload job:

```yaml
upload-r2:
  needs: release
  runs-on: ubuntu-latest
  steps:
    - uses: actions/download-artifact@v4
      with:
        path: artifacts
        merge-multiple: true

    - name: Upload to R2
      env:
        R2_ACCOUNT_ID: ${{ secrets.R2_ACCOUNT_ID }}
        R2_ACCESS_KEY: ${{ secrets.R2_ACCESS_KEY }}
        R2_SECRET_KEY: ${{ secrets.R2_SECRET_KEY }}
        R2_BUCKET: ${{ secrets.R2_BUCKET_HTMLCLAY }}
      run: |
        pip install awscli
        aws configure set aws_access_key_id "$R2_ACCESS_KEY"
        aws configure set aws_secret_access_key "$R2_SECRET_KEY"
        aws configure set region auto

        ENDPOINT="https://${R2_ACCOUNT_ID}.r2.cloudflarestorage.com"

        for file in artifacts/*; do
          filename=$(basename "$file")
          aws s3 cp "$file" "s3://${R2_BUCKET}/$filename" \
            --endpoint-url "$ENDPOINT"
          echo "Uploaded: $filename"
        done

    - name: Upload release-info.json
      env:
        R2_ACCOUNT_ID: ${{ secrets.R2_ACCOUNT_ID }}
        R2_ACCESS_KEY: ${{ secrets.R2_ACCESS_KEY }}
        R2_SECRET_KEY: ${{ secrets.R2_SECRET_KEY }}
        R2_BUCKET: ${{ secrets.R2_BUCKET_HTMLCLAY }}
      run: |
        VERSION="${GITHUB_REF_NAME#v}"
        cat > release-info.json <<EOF
        {
          "latest": "$VERSION",
          "url": "https://htmlclay.com/download",
          "date": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
          "files": $(ls artifacts/ | jq -R -s -c 'split("\n") | map(select(. != ""))')
        }
        EOF

        ENDPOINT="https://${R2_ACCOUNT_ID}.r2.cloudflarestorage.com"
        aws s3 cp release-info.json "s3://${R2_BUCKET}/release-info.json" \
          --endpoint-url "$ENDPOINT" \
          --content-type "application/json"
```

### 4b. Use R2 for update checks

The update checker already hits `https://htmlclay.com/version.json`. Two options:

1. **Keep it separate**: Maintain `version.json` on the website manually or via the release workflow. Simplest.
2. **Point to R2**: Change `DefaultVersionURL` to point at the R2-hosted `release-info.json`. The JSON format already matches what `update.Check` expects (`latest` and `url` fields).

Option 2 is better — the release workflow automatically updates it, no manual step.

### 4c. R2 bucket

Create a separate R2 bucket for htmlclay (e.g., `htmlclay-releases`) or a subfolder in the existing bucket. Add these secrets to the GitHub repo:
- `R2_ACCOUNT_ID` (same as hyperclay-local)
- `R2_ACCESS_KEY` (same or new)
- `R2_SECRET_KEY` (same or new)
- `R2_BUCKET_HTMLCLAY`

---

## Phase 5: DMG Packaging

### 5a. Replace tar.gz with DMG for macOS releases

The current `release.yml` tars the bare binary. For macOS, ship a DMG containing the `.app` bundle instead.

Update the macOS matrix entries in `release.yml`:
- Instead of `go build` + `tar`, run `dist/macos/build.sh` which produces the `.app` bundle
- Create a DMG from the `.app` using `hdiutil`
- Sign and notarize the DMG in CI (requires importing the Developer ID certificate into the macOS runner's keychain)

This is the most complex CI change because macOS signing in GitHub Actions requires:
1. Base64-encoding the `.p12` certificate
2. Storing it as a GitHub secret (`APPLE_CERTIFICATE_BASE64`, `APPLE_CERTIFICATE_PASSWORD`)
3. Importing it into a temporary keychain in the workflow

```yaml
- name: Import certificate
  if: matrix.goos == 'darwin'
  env:
    CERTIFICATE_BASE64: ${{ secrets.APPLE_CERTIFICATE_BASE64 }}
    CERTIFICATE_PASSWORD: ${{ secrets.APPLE_CERTIFICATE_PASSWORD }}
  run: |
    echo "$CERTIFICATE_BASE64" | base64 --decode > cert.p12
    security create-keychain -p "" build.keychain
    security default-keychain -s build.keychain
    security unlock-keychain -p "" build.keychain
    security import cert.p12 -k build.keychain -P "$CERTIFICATE_PASSWORD" -T /usr/bin/codesign
    security set-key-partition-list -S apple-tool:,apple: -s -k "" build.keychain
    rm cert.p12
```

Then the build step runs `dist/macos/build.sh` which handles signing, DMG creation, and notarization.

**Alternative (simpler)**: Keep macOS builds local only (like hyperclay-local does) and only use CI for Linux + Windows. The release script (Phase 6) would handle macOS locally.

### 5b. Artifact name changes

- `htmlclay-darwin-arm64.tar.gz` → `HTMLClay-{version}-arm64.dmg`
- `htmlclay-darwin-amd64.tar.gz` → `HTMLClay-{version}-amd64.dmg`
- Linux and Windows names stay as-is

---

## Phase 6: Unified Release Script

Create `scripts/release.sh` — a shell script (not Node, since htmlclay is a Go project).

### Flow

```
1. Pre-flight checks
   - Clean working directory
   - Required tools: go, gh, codesign, xcrun, hdiutil
   - Apple credentials in .env

2. Version bump
   - Accept --major / --minor / --patch
   - Or auto-detect from commits (optional Claude Code integration)
   - Update version string (only used in ldflags, not file edits)

3. Tag and push
   - git tag v{version}
   - git push origin v{version}
   - This triggers release.yml for Linux + Windows builds

4. Build macOS locally (both archs)
   - Build arm64 natively
   - Cross-compile amd64 (or build on an Intel Mac)
   - Sign both with Developer ID
   - Create DMGs
   - Submit for notarization (--wait)
   - Staple tickets

5. Collect executables
   - macOS DMGs are local
   - Wait for GitHub Actions to complete (poll with `gh run watch`)
   - Download Windows + Linux artifacts from the release

6. Upload to R2
   - Upload all artifacts
   - Upload release-info.json (powers update checker)

7. Done
   - Print download URLs
   - Log duration
```

### Key differences from hyperclay-local's `release.js`

| Aspect | hyperclay-local | htmlclay |
|--------|----------------|----------|
| Language | Node.js | Shell (bash) |
| macOS build | Local, electron-builder | Local, `go build` + `hdiutil` |
| Windows build | Trigger separate workflow | Already in `release.yml` matrix |
| Linux build | Local, electron-builder | Already in `release.yml` matrix |
| Notarization | Async submit, poll, staple | `--wait` flag (simpler, one arch at a time) |
| Version bump | Edit 3 files | ldflags only, tag is the version |
| External docs | Updates Edge template | Updates `version.json` / `release-info.json` |

---

## Phase 7: macOS Cross-Compilation for amd64

Currently `dist/macos/build.sh` only builds the native architecture. For a universal release:

- **arm64**: Build natively on Apple Silicon
- **amd64**: Cross-compile with `GOARCH=amd64` (Go supports this natively, CGO may need `CC=x86_64-apple-darwin-gcc` or similar)

The release script should build both, producing two DMGs. This mirrors hyperclay-local's dual-arch DMG output.

If cross-compiling with CGO is problematic (systray's C dependencies), an alternative is building amd64 in GitHub Actions on `macos-13` (Intel runners) and only building arm64 locally.

---

## Summary: Files to Create/Modify

| File | Action |
|------|--------|
| `dist/macos/entitlements.plist` | Create |
| `dist/macos/build.sh` | Rewrite (signing, DMG, notarization) |
| `main.go` | Change `const version` to `var version` |
| `Makefile` | Add `dist-macos-unsigned`, update ldflags |
| `dist/linux/build.sh` | Add ldflags |
| `dist/windows/build.ps1` | Add ldflags |
| `.github/workflows/release.yml` | Add Windows signing, R2 upload, macOS DMG+signing (or remove macOS from CI) |
| `.env` | Add Apple credential vars |
| `.env.example` | Create |
| `scripts/release.sh` | Create (unified release script) |

## Secrets to Configure (GitHub repo)

| Secret | Purpose |
|--------|---------|
| `AZURE_TENANT_ID` | Windows signing |
| `AZURE_CLIENT_ID` | Windows signing |
| `AZURE_CLIENT_SECRET` | Windows signing |
| `APPLE_CERTIFICATE_BASE64` | macOS signing in CI (if keeping macOS in CI) |
| `APPLE_CERTIFICATE_PASSWORD` | macOS signing in CI |
| `APPLE_ID` | Notarization |
| `APPLE_TEAM_ID` | Notarization |
| `APPLE_APP_SPECIFIC_PASSWORD` | Notarization |
| `R2_ACCOUNT_ID` | CDN upload |
| `R2_ACCESS_KEY` | CDN upload |
| `R2_SECRET_KEY` | CDN upload |
| `R2_BUCKET_HTMLCLAY` | CDN upload |

## Decision Point: macOS in CI vs Local

The biggest architectural decision is whether to sign/notarize macOS builds in CI or locally.

**CI (like current `release.yml`):**
- Pro: Fully automated, reproducible, no local Mac dependency
- Con: Requires certificate import into runner keychain, more complex workflow, Apple credentials as GitHub secrets

**Local (like hyperclay-local):**
- Pro: Simpler, certificate already in local keychain, `.env` for credentials
- Con: Requires a Mac to release, not fully automated

**Recommendation**: Start with local (matches hyperclay-local's proven pattern), migrate to CI later if needed. The release script handles macOS locally while CI handles Linux + Windows.
