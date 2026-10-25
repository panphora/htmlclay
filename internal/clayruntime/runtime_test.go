package clayruntime

import (
	"strings"
	"testing"
)

// A descriptor is the bytes between the script tags, so anything after the first JSON
// value is not part of it: accepting the junk would let a page's descriptor carry
// whatever follows it.
func TestParseDescriptorRefusesTrailingData(t *testing.T) {
	clean := []byte(`{"schema":1,"version":"1.15.2","entries":[]}`)
	if ParseDescriptor(clean) == nil {
		t.Fatal("a clean descriptor was refused")
	}
	if ParseDescriptor(append(append([]byte(nil), clean...), []byte(" junk")...)) != nil {
		t.Fatal("a descriptor with trailing junk was accepted")
	}
}

// Frames are projected without a descriptor: the receiving tab already holds one, and an
// inserted element would shift live sync's identity map.
func TestProjectRuntimeHTMLWithoutDescriptor(t *testing.T) {
	source := []byte(`<html><head><script src="https://clayjs.com/v1/clay.js"></script></head><body></body></html>`)
	projection := ProjectRuntimeHTML(source, Options{
		Releases:        Releases,
		SelectedVersion: Selected,
		NoDescriptor:    true,
	})
	if len(projection.Mappings) != 1 {
		t.Fatalf("mapped %d runtime URLs, want 1: %v", len(projection.Mappings), projection.Mappings)
	}
	if !strings.Contains(projection.HTML, "/_/clayjs/"+Selected+"/clay.js") {
		t.Errorf("the runtime URL was not projected: %s", projection.HTML)
	}
	if strings.Contains(projection.HTML, descriptorID) {
		t.Errorf("a descriptor was written: %s", projection.HTML)
	}
}

// An alias is a public URL the release carries a copy of, so a page naming only that URL
// is projected onto this host's copy of it.
func TestProjectRuntimeHTMLProjectsEveryAlias(t *testing.T) {
	aliases := Releases[Selected].Aliases
	if len(aliases) == 0 {
		t.Fatal("the selected release carries no aliases")
	}
	for alias, path := range aliases {
		t.Run(alias, func(t *testing.T) {
			source := `<html><head><script src="` + alias + `"></script></head><body></body></html>`
			if strings.HasSuffix(path, ".css") {
				source = `<html><head><link rel="stylesheet" href="` + alias + `"></head><body></body></html>`
			}
			projection := ProjectRuntimeHTML([]byte(source), Options{
				Releases:        Releases,
				SelectedVersion: Selected,
			})
			local := LocalPrefix + Selected + "/" + path
			if len(projection.Mappings) != 1 || projection.Mappings[0].Local != local {
				t.Fatalf("mapped %v, want %s", projection.Mappings, local)
			}
			if !strings.Contains(projection.HTML, local) {
				t.Errorf("the alias was not projected: %s", projection.HTML)
			}
		})
	}
}
