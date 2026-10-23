#!/bin/bash
set -euo pipefail

VERSION="${VERSION:-$(grep 'var version' cmd/htmlclay/main.go | sed 's/.*"\(.*\)"/\1/')}"

target_goarch="$(go env GOARCH)"
GOOS= GOARCH= go run ./cmd/fetchtailwind -goos linux -goarch "$target_goarch"
CGO_ENABLED=0 go build -tags tailwind_bundle -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o htmlclay ./cmd/htmlclay
go version -m htmlclay | grep -q -- '-tags=tailwind_bundle' || { echo "error: htmlclay was built without the Tailwind bundle" >&2; exit 1; }

echo "Built htmlclay v${VERSION}"
echo ""
echo "To install (into ~/.local/bin, no sudo):"
echo "  bash packaging/linux/install.sh"
echo ""
echo "To remove it again:"
echo "  bash packaging/linux/uninstall.sh"
