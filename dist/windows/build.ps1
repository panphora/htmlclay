$ErrorActionPreference = "Stop"

if (-not $env:VERSION) {
    $env:VERSION = (Select-String -Path "main.go" -Pattern 'var version = "(.+)"' | ForEach-Object { $_.Matches.Groups[1].Value })
}

$env:CGO_ENABLED = "1"
go build -trimpath -ldflags="-s -w -X main.version=$($env:VERSION)" -o htmlclay.exe .

Write-Host "Built htmlclay.exe v$($env:VERSION)"
