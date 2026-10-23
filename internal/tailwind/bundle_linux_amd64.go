//go:build tailwind_bundle && linux && amd64

package tailwind

import _ "embed"

//go:embed blobs/linux-amd64.gz
var bundled []byte

//go:embed blobs/linux-amd64.sha256
var bundledSum string
