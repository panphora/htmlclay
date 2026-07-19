package main

import (
	"bytes"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/panphora/htmlclay/htmlutil"
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
	if bytes.Contains(exampleHTML, []byte("htmlclayid=")) {
		t.Fatal("example template must ship without an htmlclayid; the server assigns one")
	}
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
