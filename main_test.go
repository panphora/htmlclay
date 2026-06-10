package main

import (
	"net/url"
	"testing"
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
