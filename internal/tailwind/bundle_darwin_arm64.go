//go:build tailwind_bundle && darwin && arm64

package tailwind

import _ "embed"

//go:embed blobs/darwin-arm64.gz
var bundled []byte

//go:embed blobs/darwin-arm64.sha256
var bundledSum string
