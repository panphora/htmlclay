#!/usr/bin/env bash
#
# generate.sh — build every shipping icon from the three SVG masters in this folder.
#
#   blob.svg           master mark, contact shadow on its own layer
#   blob-noshadow.svg  master mark, no shadow (used for tiles, doc, favicon)
#   blob-grin.svg      small-size mark, bigger classic grin (tray + favicon)
#
# Outputs (written to where the app and build scripts consume them):
#   tray/icon.png                colored grin, used by systray on Windows/Linux
#   tray/icon-template.png       black grin silhouette, macOS menu-bar template
#   dist/macos/htmlclay.icns     app icon  (CFBundleIconFile = htmlclay)
#   dist/macos/doc.icns          document icon (CFBundleTypeIconFile = doc)
#   dist/linux/htmlclay.png      app icon, 256px (install-icon.sh)
#   dist/linux/htmlclay.svg      app icon, scalable
#   dist/windows/htmlclay.ico    app icon, multi-size
#
# Requires: rsvg-convert, ImageMagick (magick or convert), python3.
# macOS .icns uses iconutil when present (falls back to ImageMagick otherwise).

set -euo pipefail

ICONS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$ICONS_DIR/../.." && pwd)"

need() { command -v "$1" >/dev/null 2>&1 || { echo "error: '$1' not found on PATH" >&2; exit 1; }; }
need rsvg-convert
need python3
if command -v magick >/dev/null 2>&1; then MAGICK="magick"
elif command -v convert >/dev/null 2>&1; then MAGICK="convert"
else echo "error: ImageMagick (magick/convert) not found" >&2; exit 1; fi

BUILD="$(mktemp -d "${TMPDIR:-/tmp}/htmlclay-icons.XXXXXX")"
trap 'rm -rf "$BUILD"' EXIT

render() { rsvg-convert -w "$2" -h "$2" "$1" -o "$3"; }   # svg, size, out (square)

echo "Composing icon SVGs from masters..."
python3 - "$ICONS_DIR" "$BUILD" <<'PY'
import sys, re, math
icons, build = sys.argv[1], sys.argv[2]

ns = open(f"{icons}/blob-noshadow.svg").read()
# Whole drawn blob (body + flap outline/fill + master face), reused on tiles and the doc.
blob_group = re.search(r'<g id="blob">.*</g>', ns, re.S).group(0)
# Body + flap outlines, for the solid tray silhouette.
body, flap = re.findall(r'<path d="(.*?)" fill="none" stroke="#010101" stroke-width="46"/>', ns, re.S)

VB = 'viewBox="0 0 909 925"'          # master coordinate space
CONTENT_CX, CONTENT_CY = 467.5, 483.0  # centre of the visible blob within the viewBox

def squircle(cx, cy, a, n=5.0, steps=256):
    pts = []
    for i in range(steps):
        t = 2*math.pi*i/steps
        ct, st = math.cos(t), math.sin(t)
        x = cx + a*math.copysign(abs(ct)**(2.0/n), ct)
        y = cy + a*math.copysign(abs(st)**(2.0/n), st)
        pts.append(f"{x:.2f} {y:.2f}")
    return "M " + " L ".join(pts) + " Z"

def place(scale, cx, cy):
    tx = cx - CONTENT_CX*scale
    ty = cy - CONTENT_CY*scale
    return f'transform="translate({tx:.2f} {ty:.2f}) scale({scale:.4f})"'

# ---- App icon: blob on a black squircle, macOS-style inset on a 1024 canvas ----
sq = squircle(512, 512, 412)                 # 824px squircle, 100px margin
app = (f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1024 1024">\n'
       f'  <path d="{sq}" fill="#000000"/>\n'
       f'  <g {place(0.717, 512, 512)}>\n    {blob_group}\n  </g>\n</svg>\n')
open(f"{build}/app.svg", "w").write(app)

# ---- Document icon: line-art mark on a folded page, 1024 canvas ----
L, R, T, B, r, fold = 222, 802, 150, 890, 26, 132
page = (f'M {L+r} {T} L {R-fold} {T} L {R} {T+fold} L {R} {B-r} '
        f'A {r} {r} 0 0 1 {R-r} {B} L {L+r} {B} A {r} {r} 0 0 1 {L} {B-r} '
        f'L {L} {T+r} A {r} {r} 0 0 1 {L+r} {T} Z')
foldtri = f'M {R-fold} {T} L {R} {T+fold} L {R-fold} {T+fold} Z'
doc = (f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1024 1024">\n'
       f'  <path d="{page}" fill="#ffffff" stroke="#d7dbe2" stroke-width="4"/>\n'
       f'  <path d="{foldtri}" fill="#d7dbe2"/>\n'
       f'  <g {place(0.452, 512, 548)}>\n    {blob_group}\n  </g>\n</svg>\n')
open(f"{build}/doc.svg", "w").write(doc)

# ---- Tray template: solid black silhouette with the grin cut out (face up 24) ----
grin = ('    <circle cx="392" cy="430" r="36" fill="black"/>\n'
        '    <circle cx="548" cy="430" r="36" fill="black"/>\n'
        '    <path d="M 352 512 Q 470 632 588 512 Z" fill="black"/>')
tray_tpl = (f'<svg xmlns="http://www.w3.org/2000/svg" {VB}>\n'
            f'  <mask id="m" maskUnits="userSpaceOnUse" x="0" y="0" width="909" height="925">\n'
            f'    <rect width="909" height="925" fill="white"/>\n'
            f'    <g transform="translate(0 -24)">\n{grin}\n    </g>\n  </mask>\n'
            f'  <g mask="url(#m)" stroke-linejoin="round" stroke-linecap="round">\n'
            f'    <path d="{body}" fill="#000000" stroke="#000000" stroke-width="46"/>\n'
            f'    <path d="{flap}" fill="#000000" stroke="#000000" stroke-width="46"/>\n'
            f'  </g>\n</svg>\n')
open(f"{build}/tray-template.svg", "w").write(tray_tpl)
print("  app.svg  doc.svg  tray-template.svg")
PY

ICNS_SIZES=(16 32 64 128 256 512 1024)
echo "Rendering app icon..."
for s in "${ICNS_SIZES[@]}"; do render "$BUILD/app.svg" "$s" "$BUILD/app-$s.png"; done

echo "  -> dist/macos/htmlclay.icns"
ICONSET="$BUILD/htmlclay.iconset"; mkdir -p "$ICONSET"
cp "$BUILD/app-16.png"   "$ICONSET/icon_16x16.png"
cp "$BUILD/app-32.png"   "$ICONSET/icon_16x16@2x.png"
cp "$BUILD/app-32.png"   "$ICONSET/icon_32x32.png"
cp "$BUILD/app-64.png"   "$ICONSET/icon_32x32@2x.png"
cp "$BUILD/app-128.png"  "$ICONSET/icon_128x128.png"
cp "$BUILD/app-256.png"  "$ICONSET/icon_128x128@2x.png"
cp "$BUILD/app-256.png"  "$ICONSET/icon_256x256.png"
cp "$BUILD/app-512.png"  "$ICONSET/icon_256x256@2x.png"
cp "$BUILD/app-512.png"  "$ICONSET/icon_512x512.png"
cp "$BUILD/app-1024.png" "$ICONSET/icon_512x512@2x.png"
if command -v iconutil >/dev/null 2>&1; then
  iconutil -c icns "$ICONSET" -o "$ROOT_DIR/dist/macos/htmlclay.icns"
else
  echo "  (iconutil missing; using ImageMagick)"
  "$MAGICK" "$BUILD/app-16.png" "$BUILD/app-32.png" "$BUILD/app-128.png" \
    "$BUILD/app-256.png" "$BUILD/app-512.png" "$BUILD/app-1024.png" "$ROOT_DIR/dist/macos/htmlclay.icns"
fi

echo "  -> dist/windows/htmlclay.ico"
"$MAGICK" "$BUILD/app-256.png" -define icon:auto-resize=256,128,64,48,32,16 "$ROOT_DIR/dist/windows/htmlclay.ico"

echo "  -> dist/linux/htmlclay.png (256) + htmlclay.svg"
cp "$BUILD/app-256.png" "$ROOT_DIR/dist/linux/htmlclay.png"
cp "$BUILD/app.svg"     "$ROOT_DIR/dist/linux/htmlclay.svg"

echo "Rendering document icon -> dist/macos/doc.icns"
for s in "${ICNS_SIZES[@]}"; do render "$BUILD/doc.svg" "$s" "$BUILD/doc-$s.png"; done
DOCSET="$BUILD/doc.iconset"; mkdir -p "$DOCSET"
cp "$BUILD/doc-16.png"   "$DOCSET/icon_16x16.png"
cp "$BUILD/doc-32.png"   "$DOCSET/icon_16x16@2x.png"
cp "$BUILD/doc-32.png"   "$DOCSET/icon_32x32.png"
cp "$BUILD/doc-64.png"   "$DOCSET/icon_32x32@2x.png"
cp "$BUILD/doc-128.png"  "$DOCSET/icon_128x128.png"
cp "$BUILD/doc-256.png"  "$DOCSET/icon_128x128@2x.png"
cp "$BUILD/doc-256.png"  "$DOCSET/icon_256x256.png"
cp "$BUILD/doc-512.png"  "$DOCSET/icon_256x256@2x.png"
cp "$BUILD/doc-512.png"  "$DOCSET/icon_512x512.png"
cp "$BUILD/doc-1024.png" "$DOCSET/icon_512x512@2x.png"
if command -v iconutil >/dev/null 2>&1; then
  iconutil -c icns "$DOCSET" -o "$ROOT_DIR/dist/macos/doc.icns"
else
  "$MAGICK" "$BUILD/doc-16.png" "$BUILD/doc-32.png" "$BUILD/doc-128.png" \
    "$BUILD/doc-256.png" "$BUILD/doc-512.png" "$BUILD/doc-1024.png" "$ROOT_DIR/dist/macos/doc.icns"
fi

echo "Rendering tray icons -> tray/icon.png + tray/icon-template.png"
# Colored grin (Windows/Linux systray + current SetIcon fallback): trim and pad to a square.
rsvg-convert -w 256 -h 261 "$ICONS_DIR/blob-grin.svg" -o "$BUILD/grin.png"
"$MAGICK" "$BUILD/grin.png" -trim +repage -background none -gravity center -extent 280x280 \
  -resize 128x128 "$ROOT_DIR/tray/icon.png"
# Black template (macOS menu bar, auto-inverts via SetTemplateIcon).
rsvg-convert -w 256 -h 261 "$BUILD/tray-template.svg" -o "$BUILD/tpl.png"
"$MAGICK" "$BUILD/tpl.png" -trim +repage -background none -gravity center -extent 300x300 \
  -resize 64x64 "$ROOT_DIR/tray/icon-template.png"

echo
echo "Done. Generated:"
for f in tray/icon.png tray/icon-template.png \
         dist/macos/htmlclay.icns dist/macos/doc.icns \
         dist/linux/htmlclay.png dist/linux/htmlclay.svg \
         dist/windows/htmlclay.ico; do
  printf "  %-32s %s\n" "$f" "$(cd "$ROOT_DIR" && du -h "$f" 2>/dev/null | cut -f1)"
done
