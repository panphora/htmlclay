#!/bin/bash
set -euo pipefail

VERSION="${VERSION:-$(grep 'var version' main.go | sed 's/.*"\(.*\)"/\1/')}"

CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o htmlclay .

echo "Built htmlclay v${VERSION}"
echo ""
echo "To install (into ~/.local/bin, no sudo):"
echo "  bash dist/linux/install.sh"
echo ""
echo "To remove it again:"
echo "  bash dist/linux/uninstall.sh"
