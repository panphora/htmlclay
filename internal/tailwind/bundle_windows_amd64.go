//go:build tailwind_bundle && windows && amd64

package tailwind

import _ "embed"

//go:embed blobs/windows-amd64.gz
var bundled []byte

//go:embed blobs/windows-amd64.sha256
var bundledSum string
