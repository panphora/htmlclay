// Package clayruntime answers ClayJS runtime requests from a release vendored into the
// binary, and projects served HTML onto the local URLs that release is served from.
package clayruntime

import (
	"embed"
	"encoding/json"
	"fmt"
	"regexp"
)

//go:embed assets
var assets embed.FS

// File is one entry of a release index: the bytes' digest, served type and length.
type File struct {
	SHA256 string `json:"sha256"`
	Type   string `json:"type"`
	Bytes  int64  `json:"bytes"`
}

// Release is one vendored ClayJS release: its index of public paths and its aliases.
type Release struct {
	Version string            `json:"version"`
	Files   map[string]File   `json:"files"`
	Aliases map[string]string `json:"aliases"`
}

// Releases is every vendored release, keyed by version.
var Releases map[string]Release

// Selected is the release the rolling `v<major>` URLs point at.
var Selected string

type lockFile struct {
	Selected string   `json:"selected"`
	Versions []string `json:"versions"`
}

var sha256Shape = regexp.MustCompile(`^[0-9a-f]{64}$`)

func init() {
	lock, err := loadLock()
	if err != nil {
		panic(fmt.Sprintf("clayruntime: %v", err))
	}
	Releases = make(map[string]Release, len(lock.Versions))
	for _, version := range lock.Versions {
		release, err := loadRelease(version)
		if err != nil {
			panic(fmt.Sprintf("clayruntime: %v", err))
		}
		Releases[version] = release
	}
	if _, ok := Releases[lock.Selected]; !ok {
		panic(fmt.Sprintf("clayruntime: assets/lock.json selects %q, which it does not list", lock.Selected))
	}
	Selected = lock.Selected
}

func loadLock() (lockFile, error) {
	var lock lockFile
	bytes, err := assets.ReadFile("assets/lock.json")
	if err != nil {
		return lock, err
	}
	if err := json.Unmarshal(bytes, &lock); err != nil {
		return lock, fmt.Errorf("assets/lock.json: %w", err)
	}
	if lock.Selected == "" || len(lock.Versions) == 0 {
		return lock, fmt.Errorf("assets/lock.json names no release")
	}
	return lock, nil
}

func loadRelease(version string) (Release, error) {
	var release Release
	bytes, err := assets.ReadFile("assets/" + version + "/offline-manifest.json")
	if err != nil {
		return release, err
	}
	if err := json.Unmarshal(bytes, &release); err != nil {
		return release, fmt.Errorf("%s: %w", version, err)
	}
	if release.Version != version {
		return release, fmt.Errorf("%s: the index names version %q", version, release.Version)
	}
	if len(release.Files) == 0 {
		return release, fmt.Errorf("%s: the index lists no files", version)
	}
	for name, file := range release.Files {
		if !sha256Shape.MatchString(file.SHA256) {
			return release, fmt.Errorf("%s: %s has sha256 %q", version, name, file.SHA256)
		}
		if file.Bytes <= 0 {
			return release, fmt.Errorf("%s: %s is %d bytes", version, name, file.Bytes)
		}
	}
	if release.Aliases == nil {
		release.Aliases = map[string]string{}
	}
	return release, nil
}

// Asset returns the index entry and the embedded bytes of one release file. It only
// answers for paths the release index lists, so no path is ever built from user input.
func Asset(version, name string) (File, []byte, bool) {
	release, ok := Releases[version]
	if !ok {
		return File{}, nil, false
	}
	file, ok := release.Files[name]
	if !ok {
		return File{}, nil, false
	}
	bytes, err := assets.ReadFile("assets/" + version + "/" + name)
	if err != nil {
		return File{}, nil, false
	}
	return file, bytes, true
}
