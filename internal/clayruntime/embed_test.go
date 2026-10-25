package clayruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestEmbeddedReleaseLoads(t *testing.T) {
	if Selected == "" {
		t.Fatal("no release is selected")
	}
	release, ok := Releases[Selected]
	if !ok {
		t.Fatalf("the selected release %q is not loaded", Selected)
	}
	if release.Version != Selected {
		t.Fatalf("the selected release names version %q", release.Version)
	}
	if len(release.Files) < 400 {
		t.Fatalf("the selected release lists %d files", len(release.Files))
	}
	if len(Releases) < 1 {
		t.Fatal("no releases are loaded")
	}
}

func TestEmbeddedReleaseBytesMatchTheIndex(t *testing.T) {
	release, ok := Releases[Selected]
	if !ok {
		t.Fatalf("the selected release %q is not loaded", Selected)
	}
	if len(release.Files) == 0 {
		t.Fatal("the selected release lists no files")
	}
	checked := 0
	for name, entry := range release.Files {
		file, bytes, ok := Asset(Selected, name)
		if !ok {
			t.Fatalf("%s: the asset is not embedded", name)
		}
		if file != entry {
			t.Fatalf("%s: the asset index entry is %+v, the release index records %+v", name, file, entry)
		}
		sum := sha256.Sum256(bytes)
		if digest := hex.EncodeToString(sum[:]); digest != entry.SHA256 {
			t.Fatalf("%s: sha256 is %s, the release index records %s", name, digest, entry.SHA256)
		}
		if int64(len(bytes)) != entry.Bytes {
			t.Fatalf("%s: is %d bytes, the release index records %d", name, len(bytes), entry.Bytes)
		}
		checked++
	}
	if checked < 400 {
		t.Fatalf("checked %d files", checked)
	}
}

func TestAssetRefusesPathsTheIndexDoesNotList(t *testing.T) {
	release, ok := Releases[Selected]
	if !ok {
		t.Fatalf("the selected release %q is not loaded", Selected)
	}
	if len(release.Files) == 0 {
		t.Fatal("the selected release lists no files")
	}
	refused := 0
	for _, name := range []string{
		"../go.mod",
		"../../go.mod",
		"../1.15.2/clay.js",
		"clay.js/../clay.js",
		"./clay.js",
		"/clay.js",
		"..",
		".",
		"",
		"clay.js/../../go.mod",
		"offline-manifest.json",
		"\\clay.js",
		"missing.js",
	} {
		if _, bytes, ok := Asset(Selected, name); ok || bytes != nil {
			t.Fatalf("Asset(%q) answered", name)
		}
		refused++
	}
	if _, _, ok := Asset("0.0.0", "clay.js"); ok {
		t.Fatal("Asset answered for a release that is not vendored")
	}
	if _, _, ok := Asset("", ""); ok {
		t.Fatal("Asset answered for an empty request")
	}
	if refused != 13 {
		t.Fatalf("refused %d spellings", refused)
	}
}
