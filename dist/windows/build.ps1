$ErrorActionPreference = "Stop"

if (-not $env:VERSION) {
    $env:VERSION = (Select-String -Path "main.go" -Pattern 'var version = "(.+)"' | ForEach-Object { $_.Matches.Groups[1].Value })
}

$env:CGO_ENABLED = "0"
go build -trimpath -ldflags="-s -w -X main.version=$($env:VERSION)" -o htmlclay.exe .
if ($LASTEXITCODE -ne 0) {
    Write-Error "go build failed"
    exit $LASTEXITCODE
}

Write-Host "Built htmlclay.exe v$($env:VERSION)"
