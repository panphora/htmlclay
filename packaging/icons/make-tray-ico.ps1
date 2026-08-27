# make-tray-ico.ps1 — build internal/tray/icon.ico from internal/tray/icon.png.
#
# Why this exists as a separate script rather than a step in generate.sh: the tray
# needs an ICO on Windows (systray's SetIcon feeds the bytes to LoadImage, which
# rejects a PNG), but generate.sh requires rsvg-convert + ImageMagick, neither of
# which is standard on a Windows box. This uses System.Drawing so a Windows dev can
# regenerate the tray icon without installing a toolchain.
#
# Source is internal/tray/icon.png rather than packaging/windows/htmlclay.ico on purpose: the tray
# art is blob-grin.svg (a bigger grin that stays legible small), while the app icon
# comes from a different master. Reusing the app icon here would quietly lose that.
#
# Run from the repo root:  powershell -File packaging/icons/make-tray-ico.ps1

$ErrorActionPreference = "Stop"
Add-Type -AssemblyName System.Drawing

$src = "internal/tray/icon.png"
$out = "internal/tray/icon.ico"
if (-not (Test-Path $src)) { throw "missing $src (run from the repo root)" }

# 16 and 32 are what Windows actually asks for in the notification area (32 at
# 200% DPI); 48 is cheap insurance for higher scaling.
$sizes = @(16, 32, 48)

$png = [System.Drawing.Image]::FromFile((Resolve-Path $src).Path)
$frames = @()
try {
    foreach ($s in $sizes) {
        $bmp = New-Object System.Drawing.Bitmap($s, $s, [System.Drawing.Imaging.PixelFormat]::Format32bppArgb)
        $g = [System.Drawing.Graphics]::FromImage($bmp)
        $g.InterpolationMode = [System.Drawing.Drawing2D.InterpolationMode]::HighQualityBicubic
        $g.PixelOffsetMode  = [System.Drawing.Drawing2D.PixelOffsetMode]::HighQuality
        $g.SmoothingMode    = [System.Drawing.Drawing2D.SmoothingMode]::HighQuality
        $g.Clear([System.Drawing.Color]::Transparent)
        $g.DrawImage($png, (New-Object System.Drawing.Rectangle(0, 0, $s, $s)))
        $g.Dispose()

        $ms = New-Object System.IO.MemoryStream
        $bmp.Save($ms, [System.Drawing.Imaging.ImageFormat]::Png)
        $bmp.Dispose()
        $frames += ,@{ Size = $s; Bytes = $ms.ToArray() }
        $ms.Dispose()
    }
} finally { $png.Dispose() }

# ICO container: 6-byte ICONDIR, then one 16-byte ICONDIRENTRY per image, then the
# image payloads. Each payload here is a PNG, which every Windows since Vista reads.
$fs = [System.IO.File]::Create((Join-Path (Get-Location) $out))
try {
    $bw = New-Object System.IO.BinaryWriter($fs)
    $bw.Write([UInt16]0)                 # reserved
    $bw.Write([UInt16]1)                 # type: 1 = icon
    $bw.Write([UInt16]$frames.Count)

    $offset = 6 + (16 * $frames.Count)
    foreach ($f in $frames) {
        # 0 in the width/height byte means 256; our sizes are all < 256.
        $bw.Write([Byte]$f.Size)
        $bw.Write([Byte]$f.Size)
        $bw.Write([Byte]0)               # palette count (0 = truecolor)
        $bw.Write([Byte]0)               # reserved
        $bw.Write([UInt16]1)             # color planes
        $bw.Write([UInt16]32)            # bits per pixel
        $bw.Write([UInt32]$f.Bytes.Length)
        $bw.Write([UInt32]$offset)
        $offset += $f.Bytes.Length
    }
    foreach ($f in $frames) { $bw.Write($f.Bytes) }
    $bw.Flush()
} finally { $fs.Dispose() }

Write-Host "Wrote $out ($((Get-Item $out).Length) bytes, sizes: $($sizes -join ', '))"
