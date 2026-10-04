package htmlutil

import (
	"bytes"
	"testing"
)

// --- documentetag tests ---

// The stamp is response metadata read back out of the exact bytes that were sent,
// so injecting it and stripping it again has to be a byte-exact identity on any
// document that did not already carry one. If it is not, a tab that saved the
// response it was served would write back bytes the author never had.
func TestDocumentETagRoundTrip(t *testing.T) {
	originals := [][]byte{
		[]byte(`<html>`),
		[]byte(`<html lang="en">`),
		[]byte(`<!DOCTYPE html>` + "\n" + `<html lang="en" class="x">` + "\n" + `<body><p>hi</p></body>` + "\n" + `</html>`),
		[]byte(`<html data-x='{"a":">"}' lang="en">`),
	}
	for _, original := range originals {
		injected := InjectDocumentETag(original, "abc123")
		if !bytes.Contains(injected, []byte(`documentetag="abc123"`)) {
			t.Fatalf("no stamp in %q", injected)
		}
		if stripped := StripDocumentETag(injected); !bytes.Equal(stripped, original) {
			t.Errorf("round-trip failed:\n got: %q\nwant: %q", stripped, original)
		}
		// The token strip carries the stamp too, because every path that writes
		// client bytes to disk already goes through it.
		if stripped := StripToken(injected); !bytes.Equal(stripped, original) {
			t.Errorf("StripToken round-trip failed:\n got: %q\nwant: %q", stripped, original)
		}
	}
}

// A stale, duplicated, oddly spelled or value-less attribute all describe the same
// stamp and must all be replaced by the one computed for this response, with the
// document's own attributes left in order and untouched.
func TestDocumentETagReplacesEverySpelling(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"stale value", `<html documentetag="old" lang="en">`, `<html documentetag="new" lang="en">`},
		{"duplicated", `<html documentetag="a" documentetag="b" lang="en">`, `<html documentetag="new" lang="en">`},
		{"uppercase name", `<html DocumentETag="old" lang="en">`, `<html documentetag="new" lang="en">`},
		{"spaced equals", `<html documentetag = "old" lang="en">`, `<html documentetag="new" lang="en">`},
		{"single quotes", `<html documentetag='old' lang="en">`, `<html documentetag="new" lang="en">`},
		{"unquoted value", `<html documentetag=old lang="en">`, `<html documentetag="new" lang="en">`},
		{"boolean attribute", `<html documentetag lang="en">`, `<html documentetag="new" lang="en">`},
		{"first attribute", `<html documentetag="old">`, `<html documentetag="new">`},
		{"last attribute", `<html lang="en" documentetag="old">`, `<html documentetag="new" lang="en">`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := InjectDocumentETag([]byte(c.in), "new")
			if !bytes.Equal(out, []byte(c.want)) {
				t.Errorf("got %q, want %q", out, c.want)
			}
		})
	}
}

// Stripping must be idempotent-before-write: the bytes that reach disk are the
// document without the stamp, and the untouched ones come back unchanged.
func TestDocumentETagStripRemovesOnlyTheStamp(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`<html documentetag="old" lang="en">`, `<html lang="en">`},
		{`<html documentetag="a" documentetag="b" class="c" lang="en">`, `<html class="c" lang="en">`},
		{`<html DocumentETag="old" lang="en">`, `<html lang="en">`},
		{`<html documentetag = "old" lang="en">`, `<html lang="en">`},
		{`<html documentetag lang="en">`, `<html lang="en">`},
		{`<html lang="en" documentetag="old">`, `<html lang="en">`},
		{`<html lang="en">`, `<html lang="en">`},
		{`<p>no root here</p>`, `<p>no root here</p>`},
	}
	for _, c := range cases {
		if out := StripDocumentETag([]byte(c.in)); !bytes.Equal(out, []byte(c.want)) {
			t.Errorf("StripDocumentETag(%q) = %q, want %q", c.in, out, c.want)
		}
	}
}

// The word can appear inside an attribute value or a comment. Reading the
// attribute run token by token is what keeps those bytes — the author's bytes —
// from being rewritten.
func TestDocumentETagLeavesTheWordInValuesAndComments(t *testing.T) {
	cases := []string{
		`<html data-note="documentetag" lang="en">`,
		`<html data-note='a documentetag="x" b' lang="en">`,
		`<html data-note="a documentetag=b" lang="en">`,
		`<html data-documentetag="x" lang="en">`,
		`<html documentetagnote="x" lang="en">`,
		`<!-- <html documentetag="fake"> --><html lang="en">`,
	}
	for _, in := range cases {
		if out := StripDocumentETag([]byte(in)); !bytes.Equal(out, []byte(in)) {
			t.Errorf("StripDocumentETag rewrote an unrelated byte: %q -> %q", in, out)
		}
	}
	// And the same inputs keep the word when a real stamp is injected alongside.
	for _, in := range cases {
		out := InjectDocumentETag([]byte(in), "new")
		if !bytes.Contains(out, []byte(`documentetag="new"`)) {
			t.Fatalf("no stamp injected into %q: %q", in, out)
		}
		if stripped := StripDocumentETag(out); !bytes.Equal(stripped, []byte(in)) {
			t.Errorf("round-trip failed:\n got: %q\nwant: %q", stripped, in)
		}
	}
}

// A child element's attribute of the same name belongs to the child. Only the
// root tag is scanned, and only the root's stamp is a response stamp.
func TestDocumentETagPreservesAChildAttribute(t *testing.T) {
	in := `<html lang="en"><body documentetag="child-owns-this"><p documentetag="so-does-this">x</p></body></html>`
	want := `<html documentetag="new" lang="en"><body documentetag="child-owns-this"><p documentetag="so-does-this">x</p></body></html>`
	if out := InjectDocumentETag([]byte(in), "new"); !bytes.Equal(out, []byte(want)) {
		t.Errorf("got %q, want %q", out, want)
	}
	if out := StripDocumentETag([]byte(want)); !bytes.Equal(out, []byte(in)) {
		t.Errorf("stripping took the children's attributes too: %q", out)
	}
}

// Byte fidelity: the stamp is added to whatever the file holds, including a
// Latin-1 byte that is not valid UTF-8 and a quoted `>` that the root-tag scan
// has to step over rather than stop at.
func TestDocumentETagPreservesLatin1AndQuotedGreaterThan(t *testing.T) {
	in := []byte("<html data-x='{\"a\":\">\"}' lang=\"en\"><body>caf\xe9</body></html>")
	stamped := InjectDocumentETag(in, "abc123")
	if !bytes.Contains(stamped, []byte(`data-x='{"a":">"}'`)) || !bytes.Contains(stamped, []byte("caf\xe9")) {
		t.Fatalf("stamping rewrote the document: %q", stamped)
	}
	if out := StripDocumentETag(stamped); !bytes.Equal(out, in) {
		t.Errorf("round-trip failed:\n got: %q\nwant: %q", out, in)
	}
}

// A fragment has no root tag, so there is nowhere to put a stamp and nothing to
// strip. Serving it unchanged is the whole contract: guessing at a tag to stamp
// would rewrite a document the host was never asked to edit.
func TestDocumentETagNoRootTagIsUntouched(t *testing.T) {
	for _, in := range []string{`<div>hello</div>`, `<!-- <html documentetag="x"> -->`, ``} {
		if out := InjectDocumentETag([]byte(in), "new"); !bytes.Equal(out, []byte(in)) {
			t.Errorf("InjectDocumentETag(%q) = %q, want unchanged", in, out)
		}
		if out := StripDocumentETag([]byte(in)); !bytes.Equal(out, []byte(in)) {
			t.Errorf("StripDocumentETag(%q) = %q, want unchanged", in, out)
		}
	}
}
