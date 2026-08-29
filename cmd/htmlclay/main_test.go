package main

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/panphora/htmlclay/internal/htmlutil"
)

func TestFileURL(t *testing.T) {
	cases := []struct{ rel, want string }{
		{"a.htmlclay", "http://127.0.0.1:8080/a.htmlclay"},
		{"dir/a.htmlclay", "http://127.0.0.1:8080/dir/a.htmlclay"},
	}
	for _, c := range cases {
		if got := fileURL(8080, c.rel); got != c.want {
			t.Errorf("fileURL(8080, %q) = %q, want %q", c.rel, got, c.want)
		}
	}
}

func TestFileURLEscapesSpecialChars(t *testing.T) {
	got := fileURL(8080, "my file & test.htmlclay")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("fileURL produced unparseable URL %q: %v", got, err)
	}
	if u.Path != "/my file & test.htmlclay" {
		t.Errorf("decoded path = %q, want /my file & test.htmlclay", u.Path)
	}
}

func TestExampleEmbedded(t *testing.T) {
	if len(exampleHTML) == 0 {
		t.Fatal("example.htmlclay not embedded")
	}
	if !htmlutil.HasHTMLTag(exampleHTML) {
		t.Fatal("embedded example is not an HTML document")
	}
	// Both spellings: a template shipping a hardcoded id would hand the same
	// version history to every copy of the example anyone ever opens, and the
	// legacy name would do it just as thoroughly as the current one.
	if htmlutil.ReadHTMLClayID(exampleHTML) != "" {
		t.Fatal("example template must ship without a document id; the server assigns one")
	}
}

// The example is the first file most people open, and its save is inline script
// reading the token attribute by name. Nothing else in the tree connects the name
// the server injects to the name this document reads, so a rename on one side
// leaves an example that loads perfectly and silently cannot save.
//
// The injected name is derived, not written out, so this test cannot drift the way
// the example did.
func TestExampleReadsTheTokenNameTheServerInjects(t *testing.T) {
	injected := regexp.MustCompile(`\s([a-zA-Z]+)="SENTINEL"`).
		FindStringSubmatch(string(htmlutil.InjectToken([]byte("<html></html>"), "SENTINEL")))
	if injected == nil {
		t.Fatal("could not determine the injected token attribute name")
	}

	var read []string
	for _, m := range regexp.MustCompile(`getAttribute\('([^']+)'\)`).FindAllStringSubmatch(string(exampleHTML), -1) {
		read = append(read, m[1])
	}
	if len(read) == 0 {
		t.Fatal("the example reads no attributes at all; this test proves nothing")
	}
	for _, name := range read {
		if name == injected[1] {
			return
		}
	}
	t.Errorf("the example never reads %q, the attribute the server injects; it reads %v", injected[1], read)
}

func TestEnsureExampleFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "htmlclay", "examples", "welcome.htmlclay")
	if err := ensureExampleFile(path); err != nil {
		t.Fatalf("create: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		t.Fatalf("example not written: %v", err)
	}
	edited := []byte("<!DOCTYPE html>\n<html><body>edited</body></html>")
	os.WriteFile(path, edited, 0644)
	if err := ensureExampleFile(path); err != nil {
		t.Fatalf("second call: %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(edited) {
		t.Error("ensureExampleFile overwrote an existing example")
	}
}
