//go:build !tailwind_bundle || !((darwin && (arm64 || amd64)) || (linux && (amd64 || arm64)) || (windows && (amd64 || arm64)))

package tailwind

var bundled []byte

var bundledSum string
