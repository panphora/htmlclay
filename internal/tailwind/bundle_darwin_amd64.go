//go:build tailwind_bundle && darwin && amd64

package tailwind

import _ "embed"

//go:embed blobs/darwin-amd64.gz
var bundled []byte

//go:embed blobs/darwin-amd64.sha256
var bundledSum string
