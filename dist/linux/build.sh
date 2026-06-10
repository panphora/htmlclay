#!/bin/bash
set -euo pipefail

VERSION="${VERSION:-$(grep 'var version' main.go | sed 's/.*"\(.*\)"/\1/')}"

CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o htmlclay .

echo "Built htmlclay v${VERSION}"
echo ""
echo "To install:"
echo "  sudo cp htmlclay /usr/local/bin/"
echo "  cp dist/linux/htmlclay.desktop ~/.local/share/applications/"
echo "  cp dist/linux/htmlclay-mime.xml ~/.local/share/mime/packages/"
echo "  bash dist/linux/install-icon.sh"
echo "  update-mime-database ~/.local/share/mime"
echo "  update-desktop-database ~/.local/share/applications"
