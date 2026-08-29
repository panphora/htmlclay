package specwire

import "testing"

// The expected stamps below were produced by running hyperclay's own
// server-lib/spec-wire.js documentEtag over these exact inputs, not derived from
// this implementation. That is what makes them a cross-host agreement test rather
// than a restatement of the code under test: if either host changes how a stamp is
// computed, this fails.
//
// It matters because an etag is a promise BETWEEN hosts. A document that has lived
// on hyperclay carries the stamp it saw there, and if HTML Clay computes a
// different one, every first conditional save is refused with a conflict nobody
// can explain and no retry can clear.
func TestEtagMatchesHyperclay(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "e3b0c44298fc1c14"},
		{"<html></html>", "b633a587c652d023"},
		{`<!DOCTYPE html><html lang="en"><body>hi</body></html>`, "af7bce6cbe4ad9d2"},
		{"a", "ca978112ca1bbdca"},
		{"ünïcödé ✅", "19b726abb86d3027"},
		{"<html>\n\ttabs and newlines\n</html>", "23be88e6982ef0fa"},
	}
	for _, c := range cases {
		if got := Etag([]byte(c.in)); got != c.want {
			t.Errorf("Etag(%q) = %q, want %q (hyperclay's value)", c.in, got, c.want)
		}
	}
}

func TestEtagIsSixteenHexCharacters(t *testing.T) {
	got := Etag([]byte("anything"))
	if len(got) != 16 {
		t.Errorf("stamp length %d, want 16: %q", len(got), got)
	}
	for _, r := range got {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Errorf("stamp is not lowercase hex: %q", got)
			break
		}
	}
}

// An empty document has a stamp like any other. It is NOT the same as having no
// stamp, which is why `*` gets its own rule below rather than being answered by
// comparing against the empty digest.
func TestEmptyBytesStillStamp(t *testing.T) {
	if Etag(nil) != Etag([]byte("")) {
		t.Error("nil and empty must stamp identically")
	}
	if Etag(nil) == "" {
		t.Error("empty bytes must still produce a stamp")
	}
}

// Every accepted and refused spelling below was likewise confirmed against
// hyperclay's ifMatchSatisfied, so the two hosts refuse and accept the same
// requests. RFC 9110 §13.1.1 is what the third-party spellings come from.
func TestIfMatchMatchesHyperclay(t *testing.T) {
	stored := []byte("<html>x</html>")
	tag := Etag(stored)
	if tag != "71c0ca1eca159324" {
		t.Fatalf("fixture stamp drifted: %q", tag)
	}

	cases := []struct {
		field string
		want  bool
	}{
		{"*", true},
		{tag, true},
		{`"` + tag + `"`, true},
		{`W/"` + tag + `"`, true},
		{"w/" + tag, true},
		{"nope, " + tag, true},
		{" , ," + tag, true},
		{"", false},
		{"   ", false},
		{"nope", false},
		{tag + "extra", false},
	}
	for _, c := range cases {
		if got := IfMatchSatisfied(c.field, stored); got != c.want {
			t.Errorf("IfMatchSatisfied(%q) = %v, want %v", c.field, got, c.want)
		}
	}
}

// `*` asks only that the document exist. Answering it true for zero bytes would
// let a save land on a file another writer had just truncated, which is the one
// case the plain stamp comparison cannot express.
func TestStarRequiresBytes(t *testing.T) {
	if IfMatchSatisfied("*", []byte("")) {
		t.Error("* must not match an empty document")
	}
	if IfMatchSatisfied("*", nil) {
		t.Error("* must not match a missing document")
	}
	if !IfMatchSatisfied("*", []byte("x")) {
		t.Error("* must match any document that has bytes")
	}
}

// A client that computed its stamp wrong is refused, never quietly dropped back to
// last-write-wins. A host that advertises `conditional` and then ignores a field it
// could not parse would tell clients they are protected when they are not.
func TestUnparseableFieldRefuses(t *testing.T) {
	stored := []byte("<html>x</html>")
	for _, field := range []string{"", "   ", ",", ", ,", `W/""`, `""`, "W/"} {
		if IfMatchSatisfied(field, stored) {
			t.Errorf("If-Match %q was accepted; it names no stamp we issued", field)
		}
	}
}
