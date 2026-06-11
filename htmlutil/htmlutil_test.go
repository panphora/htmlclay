package htmlutil

import (
	"bytes"
	"testing"
)

func TestHasHTMLTag(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"full doc", "<!DOCTYPE html><html><body>x</body></html>", true},
		{"bare html tag", "<html>", true},
		{"fragment", "<p>Hello</p>", false},
		{"only commented html", "<!-- <html> --><p>x</p>", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		if got := HasHTMLTag([]byte(c.in)); got != c.want {
			t.Errorf("%s: HasHTMLTag = %v, want %v", c.name, got, c.want)
		}
	}
}

// --- InjectToken / StripToken tests ---

func TestInjectTokenBareHTML(t *testing.T) {
	in := []byte(`<html>`)
	out := InjectToken(in, "tok123")
	expected := []byte(`<html htmlclaytoken="tok123">`)
	if !bytes.Equal(out, expected) {
		t.Errorf("got %q, want %q", out, expected)
	}
}

func TestInjectTokenWithExistingAttrs(t *testing.T) {
	in := []byte(`<html lang="en">`)
	out := InjectToken(in, "tok123")
	expected := []byte(`<html htmlclaytoken="tok123" lang="en">`)
	if !bytes.Equal(out, expected) {
		t.Errorf("got %q, want %q", out, expected)
	}
}

func TestInjectTokenUppercaseHTML(t *testing.T) {
	in := []byte(`<HTML>`)
	out := InjectToken(in, "tok123")
	expected := []byte(`<HTML htmlclaytoken="tok123">`)
	if !bytes.Equal(out, expected) {
		t.Errorf("got %q, want %q", out, expected)
	}
}

func TestReplaceExistingToken(t *testing.T) {
	in := []byte(`<html htmlclaytoken="old-value" lang="en">`)
	out := InjectToken(in, "new-value")
	expected := []byte(`<html htmlclaytoken="new-value" lang="en">`)
	if !bytes.Equal(out, expected) {
		t.Errorf("got %q, want %q", out, expected)
	}
}

func TestReplaceExistingTokenSingleQuotes(t *testing.T) {
	in := []byte(`<html htmlclaytoken='old-value'>`)
	out := InjectToken(in, "new-value")
	expected := []byte(`<html htmlclaytoken="new-value">`)
	if !bytes.Equal(out, expected) {
		t.Errorf("got %q, want %q", out, expected)
	}
}

func TestStripToken(t *testing.T) {
	in := []byte(`<html htmlclaytoken="tok123" lang="en">`)
	out := StripToken(in)
	expected := []byte(`<html lang="en">`)
	if !bytes.Equal(out, expected) {
		t.Errorf("got %q, want %q", out, expected)
	}
}

func TestStripTokenOnly(t *testing.T) {
	in := []byte(`<html htmlclaytoken="tok123">`)
	out := StripToken(in)
	expected := []byte(`<html>`)
	if !bytes.Equal(out, expected) {
		t.Errorf("got %q, want %q", out, expected)
	}
}

func TestTokenRoundTrip(t *testing.T) {
	original := []byte(`<html lang="en">`)
	injected := InjectToken(original, "tok123")
	stripped := StripToken(injected)
	if !bytes.Equal(stripped, original) {
		t.Errorf("round-trip failed: got %q, want %q", stripped, original)
	}
}

func TestNoHTMLTag(t *testing.T) {
	in := []byte(`<div>hello</div>`)
	out := InjectToken(in, "tok123")
	if !bytes.Equal(out, in) {
		t.Errorf("expected unchanged input, got %q", out)
	}
}

func TestFullDocumentRoundTrip(t *testing.T) {
	original := []byte("<!DOCTYPE html>\n<html lang=\"en\">\n<head><title>Test</title></head>\n<body></body>\n</html>")
	injected := InjectToken(original, "tok123")
	stripped := StripToken(injected)
	if !bytes.Equal(stripped, original) {
		t.Errorf("full doc round-trip failed:\ngot:  %q\nwant: %q", stripped, original)
	}
}

func TestTokenInScriptBodyNotTouched(t *testing.T) {
	in := []byte(`<html><script>var x="htmlclaytoken=test"</script></html>`)
	out := StripToken(in)
	if !bytes.Equal(out, in) {
		t.Errorf("script body token should not be touched, got %q", out)
	}
}

func TestTokenOnlyStrippedFromHTMLTag(t *testing.T) {
	in := []byte(`<html htmlclaytoken="tok"><body data-htmlclaytoken="keep"></body></html>`)
	out := StripToken(in)
	expected := []byte(`<html><body data-htmlclaytoken="keep"></body></html>`)
	if !bytes.Equal(out, expected) {
		t.Errorf("got %q, want %q", out, expected)
	}
}

func TestStripDoesNotTouchTokenElsewhere(t *testing.T) {
	in := []byte(`<html htmlclaytoken="tok"><div htmlclaytoken="user-data">keep</div></html>`)
	out := StripToken(in)
	expected := []byte(`<html><div htmlclaytoken="user-data">keep</div></html>`)
	if !bytes.Equal(out, expected) {
		t.Errorf("got %q, want %q", out, expected)
	}
}

func TestStripPreservesCommentToken(t *testing.T) {
	in := []byte(`<html htmlclaytoken="tok"><!-- htmlclaytoken="fake" --></html>`)
	out := StripToken(in)
	expected := []byte(`<html><!-- htmlclaytoken="fake" --></html>`)
	if !bytes.Equal(out, expected) {
		t.Errorf("got %q, want %q", out, expected)
	}
}

func TestTokenNotFirstAttribute(t *testing.T) {
	in := []byte(`<html lang="en" htmlclaytoken="old">`)
	out := InjectToken(in, "new")
	expected := []byte(`<html htmlclaytoken="new" lang="en">`)
	if !bytes.Equal(out, expected) {
		t.Errorf("got %q, want %q", out, expected)
	}
}

func TestStripTokenNotFirstAttribute(t *testing.T) {
	in := []byte(`<html lang="en" htmlclaytoken="tok123">`)
	out := StripToken(in)
	expected := []byte(`<html lang="en">`)
	if !bytes.Equal(out, expected) {
		t.Errorf("got %q, want %q", out, expected)
	}
}

func TestInjectTokenNoDuplication(t *testing.T) {
	in := []byte(`<html lang="en" htmlclaytoken="old" class="x">`)
	out := InjectToken(in, "new")
	expected := []byte(`<html htmlclaytoken="new" lang="en" class="x">`)
	if !bytes.Equal(out, expected) {
		t.Errorf("got %q, want %q", out, expected)
	}
}

func TestRoundTripNonFirstAttribute(t *testing.T) {
	original := []byte(`<html lang="en" class="x">`)
	injected := InjectToken(original, "tok123")
	stripped := StripToken(injected)
	if !bytes.Equal(stripped, original) {
		t.Errorf("round-trip failed: got %q, want %q", stripped, original)
	}
}

func TestInjectUnquotedToken(t *testing.T) {
	in := []byte(`<html htmlclaytoken=foo lang="en">`)
	out := InjectToken(in, "new")
	expected := []byte(`<html htmlclaytoken="new" lang="en">`)
	if !bytes.Equal(out, expected) {
		t.Errorf("got %q, want %q", out, expected)
	}
}

func TestStripUnquotedToken(t *testing.T) {
	in := []byte(`<html htmlclaytoken=foo lang="en">`)
	out := StripToken(in)
	expected := []byte(`<html lang="en">`)
	if !bytes.Equal(out, expected) {
		t.Errorf("got %q, want %q", out, expected)
	}
}

func TestInjectWithAngleBracketInAttrValue(t *testing.T) {
	in := []byte(`<html data-x='{"a":">"}' htmlclaytoken="old" lang="en">`)
	out := InjectToken(in, "new")
	expected := []byte(`<html htmlclaytoken="new" data-x='{"a":">"}' lang="en">`)
	if !bytes.Equal(out, expected) {
		t.Errorf("got %q, want %q", out, expected)
	}
}

func TestStripWithAngleBracketInAttrValue(t *testing.T) {
	in := []byte(`<html data-x='{"a":">"}' htmlclaytoken="old" lang="en">`)
	out := StripToken(in)
	expected := []byte(`<html data-x='{"a":">"}' lang="en">`)
	if !bytes.Equal(out, expected) {
		t.Errorf("got %q, want %q", out, expected)
	}
}

func TestRoundTripWithAngleBracketInAttrValue(t *testing.T) {
	original := []byte(`<html data-x='{"a":">"}' lang="en">`)
	injected := InjectToken(original, "tok123")
	stripped := StripToken(injected)
	if !bytes.Equal(stripped, original) {
		t.Errorf("round-trip failed: got %q, want %q", stripped, original)
	}
}

// --- HTMLClayID tests ---

func TestReadHTMLClayIDPresent(t *testing.T) {
	in := []byte(`<html htmlclayid="abc-123" lang="en">`)
	id := ReadHTMLClayID(in)
	if id != "abc-123" {
		t.Errorf("got %q, want %q", id, "abc-123")
	}
}

func TestReadHTMLClayIDAbsent(t *testing.T) {
	in := []byte(`<html lang="en">`)
	id := ReadHTMLClayID(in)
	if id != "" {
		t.Errorf("got %q, want empty", id)
	}
}

func TestReadHTMLClayIDNoHTMLTag(t *testing.T) {
	in := []byte(`<div>hello</div>`)
	id := ReadHTMLClayID(in)
	if id != "" {
		t.Errorf("got %q, want empty", id)
	}
}

func TestInjectHTMLClayIDWhenAbsent(t *testing.T) {
	in := []byte(`<html lang="en">`)
	out := InjectHTMLClayID(in, "new-uuid")
	expected := []byte(`<html htmlclayid="new-uuid" lang="en">`)
	if !bytes.Equal(out, expected) {
		t.Errorf("got %q, want %q", out, expected)
	}
}

func TestInjectHTMLClayIDWhenPresent(t *testing.T) {
	in := []byte(`<html htmlclayid="existing-uuid" lang="en">`)
	out := InjectHTMLClayID(in, "new-uuid")
	if !bytes.Equal(out, in) {
		t.Errorf("should not overwrite existing id, got %q", out)
	}
}

func TestInjectHTMLClayIDNoHTMLTag(t *testing.T) {
	in := []byte(`<div>hello</div>`)
	out := InjectHTMLClayID(in, "new-uuid")
	if !bytes.Equal(out, in) {
		t.Errorf("expected unchanged input, got %q", out)
	}
}

func TestHTMLClayIDSurvivesTokenRoundTrip(t *testing.T) {
	original := []byte(`<html htmlclayid="my-uuid" lang="en">`)
	injected := InjectToken(original, "tok123")
	stripped := StripToken(injected)
	if !bytes.Equal(stripped, original) {
		t.Errorf("htmlclayid should survive token round-trip: got %q, want %q", stripped, original)
	}
}

func TestTokenAndHTMLClayIDCoexist(t *testing.T) {
	in := []byte(`<html htmlclayid="my-uuid" lang="en">`)
	out := InjectToken(in, "tok123")
	if !bytes.Contains(out, []byte(`htmlclayid="my-uuid"`)) {
		t.Error("htmlclayid missing after token injection")
	}
	if !bytes.Contains(out, []byte(`htmlclaytoken="tok123"`)) {
		t.Error("htmlclaytoken missing after injection")
	}
}

func TestStripTokenPreservesHTMLClayID(t *testing.T) {
	in := []byte(`<html htmlclaytoken="tok" htmlclayid="my-uuid" lang="en">`)
	out := StripToken(in)
	expected := []byte(`<html htmlclayid="my-uuid" lang="en">`)
	if !bytes.Equal(out, expected) {
		t.Errorf("got %q, want %q", out, expected)
	}
}

func TestGenerateHTMLClayID(t *testing.T) {
	id, err := GenerateHTMLClayID()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(id) != 36 {
		t.Errorf("expected 36-char UUID, got %d chars: %q", len(id), id)
	}
	if id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		t.Errorf("UUID format wrong: %q", id)
	}
	if id[14] != '4' {
		t.Errorf("expected version 4 UUID, got %q at position 14", string(id[14]))
	}
}

func TestGenerateHTMLClayIDUnique(t *testing.T) {
	id1, _ := GenerateHTMLClayID()
	id2, _ := GenerateHTMLClayID()
	if id1 == id2 {
		t.Error("two generated IDs should not be equal")
	}
}

// --- commented-out <html> tag should be skipped ---

func TestInjectTokenSkipsCommentedHTMLTag(t *testing.T) {
	in := []byte(`<!-- <html> --><html lang="en">`)
	out := InjectToken(in, "tok")
	expected := []byte(`<!-- <html> --><html htmlclaytoken="tok" lang="en">`)
	if !bytes.Equal(out, expected) {
		t.Errorf("got %q, want %q", out, expected)
	}
}

func TestStripTokenSkipsCommentedHTMLTag(t *testing.T) {
	in := []byte(`<!-- <html foo> --><html htmlclaytoken="tok" lang="en">`)
	out := StripToken(in)
	expected := []byte(`<!-- <html foo> --><html lang="en">`)
	if !bytes.Equal(out, expected) {
		t.Errorf("got %q, want %q", out, expected)
	}
}

func TestReadHTMLClayIDSkipsCommentedTag(t *testing.T) {
	in := []byte(`<!-- <html htmlclayid="fake"> --><html htmlclayid="real" lang="en">`)
	id := ReadHTMLClayID(in)
	if id != "real" {
		t.Errorf("got %q, want %q", id, "real")
	}
}

func TestInjectTokenCommentRoundTrip(t *testing.T) {
	original := []byte(`<!-- <html> --><html lang="en">`)
	injected := InjectToken(original, "tok123")
	stripped := StripToken(injected)
	if !bytes.Equal(stripped, original) {
		t.Errorf("round-trip failed: got %q, want %q", stripped, original)
	}
}
