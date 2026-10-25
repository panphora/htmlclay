package dataapi

import (
	"strings"
	"testing"
)

func TestWriteStyleVar(t *testing.T) {
	cases := []struct {
		style, name, value, want string
		ok                       bool
	}{
		{"color: red;", "--x", "1", "color: red; --x: 1", true},
		{"", "--x", "1", "--x: 1", true},
		{`--x: url("a;b"); color: red`, "--x", "2", "--x: 2; color: red", true},
		{"--x: 1", "--x", "", "", false},
	}
	for _, c := range cases {
		got, ok := writeStyleVar(c.style, c.name, c.value)
		if got != c.want || ok != c.ok {
			t.Errorf("writeStyleVar(%q, %q, %q) = %q, %v; want %q, %v", c.style, c.name, c.value, got, ok, c.want, c.ok)
		}
	}
}

func TestReadStyleVar(t *testing.T) {
	if got, _ := readStyleVar(`--x: "a/*b*/c"`, "--x"); got != `"a/*b*/c"` {
		t.Errorf("comment inside a string: got %q", got)
	}
	if got, _ := readStyleVar("--x: a !IMPORTANT; --x: b", "--x"); got != "a" {
		t.Errorf("important wins: got %q", got)
	}
}

func TestCheckCustomPropertyValue(t *testing.T) {
	if got := checkCustomPropertyValue(strings.Repeat("😀", 100), writePolicy); got != "" {
		t.Errorf("100 emoji are 200 UTF-16 units and fit: got %q", got)
	}
	if got := checkCustomPropertyValue(strings.Repeat("😀", 101), writePolicy); got != "custom property values are limited to 200 characters" {
		t.Errorf("101 emoji: got %q", got)
	}
	if got := checkCustomPropertyValue("url (x)", writePolicy); got != `"url()" is not allowed in a custom property value` {
		t.Errorf("JS whitespace before the bracket: got %q", got)
	}
	for _, ok := range []string{"myurl(x)", `"Inter", sans-serif`, "#3b82f6"} {
		if got := checkCustomPropertyValue(ok, writePolicy); got != "" {
			t.Errorf("%q should pass: got %q", ok, got)
		}
	}
}
