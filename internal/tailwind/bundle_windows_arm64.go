//go:build tailwind_bundle && windows && arm64

package tailwind

import _ "embed"

//go:embed blobs/windows-arm64.gz
var bundled []byte

//go:embed blobs/windows-arm64.sha256
var bundledSum string
