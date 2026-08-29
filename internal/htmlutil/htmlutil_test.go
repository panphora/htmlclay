package htmlutil

import (
	"bytes"
	"strings"
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
	expected := []byte(`<html savetoken="tok123" htmlclaytoken="tok123">`)
	if !bytes.Equal(out, expected) {
		t.Errorf("got %q, want %q", out, expected)
	}
}

func TestInjectTokenWithExistingAttrs(t *testing.T) {
	in := []byte(`<html lang="en">`)
	out := InjectToken(in, "tok123")
	expected := []byte(`<html savetoken="tok123" htmlclaytoken="tok123" lang="en">`)
	if !bytes.Equal(out, expected) {
		t.Errorf("got %q, want %q", out, expected)
	}
}

func TestInjectTokenUppercaseHTML(t *testing.T) {
	in := []byte(`<HTML>`)
	out := InjectToken(in, "tok123")
	expected := []byte(`<HTML savetoken="tok123" htmlclaytoken="tok123">`)
	if !bytes.Equal(out, expected) {
		t.Errorf("got %q, want %q", out, expected)
	}
}

func TestReplaceExistingToken(t *testing.T) {
	in := []byte(`<html savetoken="old-value" htmlclaytoken="old-value" lang="en">`)
	out := InjectToken(in, "new-value")
	expected := []byte(`<html savetoken="new-value" htmlclaytoken="new-value" lang="en">`)
	if !bytes.Equal(out, expected) {
		t.Errorf("got %q, want %q", out, expected)
	}
}

func TestReplaceExistingTokenSingleQuotes(t *testing.T) {
	in := []byte(`<html savetoken='old-value'>`)
	out := InjectToken(in, "new-value")
	expected := []byte(`<html savetoken="new-value" htmlclaytoken="new-value">`)
	if !bytes.Equal(out, expected) {
		t.Errorf("got %q, want %q", out, expected)
	}
}

func TestStripToken(t *testing.T) {
	in := []byte(`<html savetoken="tok123" htmlclaytoken="tok123" lang="en">`)
	out := StripToken(in)
	expected := []byte(`<html lang="en">`)
	if !bytes.Equal(out, expected) {
		t.Errorf("got %q, want %q", out, expected)
	}
}

func TestStripTokenOnly(t *testing.T) {
	in := []byte(`<html savetoken="tok123" htmlclaytoken="tok123">`)
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
	in := []byte(`<html><script>var x="savetoken=test"</script></html>`)
	out := StripToken(in)
	if !bytes.Equal(out, in) {
		t.Errorf("script body token should not be touched, got %q", out)
	}
}

func TestTokenOnlyStrippedFromHTMLTag(t *testing.T) {
	in := []byte(`<html savetoken="tok" htmlclaytoken="tok"><body data-savetoken="keep"></body></html>`)
	out := StripToken(in)
	expected := []byte(`<html><body data-savetoken="keep"></body></html>`)
	if !bytes.Equal(out, expected) {
		t.Errorf("got %q, want %q", out, expected)
	}
}

func TestStripDoesNotTouchTokenElsewhere(t *testing.T) {
	in := []byte(`<html savetoken="tok" htmlclaytoken="tok"><div savetoken="user-data">keep</div></html>`)
	out := StripToken(in)
	expected := []byte(`<html><div savetoken="user-data">keep</div></html>`)
	if !bytes.Equal(out, expected) {
		t.Errorf("got %q, want %q", out, expected)
	}
}

func TestStripPreservesCommentToken(t *testing.T) {
	in := []byte(`<html savetoken="tok" htmlclaytoken="tok"><!-- savetoken="fake" --></html>`)
	out := StripToken(in)
	expected := []byte(`<html><!-- savetoken="fake" --></html>`)
	if !bytes.Equal(out, expected) {
		t.Errorf("got %q, want %q", out, expected)
	}
}

func TestTokenNotFirstAttribute(t *testing.T) {
	in := []byte(`<html lang="en" savetoken="old" htmlclaytoken="old">`)
	out := InjectToken(in, "new")
	expected := []byte(`<html savetoken="new" htmlclaytoken="new" lang="en">`)
	if !bytes.Equal(out, expected) {
		t.Errorf("got %q, want %q", out, expected)
	}
}

func TestStripTokenNotFirstAttribute(t *testing.T) {
	in := []byte(`<html lang="en" savetoken="tok123" htmlclaytoken="tok123">`)
	out := StripToken(in)
	expected := []byte(`<html lang="en">`)
	if !bytes.Equal(out, expected) {
		t.Errorf("got %q, want %q", out, expected)
	}
}

func TestInjectTokenNoDuplication(t *testing.T) {
	in := []byte(`<html lang="en" savetoken="old" htmlclaytoken="old" class="x">`)
	out := InjectToken(in, "new")
	expected := []byte(`<html savetoken="new" htmlclaytoken="new" lang="en" class="x">`)
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
	in := []byte(`<html savetoken=foo lang="en">`)
	out := InjectToken(in, "new")
	expected := []byte(`<html savetoken="new" htmlclaytoken="new" lang="en">`)
	if !bytes.Equal(out, expected) {
		t.Errorf("got %q, want %q", out, expected)
	}
}

func TestStripUnquotedToken(t *testing.T) {
	in := []byte(`<html savetoken=foo lang="en">`)
	out := StripToken(in)
	expected := []byte(`<html lang="en">`)
	if !bytes.Equal(out, expected) {
		t.Errorf("got %q, want %q", out, expected)
	}
}

func TestInjectWithAngleBracketInAttrValue(t *testing.T) {
	in := []byte(`<html data-x='{"a":">"}' savetoken="old" htmlclaytoken="old" lang="en">`)
	out := InjectToken(in, "new")
	expected := []byte(`<html savetoken="new" htmlclaytoken="new" data-x='{"a":">"}' lang="en">`)
	if !bytes.Equal(out, expected) {
		t.Errorf("got %q, want %q", out, expected)
	}
}

func TestStripWithAngleBracketInAttrValue(t *testing.T) {
	in := []byte(`<html data-x='{"a":">"}' savetoken="old" htmlclaytoken="old" lang="en">`)
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
	in := []byte(`<html documentid="abc-123" lang="en">`)
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
	expected := []byte(`<html documentid="new-uuid" lang="en">`)
	if !bytes.Equal(out, expected) {
		t.Errorf("got %q, want %q", out, expected)
	}
}

func TestInjectHTMLClayIDWhenPresent(t *testing.T) {
	in := []byte(`<html documentid="existing-uuid" lang="en">`)
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
	original := []byte(`<html documentid="my-uuid" lang="en">`)
	injected := InjectToken(original, "tok123")
	stripped := StripToken(injected)
	if !bytes.Equal(stripped, original) {
		t.Errorf("documentid should survive token round-trip: got %q, want %q", stripped, original)
	}
}

func TestTokenAndHTMLClayIDCoexist(t *testing.T) {
	in := []byte(`<html documentid="my-uuid" lang="en">`)
	out := InjectToken(in, "tok123")
	if !bytes.Contains(out, []byte(`documentid="my-uuid"`)) {
		t.Error("documentid missing after token injection")
	}
	if !bytes.Contains(out, []byte(`savetoken="tok123" htmlclaytoken="tok123"`)) {
		t.Error("savetoken missing after injection")
	}
}

func TestStripTokenPreservesHTMLClayID(t *testing.T) {
	in := []byte(`<html savetoken="tok" htmlclaytoken="tok" documentid="my-uuid" lang="en">`)
	out := StripToken(in)
	expected := []byte(`<html documentid="my-uuid" lang="en">`)
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
	expected := []byte(`<!-- <html> --><html savetoken="tok" htmlclaytoken="tok" lang="en">`)
	if !bytes.Equal(out, expected) {
		t.Errorf("got %q, want %q", out, expected)
	}
}

func TestStripTokenSkipsCommentedHTMLTag(t *testing.T) {
	in := []byte(`<!-- <html foo> --><html savetoken="tok" htmlclaytoken="tok" lang="en">`)
	out := StripToken(in)
	expected := []byte(`<!-- <html foo> --><html lang="en">`)
	if !bytes.Equal(out, expected) {
		t.Errorf("got %q, want %q", out, expected)
	}
}

func TestReadHTMLClayIDSkipsCommentedTag(t *testing.T) {
	in := []byte(`<!-- <html documentid="fake"> --><html documentid="real" lang="en">`)
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

// HasHTMLTag is not sufficient for a restore: it accepts `<html><body>partial`,
// which would let a truncated version overwrite a good file.
func TestIsCompleteHTMLDocument(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"complete", "<!DOCTYPE html>\n<html><body>hi</body></html>", true},
		{"complete uppercase", "<HTML><BODY>hi</BODY></HTML>", true},
		{"complete with spacing", "<html><body>hi</body></html   >", true},
		{"complete with attributes", `<html lang="en" documentid="x"><body></body></html>`, true},
		{"truncated mid body", "<html><body>partial", false},
		{"open tag only", "<html>", false},
		{"no html tag", "<div>hi</div>", false},
		{"close before open", "</html><html><body>", false},
		{"empty", "", false},
		{"commented open tag", "<!-- <html> --><body>hi</body></html>", false},
	}
	for _, c := range cases {
		if got := IsCompleteHTMLDocument([]byte(c.in)); got != c.want {
			t.Errorf("%s: IsCompleteHTMLDocument(%q) = %v, want %v", c.name, c.in, got, c.want)
		}
	}
}

// SetHTMLClayID always wins, unlike InjectHTMLClayID which is a no-op when an id
// is already present. Restore uses it to keep the target file's identity.
func TestSetHTMLClayIDReplacesExisting(t *testing.T) {
	in := []byte(`<html documentid="old-one"><body>hi</body></html>`)

	out := SetHTMLClayID(in, "new-one")
	if got := ReadHTMLClayID(out); got != "new-one" {
		t.Fatalf("ReadHTMLClayID = %q, want new-one", got)
	}
	if bytes.Contains(out, []byte("old-one")) {
		t.Fatalf("the old id survived: %q", out)
	}
	if !bytes.Contains(out, []byte("<body>hi</body>")) {
		t.Fatalf("document body was altered: %q", out)
	}

	// InjectHTMLClayID keeps its existing no-op behavior.
	kept := InjectHTMLClayID(in, "new-one")
	if got := ReadHTMLClayID(kept); got != "old-one" {
		t.Fatalf("InjectHTMLClayID overwrote an existing id: %q", got)
	}
}

func TestStripHTMLClayID(t *testing.T) {
	in := []byte(`<html documentid="abc" lang="en"><body>hi</body></html>`)

	out := StripHTMLClayID(in)
	if got := ReadHTMLClayID(out); got != "" {
		t.Fatalf("id survived the strip: %q", got)
	}
	if !bytes.Contains(out, []byte(`lang="en"`)) {
		t.Fatalf("stripping removed an unrelated attribute: %q", out)
	}
	if !bytes.Contains(out, []byte("<body>hi</body>")) {
		t.Fatalf("document body was altered: %q", out)
	}

	// Stripping a document that has no id is a no-op on content.
	plain := []byte(`<html lang="en"><body>hi</body></html>`)
	if got := ReadHTMLClayID(StripHTMLClayID(plain)); got != "" {
		t.Fatalf("stripping a plain document produced an id %q", got)
	}
}

// Finding 8. The completeness check must be as context-aware about the closing
// tag as it is about the opening one. A raw regex over the trailing bytes accepted
// a fake closing tag, so a truncated version passed and replaced a good file.
func TestIsCompleteHTMLDocumentRejectsAFakeClosingTag(t *testing.T) {
	cases := []struct {
		name string
		data string
		want bool
	}{
		{"closing tag only inside a comment", `<html><body><!-- </html> -->`, false},
		{"closing tag only inside a script", `<html><body><script>var s = "</html>";</script>`, false},
		{"closing tag only inside a style", `<html><body><style>/* </html> */</style>`, false},
		{"closing tag only inside a textarea", `<html><body><textarea></html></textarea>`, false},
		{"truncated with no closing tag at all", `<html><body>partial`, false},
		{"real closing tag", `<html><body>ok</body></html>`, true},
		{"real closing tag after a decoy comment", `<html><body><!-- </html> --></body></html>`, true},
		{"real closing tag after a decoy script", `<html><script>"</html>"</script></html>`, true},
		{"real closing tag with whitespace", "<html><body>ok</body></html\n>", true},
		{"uppercase closing tag", `<html><body>ok</BODY></HTML>`, true},
	}
	for _, c := range cases {
		if got := IsCompleteHTMLDocument([]byte(c.data)); got != c.want {
			t.Errorf("%s: IsCompleteHTMLDocument(%q) = %v, want %v", c.name, c.data, got, c.want)
		}
	}
}

// H6. A fake script end tag must not exit raw-text mode. The scan for </name
// requires the HTML tag-name delimiter after the name, so </scripture is a longer
// name and keeps the raw text open. Without the delimiter check the trailing
// textual </html> passed the completeness check and a truncated version replaced a
// good file.
func TestIsCompleteHTMLDocumentRequiresTagNameDelimiter(t *testing.T) {
	cases := []struct {
		name string
		data string
		want bool
	}{
		// The reproduced bypass, made permanent.
		{"scripture is not a script end tag", `<html><script>x</scripture></html>`, false},
		{"scripture then a real script close", `<html><script>x</scripture></script></html>`, true},
		{"scriptx is not a script end tag", `<html><script>x</scriptx></html>`, false},
		// The stricter delimiters that must still be accepted as real closes.
		{"space before the close angle", `<html><script>x</script ></html>`, true},
		{"self-closing style close angle", `<html><script>x</script/></html>`, true},
		{"tab and uppercase close", "<html><script>x</SCRIPT\t></html>", true},
		// A raw-text element that never closes: genuinely incomplete.
		{"eof mid close tag", `<html><script>x</script`, false},
		{"eof inside a longer name", `<html><script>x</scripture`, false},
		// The other raw-text elements share the same delimiter rule.
		{"styleX is not a style end tag", `<html><body><style>a{}</styleX></html>`, false},
		{"style then a real close", `<html><body><style>a{}</styleX></style></html>`, true},
		{"textareaX is not a textarea end tag", `<html><body><textarea>x</textareaX></html>`, false},
		{"textarea then a real close", `<html><body><textarea>x</textareaX></textarea></html>`, true},
		{"titleX is not a title end tag", `<html><head><title>x</titleX></html>`, false},
		{"title then a real close", `<html><head><title>x</titleX></title></html>`, true},
	}
	for _, c := range cases {
		if got := IsCompleteHTMLDocument([]byte(c.data)); got != c.want {
			t.Errorf("%s: IsCompleteHTMLDocument(%q) = %v, want %v", c.name, c.data, got, c.want)
		}
	}
}

func TestInjectBannerBeforeRealCloseTag(t *testing.T) {
	banner := WrapBanner([]byte(`<div id="b">open?</div>`))

	doc := []byte(`<html><body>content</body></html>`)
	out := string(InjectBanner(doc, banner))
	want := `<html><body>content</body>` + string(banner) + `</html>`
	if out != want {
		t.Fatalf("banner misplaced:\n got %q\nwant %q", out, want)
	}

	// A fake close tag inside a comment or a script is text, not a tag; the
	// banner must land before the REAL close.
	tricky := []byte(`<html><body><!-- </html> --><script>var a="</html>"</script>tail</body></html>`)
	outTricky := string(InjectBanner(tricky, banner))
	if !strings.HasSuffix(outTricky, string(banner)+`</html>`) {
		t.Fatalf("banner not before the real close tag: %q", outTricky)
	}
	if strings.Count(outTricky, string(banner)) != 1 {
		t.Fatalf("banner injected more than once: %q", outTricky)
	}

	// No close tag: appended at the end.
	open := []byte(`<html><body>partial`)
	if got := string(InjectBanner(open, banner)); got != string(open)+string(banner) {
		t.Fatalf("bannerless-close doc: %q", got)
	}

	// No <html> tag at all: served unchanged rather than guessed at.
	frag := []byte(`<div>fragment</div>`)
	if got := string(InjectBanner(frag, banner)); got != string(frag) {
		t.Fatalf("fragment must be unchanged: %q", got)
	}
}

func TestStripBannerRemovesEveryInjection(t *testing.T) {
	banner := WrapBanner([]byte(`<div>open?</div>`))
	doc := []byte(`<html><body>content</body></html>`)

	injected := InjectBanner(doc, banner)
	if got := string(StripBanner(injected)); got != string(doc) {
		t.Fatalf("strip(inject(doc)) != doc: %q", got)
	}

	// Multiple banners (a live-sync echo can duplicate one) all go.
	double := InjectBanner(injected, banner)
	if got := string(StripBanner(double)); got != string(doc) {
		t.Fatalf("double banner survived: %q", got)
	}

	// No marker: unchanged.
	if got := string(StripBanner(doc)); got != string(doc) {
		t.Fatalf("bannerless doc changed: %q", got)
	}

	// An unterminated start marker strips nothing rather than guessing.
	unterminated := []byte(`<html><body><!--htmlclay-banner--><div>x</div></body></html>`)
	if got := string(StripBanner(unterminated)); got != string(unterminated) {
		t.Fatalf("unterminated marker mangled the doc: %q", got)
	}
}

// --- legacy attribute spellings -------------------------------------------
//
// htmlclay shipped `htmlclaytoken` and `htmlclayid` before the spec settled on
// `savetoken` and `documentid`. Neither fallback is a migration step that can be
// deleted later: a saved document is a frozen client, so a file written before the
// rename carries the old id on disk forever, and a page served by an older build
// can still come back to a newer one carrying the old token. Each test below pins
// one rule that a documented-but-untested fallback would quietly lose.
//
// The old spellings are assembled from legacyToken and legacyID rather than
// written as literals, because writing them out is what a rename sweep deletes.
// The sweep that introduced these names rewrote an earlier draft of this very
// block into the new spelling, leaving eight tests that asserted nothing about the
// fallback they exist to protect.

const legacyToken = "htmlclay" + "token"
const legacyID = "htmlclay" + "id"

func legacyTokenAttr(value string) string { return ` ` + legacyToken + `="` + value + `"` }
func legacyIDAttr(value string) string    { return ` ` + legacyID + `="` + value + `"` }

func TestReadFallsBackToLegacyDocumentID(t *testing.T) {
	in := []byte(`<html` + legacyIDAttr("from-2024") + ` lang="en"><body>hi</body></html>`)
	if got := ReadHTMLClayID(in); got != "from-2024" {
		t.Errorf("legacy id unreadable: got %q, want %q", got, "from-2024")
	}
}

func TestReadPrefersCurrentDocumentIDSpelling(t *testing.T) {
	in := []byte(`<html` + legacyIDAttr("old") + ` documentid="current" lang="en"></html>`)
	if got := ReadHTMLClayID(in); got != "current" {
		t.Errorf("a document carrying both should answer with the current name: got %q", got)
	}
}

// The strip is the one that must never narrow. A save that only knew the new name
// would write a live credential to disk.
func TestStripTokenRemovesLegacySpelling(t *testing.T) {
	in := []byte(`<html` + legacyTokenAttr("secret") + ` lang="en"><body>hi</body></html>`)
	expected := []byte(`<html lang="en"><body>hi</body></html>`)
	if got := StripToken(in); !bytes.Equal(got, expected) {
		t.Errorf("got %q, want %q", got, expected)
	}
}

func TestStripTokenRemovesBothSpellings(t *testing.T) {
	in := []byte(`<html savetoken="new"` + legacyTokenAttr("old") + ` lang="en"></html>`)
	got := StripToken(in)
	if bytes.Contains(got, []byte("savetoken")) || bytes.Contains(got, []byte(legacyToken)) {
		t.Errorf("a credential survived the strip: %q", got)
	}
}

// Serving writes BOTH spellings, carrying one value, and a stale value under
// either name is cleared first. Two names is what keeps a document that reads the
// old one saving; one value is what keeps them from being two credentials, which
// would matter because a reader takes the first name it recognises.
func TestInjectTokenWritesBothSpellingsWithOneValue(t *testing.T) {
	in := []byte(`<html` + legacyTokenAttr("stale") + ` lang="en"></html>`)
	got := InjectToken(in, "fresh")

	for _, name := range []string{"savetoken", legacyToken} {
		if !bytes.Contains(got, []byte(name+`="fresh"`)) {
			t.Errorf("no %s injected: %q", name, got)
		}
	}
	if bytes.Contains(got, []byte("stale")) {
		t.Errorf("a stale token value survived: %q", got)
	}
	if bytes.Count(got, []byte(`="fresh"`)) != 2 {
		t.Errorf("expected exactly one value under each name: %q", got)
	}
}

// The pair is stripped on save under either name, so it never reaches disk. Both
// halves matter: leaving one behind writes a live credential into the file.
func TestInjectedTokenPairIsFullyStripped(t *testing.T) {
	served := InjectToken([]byte(`<html lang="en"><body>hi</body></html>`), "secret")
	saved := StripToken(served)
	if bytes.Contains(saved, []byte("secret")) {
		t.Errorf("a token value reached the stripped bytes: %q", saved)
	}
	if !bytes.Equal(saved, []byte(`<html lang="en"><body>hi</body></html>`)) {
		t.Errorf("the round trip changed the document: %q", saved)
	}
}

// A file whose version history is filed under its legacy id keeps that id. Stamping
// a second, current-spelling id would strand every version taken before today.
func TestInjectDocumentIDLeavesLegacyIDAlone(t *testing.T) {
	in := []byte(`<html` + legacyIDAttr("from-2024") + ` lang="en"></html>`)
	got := InjectHTMLClayID(in, "brand-new")
	if !bytes.Equal(got, in) {
		t.Errorf("a document with a legacy id was restamped: %q", got)
	}
}

// Restore forces the target file's canonical identity, and that write is where a
// document could end up carrying both spellings at once.
func TestSetDocumentIDReplacesLegacySpelling(t *testing.T) {
	in := []byte(`<html` + legacyIDAttr("donor") + ` lang="en"><body>hi</body></html>`)
	got := SetHTMLClayID(in, "canonical")
	if bytes.Contains(got, []byte(legacyID)) {
		t.Errorf("the legacy id survived alongside the canonical one: %q", got)
	}
	if ReadHTMLClayID(got) != "canonical" {
		t.Errorf("wrong id after set: %q", got)
	}
}

func TestStripDocumentIDRemovesLegacySpelling(t *testing.T) {
	in := []byte(`<html` + legacyIDAttr("donor") + ` lang="en"><body>hi</body></html>`)
	expected := []byte(`<html lang="en"><body>hi</body></html>`)
	if got := StripHTMLClayID(in); !bytes.Equal(got, expected) {
		t.Errorf("got %q, want %q", got, expected)
	}
}

// A legacy-id file served, edited and saved must come back with its id intact and
// its token gone: the exact round trip an existing user's file takes on the first
// run of a build that speaks the new names.
func TestLegacyDocumentRoundTrip(t *testing.T) {
	onDisk := []byte(`<!DOCTYPE html><html` + legacyIDAttr("from-2024") + ` lang="en"><body>hi</body></html>`)
	served := InjectToken(onDisk, "tok")
	if ReadHTMLClayID(served) != "from-2024" {
		t.Fatalf("serving lost the legacy id: %q", served)
	}
	saved := StripToken(served)
	if !bytes.Equal(saved, onDisk) {
		t.Errorf("round trip changed the file: got %q, want %q", saved, onDisk)
	}
}
