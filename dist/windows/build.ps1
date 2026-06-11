$ErrorActionPreference = "Stop"

if (-not $env:VERSION) {
    $env:VERSION = (Select-String -Path "main.go" -Pattern 'var version = "(.+)"' | ForEach-Object { $_.Matches.Groups[1].Value })
}

# Embed the app icon + version metadata into the .exe via a Windows resource.
# goversioninfo writes resource_windows_amd64.syso, which `go build` links automatically.
$nums = [regex]::Matches($env:VERSION, '\d+') | ForEach-Object { $_.Value }
$maj = if ($nums.Count -gt 0) { $nums[0] } else { "0" }
$min = if ($nums.Count -gt 1) { $nums[1] } else { "0" }
$pat = if ($nums.Count -gt 2) { $nums[2] } else { "0" }
go run github.com/josephspurrier/goversioninfo/cmd/goversioninfo@v1.4.1 `
    -64 -o resource_windows_amd64.syso `
    -icon dist/windows/htmlclay.ico `
    -file-version $env:VERSION -product-version $env:VERSION `
    -ver-major $maj -ver-minor $min -ver-patch $pat `
    -product-ver-major $maj -product-ver-minor $min -product-ver-patch $pat `
    dist/windows/versioninfo.json
if ($LASTEXITCODE -ne 0) {
    Write-Error "goversioninfo failed"
    exit $LASTEXITCODE
}

$env:CGO_ENABLED = "0"
go build -trimpath -ldflags="-s -w -X main.version=$($env:VERSION)" -o htmlclay.exe .
if ($LASTEXITCODE -ne 0) {
    Write-Error "go build failed"
    exit $LASTEXITCODE
}

Write-Host "Built htmlclay.exe v$($env:VERSION)"
