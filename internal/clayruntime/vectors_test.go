package clayruntime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type urlVector struct {
	Name       string          `json:"name"`
	Fn         string          `json:"fn"`
	Input      string          `json:"input"`
	Descriptor json.RawMessage `json:"descriptor"`
	NoReleases bool            `json:"noReleases"`
	Expect     json.RawMessage `json:"expect"`
}

type urlFixture struct {
	Cases []urlVector `json:"cases"`
}

type releasesFixture struct {
	SelectedVersion string             `json:"selectedVersion"`
	DocumentURL     string             `json:"documentURL"`
	Releases        map[string]Release `json:"releases"`
}

type htmlVector struct {
	Name      string          `json:"name"`
	Input     string          `json:"input"`
	Project   json.RawMessage `json:"project"`
	Canonical string          `json:"canonical"`
}

type htmlFixture struct {
	Cases         []htmlVector `json:"cases"`
	CanonicalOnly []htmlVector `json:"canonicalOnly"`
}

func readVector(t *testing.T, name string) []byte {
	t.Helper()
	bytes, err := os.ReadFile(filepath.Join("testdata", "vectors", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return bytes
}

func loadReleases(t *testing.T) (releasesFixture, Options) {
	t.Helper()
	var fixture releasesFixture
	if err := json.Unmarshal(readVector(t, "releases.json"), &fixture); err != nil {
		t.Fatalf("decode releases.json: %v", err)
	}
	if len(fixture.Releases) == 0 {
		t.Fatal("releases.json names no releases")
	}
	return fixture, Options{
		Releases:        fixture.Releases,
		SelectedVersion: fixture.SelectedVersion,
		DocumentURL:     fixture.DocumentURL,
	}
}

func toJSON(t *testing.T, value any) any {
	t.Helper()
	bytes, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return fromJSON(t, bytes)
}

func fromJSON(t *testing.T, bytes []byte) any {
	t.Helper()
	var value any
	if err := json.Unmarshal(bytes, &value); err != nil {
		t.Fatalf("decode %s: %v", bytes, err)
	}
	return value
}

func assertJSONEqual(t *testing.T, what string, actual, expected any) {
	t.Helper()
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("%s:\n actual: %s\n expect: %s", what, marshalString(t, actual), marshalString(t, expected))
	}
}

func marshalString(t *testing.T, value any) string {
	t.Helper()
	bytes, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "<unmarshalable>"
	}
	return string(bytes)
}

func TestURLVectorsMatchClayJS(t *testing.T) {
	_, options := loadReleases(t)
	var fixture urlFixture
	if err := json.Unmarshal(readVector(t, "urls.json"), &fixture); err != nil {
		t.Fatalf("decode urls.json: %v", err)
	}
	if len(fixture.Cases) < 44 {
		t.Fatalf("urls.json carries %d cases", len(fixture.Cases))
	}
	answered := 0
	rejected := 0
	for _, vector := range fixture.Cases {
		if vector.Expect == nil {
			t.Fatalf("%s: the vector carries no expectation", vector.Name)
		}
		if string(vector.Expect) == "null" {
			rejected++
		} else {
			answered++
		}
		var descriptor *Descriptor
		if len(vector.Descriptor) > 0 && string(vector.Descriptor) != "null" {
			descriptor = ParseDescriptor(vector.Descriptor)
			if descriptor == nil {
				t.Fatalf("%s: the vector descriptor does not parse", vector.Name)
			}
		}
		var actual any
		switch vector.Fn {
		case "resolve":
			if result := ResolveRuntimeURL(vector.Input, options); result != nil {
				actual = toJSON(t, result)
			}
		case "canonicalize":
			releases := options.Releases
			if vector.NoReleases {
				releases = nil
			}
			actual = CanonicalizeRuntimeURL(vector.Input, CanonOptions{
				Descriptor:  descriptor,
				Releases:    releases,
				DocumentURL: options.DocumentURL,
			})
		default:
			t.Fatalf("%s: unknown fn %q", vector.Name, vector.Fn)
		}
		assertJSONEqual(t, vector.Name, actual, fromJSON(t, vector.Expect))
	}
	if answered < 10 {
		t.Fatalf("urls.json answers only %d URLs", answered)
	}
	if rejected < 15 {
		t.Fatalf("urls.json rejects only %d URLs", rejected)
	}
}

func TestHTMLVectorsMatchClayJS(t *testing.T) {
	_, options := loadReleases(t)
	var vectors htmlFixture
	if err := json.Unmarshal(readVector(t, "html.json"), &vectors); err != nil {
		t.Fatalf("decode html.json: %v", err)
	}
	if len(vectors.Cases) < 12 {
		t.Fatalf("html.json carries %d cases", len(vectors.Cases))
	}
	for _, vector := range vectors.Cases {
		projection := ProjectRuntimeHTML([]byte(vector.Input), options)
		assertJSONEqual(t, vector.Name+" project", toJSON(t, projection), fromJSON(t, vector.Project))
		canonical := CanonicalizeRuntimeHTML([]byte(projection.HTML), options.Releases, options.DocumentURL)
		if canonical.HTML != vector.Canonical {
			t.Fatalf("%s canonical:\n actual: %s\n expect: %s", vector.Name, canonical.HTML, vector.Canonical)
		}
		if len(projection.Mappings) > 0 && canonical.HTML != vector.Input {
			t.Fatalf("%s round trip:\n actual: %s\n expect: %s", vector.Name, canonical.HTML, vector.Input)
		}
	}

	if len(vectors.CanonicalOnly) < 4 {
		t.Fatalf("html.json carries %d canonical-only cases", len(vectors.CanonicalOnly))
	}
	for _, vector := range vectors.CanonicalOnly {
		canonical := CanonicalizeRuntimeHTML([]byte(vector.Input), options.Releases, options.DocumentURL)
		if canonical.HTML != vector.Canonical {
			t.Fatalf("%s canonical:\n actual: %s\n expect: %s", vector.Name, canonical.HTML, vector.Canonical)
		}
	}
}

func TestHTMLWithoutRuntimeURLIsByteIdentical(t *testing.T) {
	_, options := loadReleases(t)
	var vectors htmlFixture
	if err := json.Unmarshal(readVector(t, "html.json"), &vectors); err != nil {
		t.Fatalf("decode html.json: %v", err)
	}
	var input string
	for _, vector := range vectors.Cases {
		if vector.Name == "no-runtime-url" {
			input = vector.Input
		}
	}
	if input == "" {
		t.Fatal("html.json has no no-runtime-url case")
	}
	canonical := CanonicalizeRuntimeHTML([]byte(input), options.Releases, options.DocumentURL)
	if canonical.HTML != input {
		t.Fatalf("canonicalize changed the document:\n actual: %s\n expect: %s", canonical.HTML, input)
	}
	if canonical.Changed != 0 {
		t.Fatalf("canonicalize reports %d changes", canonical.Changed)
	}
	if canonical.DescriptorRemoved {
		t.Fatal("canonicalize removed a descriptor the document does not carry")
	}
	projection := ProjectRuntimeHTML([]byte(input), options)
	if len(projection.Mappings) != 0 || projection.HTML != input {
		t.Fatalf("projection rewrote a document with no runtime URL")
	}
}

// A large document that names no runtime URL must not be parsed: the fast path
// scans for the strings that could name one and hands back the bytes untouched.
func TestLargeHTMLWithoutRuntimeURLIsNotParsed(t *testing.T) {
	_, options := loadReleases(t)
	var builder strings.Builder
	builder.WriteString("<!DOCTYPE html>\n<html><head><title>Big</title></head><body>\n")
	for builder.Len() < 4<<20 {
		builder.WriteString("<p class=\"row\">a plain paragraph with some words in it</p>\n")
	}
	builder.WriteString("</body></html>\n")
	source := []byte(builder.String())

	projection := ProjectRuntimeHTML(source, options)
	if projection.HTML != string(source) || len(projection.Mappings) != 0 {
		t.Fatal("projection rewrote a document with no runtime URL")
	}

	allocs := testing.AllocsPerRun(3, func() { ProjectRuntimeHTML(source, options) })
	if allocs > 100 {
		t.Fatalf("projecting a %d byte document with no runtime URL took %v allocations per call, want the tokenizer scan skipped", len(source), allocs)
	}
}
