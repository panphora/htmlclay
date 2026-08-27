$ErrorActionPreference = "Stop"

if (-not $env:VERSION) {
    $env:VERSION = (Select-String -Path "cmd/htmlclay/main.go" -Pattern 'var version = "(.+)"' | ForEach-Object { $_.Matches.Groups[1].Value })
}

# Embed the icons + version metadata into the .exe via a Windows resource.
# go-winres writes resource_windows_<arch>.syso, which `go build` links
# automatically for the matching GOARCH; both are written so a build on Windows
# on ARM works from the same command.
#
# go-winres rather than goversioninfo because the exe carries TWO icon groups.
# goversioninfo takes one -icon and numbers it itself; winres.json names the
# resource IDs, and the document icon's ID is what the DefaultIcon registry value
# points at (see docIconIndex in internal/platform/register_windows.go).
go run github.com/tc-hib/go-winres@v0.3.1 make `
    --in packaging/windows/winres.json `
    --out cmd/htmlclay/resource `
    --arch amd64,arm64 `
    --file-version $env:VERSION --product-version $env:VERSION
if ($LASTEXITCODE -ne 0) {
    Write-Error "go-winres failed"
    exit $LASTEXITCODE
}

$env:CGO_ENABLED = "0"
go build -trimpath -ldflags="-s -w -X main.version=$($env:VERSION)" -o htmlclay.exe ./cmd/htmlclay
if ($LASTEXITCODE -ne 0) {
    Write-Error "go build failed"
    exit $LASTEXITCODE
}

Write-Host "Built htmlclay.exe v$($env:VERSION)"
