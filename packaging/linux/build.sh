#!/bin/bash
set -euo pipefail

VERSION="${VERSION:-$(grep 'var version' cmd/htmlclay/main.go | sed 's/.*"\(.*\)"/\1/')}"

CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o htmlclay ./cmd/htmlclay

echo "Built htmlclay v${VERSION}"
echo ""
echo "To install (into ~/.local/bin, no sudo):"
echo "  bash packaging/linux/install.sh"
echo ""
echo "To remove it again:"
echo "  bash packaging/linux/uninstall.sh"
