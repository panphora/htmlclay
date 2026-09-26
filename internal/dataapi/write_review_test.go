package dataapi

import (
	"errors"
	"strings"
	"testing"
)

// TestReviewWriteBOM pins the leading-byte-order-mark fix. x/net/html treats U+FEFF as text, which
// drops the DOCTYPE, moves head content into the body, and leaves the tree unable to pair with its
// tokens; both the read path (ParseBytes) and the write path strip it.
func TestReviewWriteBOM(t *testing.T) {
	src := "\xEF\xBB\xBF<!DOCTYPE html><html lang=en><head><meta charset=utf-8>" +
		"<title>T</title><script type=\"application/json\" data-rules-name=\"api\" data-rules-version=\"1\">{t:\"h1\"}</script>" +
		"</head><body>\r\n<h1>Hi</h1>\r\n<p class='x'>&eacute;</p></body></html>"
	data, err := decodeJSON(`{"t":"Yo"}`)
	if err != nil {
		t.Fatal(err)
	}

	res, err := WriteDocument([]byte(src), data, "api")
	if err != nil {
		t.Fatalf("WriteDocument: %v", err)
	}
	if !strings.HasPrefix(string(res.HTML), "\xEF\xBB\xBF") {
		t.Errorf("output lost the BOM: %q", res.HTML)
	}
	if !strings.Contains(string(res.HTML), "<!DOCTYPE html>") {
		t.Errorf("output lost the DOCTYPE: %q", res.HTML)
	}
	if !res.Spliced {
		t.Errorf("Spliced = false, want true")
	}
	want := strings.Replace(src, "<h1>Hi</h1>", "<h1>Yo</h1>", 1)
	if string(res.HTML) != want {
		t.Errorf("bytes:\n got: %q\nwant: %q", res.HTML, want)
	}

	withBOM := doc(t, src)
	withoutBOM := doc(t, strings.TrimPrefix(src, "\xEF\xBB\xBF"))
	withRules, err := withBOM.FindRulesIn("api")
	if err != nil || withRules == nil {
		t.Fatalf("BOM document rules: %v, %v", withRules, err)
	}
	withoutRules, err := withoutBOM.FindRulesIn("api")
	if err != nil || withoutRules == nil {
		t.Fatalf("plain document rules: %v, %v", withoutRules, err)
	}
	got, err := withBOM.Extract(withRules.Rules)
	if err != nil {
		t.Fatalf("extract BOM document: %v", err)
	}
	plain, err := withoutBOM.Extract(withoutRules.Rules)
	if err != nil {
		t.Fatalf("extract plain document: %v", err)
	}
	if marshal(t, got) != marshal(t, plain) {
		t.Errorf("BOM extraction = %s, plain = %s", marshal(t, got), marshal(t, plain))
	}
}

// TestReviewWriteSVGSplice pins the case-insensitive offsets fix. The tokenizer lowercases tag and
// attribute names while the parser restores SVG's mixed case (viewBox, linearGradient), so without
// folding the two the document never pairs and every write re-renders the whole file.
func TestReviewWriteSVGSplice(t *testing.T) {
	src := "<script data-rules-name=\"api\" data-rules-version=\"1\">{h:\"h1\"}</script>\r\n" +
		"<h1>Hi</h1>\r\n" +
		"<svg viewBox='0 0 24 24'><linearGradient id=g></linearGradient><path d='M3 12h18'/></svg>"
	data, err := decodeJSON(`{"h":"Yo"}`)
	if err != nil {
		t.Fatal(err)
	}

	res, err := WriteDocument([]byte(src), data, "api")
	if err != nil {
		t.Fatalf("WriteDocument: %v", err)
	}
	if !res.Spliced {
		t.Errorf("Spliced = false, want true; output: %q", res.HTML)
	}
	restored := strings.Replace(string(res.HTML), "<h1>Yo</h1>", "<h1>Hi</h1>", 1)
	if restored != src {
		t.Errorf("bytes outside <h1> changed:\n got: %q\nwant: %q", restored, src)
	}
}

// TestReviewWriteReadOnlyNoOp pins the read-only no-op. Posting back the value an `@readOnly`
// extraction reports must not error; running the same document and body through the JS engine's
// writeDocument gives {changed: false, spliced: true} with the source unchanged.
func TestReviewWriteReadOnlyNoOp(t *testing.T) {
	src := `<script data-rules-name="api" data-rules-version="1">{r:"#i@readOnly"}</script><input id=i readonly>`
	data, err := decodeJSON(`{"r":false}`)
	if err != nil {
		t.Fatal(err)
	}

	res, err := WriteDocument([]byte(src), data, "api")
	if err != nil {
		t.Fatalf("WriteDocument: %v", err)
	}
	if res.Changed {
		t.Errorf("Changed = true, want false")
	}
	if !res.Spliced {
		t.Errorf("Spliced = false, want true")
	}
	if string(res.HTML) != src {
		t.Errorf("bytes:\n got: %q\nwant: %q", res.HTML, src)
	}
}

// TestReviewEmptyListInsertPath pins the path's types: list indices are numbers and object keys are
// strings, so the marshalled path matches the JS engine's mixed-type path. The JS engine throws the
// same v: ["c",2,"i"] for this document and body.
func TestReviewEmptyListInsertPath(t *testing.T) {
	src := `<script data-rules-name="api" data-rules-version="1">{c:["#c .row",{i:".x[]"}]}</script>` +
		`<div id=c><div class=row><p class=x>a</p></div><div class=row><p class=x>b</p></div><div class=row><p class=y>c</p></div></div>`
	data, err := decodeJSON(`{"c":[{"i":["a"]},{"i":["b"]},{"i":["z"]}]}`)
	if err != nil {
		t.Fatal(err)
	}

	_, err = WriteDocument([]byte(src), data, "api")
	var empty *EmptyListInsert
	if !errors.As(err, &empty) {
		t.Fatalf("WriteDocument error = %v, want EmptyListInsert", err)
	}
	got, err := Marshal(empty.Path)
	if err != nil {
		t.Fatalf("Marshal(path): %v", err)
	}
	if string(got) != `["c",2,"i"]` {
		t.Errorf("path = %s, want [\"c\",2,\"i\"]", got)
	}
	if want := `cannot add items to empty list at "c.2.i" — no sibling to clone as template. Seed the list with a hidden item first.`; empty.Error() != want {
		t.Errorf("message = %q, want %q", empty.Error(), want)
	}
}

// TestReviewSerializerRoundTripsCR guards the one place the serializer leaves dom-serializer: a
// carriage return (reachable from &#13;) is escaped rather than written raw. A raw CR reparses as
// LF, so a whole-document fallback built from it would silently change the document it wrote.
func TestReviewSerializerRoundTripsCR(t *testing.T) {
	d := doc(t, "0&#13;")
	full := outerHTML(d, d.Root)
	if !strings.Contains(full, "&#13;") {
		t.Fatalf("render = %q, want an escaped CR", full)
	}
	reparsed := doc(t, full)
	if again := outerHTML(reparsed, reparsed.Root); again != full {
		t.Errorf("render is not a fixed point:\n got: %q\nwant: %q", again, full)
	}
}
