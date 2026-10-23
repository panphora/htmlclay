package tailwind

// Asset is one upstream release file and its published SHA-256
// (github.com/tailwindlabs/tailwindcss/releases/download/v4.1.17/sha256sums.txt).
type Asset struct {
	Name   string
	SHA256 string
}

// Assets maps GOOS/GOARCH to the upstream binary that build embeds. Windows on
// ARM runs the x64 build under Windows 11's emulation; Tailwind ships no arm64
// Windows binary.
var Assets = map[string]Asset{
	"darwin/arm64":  {"tailwindcss-macos-arm64", "852b62d91a6dfa56969686bc8a4bae0e4722de8f5a9b42b102bb589b7ece8cfe"},
	"darwin/amd64":  {"tailwindcss-macos-x64", "6337c87185321c079244dfbd9c24502a3006401bd1539d19cdc9df8de9101843"},
	"linux/amd64":   {"tailwindcss-linux-x64", "cc115d9b6c4ede4e423bfea6a3cfc3f03e6c1702b7d910369b9540e2b4cf3860"},
	"linux/arm64":   {"tailwindcss-linux-arm64", "264689984311cc2ca104a9d6a4d039b4267aecf4543dc9ea0bbc094eeb0ccc8d"},
	"windows/amd64": {"tailwindcss-windows-x64.exe", "7170cedd7cfa57c8d35b46d211f350a8afcab2ef55bbe25179e68684e373e3b7"},
	"windows/arm64": {"tailwindcss-windows-x64.exe", "7170cedd7cfa57c8d35b46d211f350a8afcab2ef55bbe25179e68684e373e3b7"},
}

// AssetURL is where an asset is downloaded from.
func AssetURL(name string) string {
	return "https://github.com/tailwindlabs/tailwindcss/releases/download/v" + Version + "/" + name
}
