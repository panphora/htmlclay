package dataapi

import (
	"errors"
	"strings"
	"testing"
)

// regression_test.go pins the bugs found by the phase-4 review
// (~/guys/20260812-144258-htmlclay-dataapi-gate/COMPARISON.md). Every one of them was a SILENT
// wrong answer, and every one was live while the suite, the parity baseline, the conformance corpus
// and a 7.3-million-execution fuzz run were green.
//
// The parity baseline covers most of them now, but a regenerated baseline is only ever as good as
// the generator's list, and these are named so a future reader knows what they are rather than
// seeing an anonymous selector string.

// The two whole-document reads. An empty selector is cheerio's "no matches"; an empty comma GROUP
// is cheerio's Empty sub-selector throw. Neither is the universal selector.
func TestRegressionEmptyGroupIsNotUniversal(t *testing.T) {
	root := doc(t, `<html><head><title>secret</title></head><body><p>a</p><p>b</p></body></html>`)

	for _, selector := range []string{"", "   ", "\t\n"} {
		got, err := Find(root.Root, selector, FindOpts{})
		if err != nil {
			t.Errorf("Find(%q) = %v, want an empty set", selector, err)
		}
		if len(got) != 0 {
			t.Errorf("Find(%q) matched %d nodes, want 0", selector, len(got))
		}
	}

	for _, selector := range []string{"p,", ",p", "p,,p", "p, ", " ,p", ","} {
		if _, err := Find(root.Root, selector, FindOpts{}); err == nil {
			t.Errorf("Find(%q) succeeded, want a refusal", selector)
		}
	}

	// The rule that made this reachable without anything unusual: "[]" strips its own suffix and
	// arrives at Find empty, so an author's empty list dumped every element's text.
	if got := extractJSON(t, `<p>a</p><p>b</p>`, `{v:"[]"}`); got != `{"v":[]}` {
		t.Errorf(`{v:"[]"} = %s, want {"v":[]}`, got)
	}
	if got := extractJSON(t, `<p>a</p><p>b</p>`, `{v:""}`); got != `{"v":null}` {
		t.Errorf(`{v:""} = %s, want {"v":null}`, got)
	}
}

// An attribute clause the scanner cannot read must be a refusal. Trusting cascadia to reject it was
// false — cascadia accepts `!=` — and an unreadable clause ends the scan for the whole comma group,
// so every pseudo after it went unclassified and straight to cascadia.
func TestRegressionUnreadableClauseFailsClosed(t *testing.T) {
	root := doc(t, `<div id="d1" a="q">deep</div>`)

	for _, selector := range []string{
		"div[a!=zz]:matches(^de)", "div[a!=zz]:input", "div[a!=zz]:empty",
		"div[a!=zz]:lang(en)", "div[a!=zz]:enabled", "div[a!=zz]:containsOwn(x)",
		"div[a!=zz]::before", "div[a!=zz]:has(> b)",
	} {
		_, err := Find(root.Root, selector, FindOpts{})
		var fail SelectorFailure
		if !errors.As(err, &fail) {
			t.Errorf("Find(%q) = %v, want a refusal", selector, err)
		}
	}

	// ...while a readable `!=` works and folds case on the 46 names, which is the other half.
	inputs := doc(t, `<input id="ia" type="TEXT"><input id="ib" type="text"><input id="ic" type="radio">`)
	got, err := Find(inputs.Root, "input[type!=text]", FindOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || nodeID(got[0]) != "ic" {
		var ids []string
		for _, n := range got {
			ids = append(ids, nodeID(n))
		}
		t.Errorf("input[type!=text] = %v, want [ic]: type=\"TEXT\" must fold", ids)
	}
}

// CSS comments mean different things to the two parsers, and a comment can hide the bracket or
// quote that blinds the scanner. The check is on raw text, ahead of the scan, for that reason.
func TestRegressionCommentsRefused(t *testing.T) {
	root := doc(t, `<div id="d1"><span id="s1">deep</span></div>`)

	for _, selector := range []string{
		"div/*x*/span", "div /*x*/ span", "div/*[*/:matches(^de)",
		`div/*'*/span`, `div/*"*/span`, `div[id=d1/*]*/]`, "/*x*/div",
	} {
		_, err := Find(root.Root, selector, FindOpts{})
		var unsupported *UnsupportedSelector
		if !errors.As(err, &unsupported) {
			t.Errorf("Find(%q) = %v, want an UnsupportedSelector", selector, err)
		}
	}

	// A "/*" inside a string is not a comment. Refusing these would be a false alarm on selectors
	// cheerio runs happily, which is why the check tokenizes rather than calling strings.Contains.
	for _, selector := range []string{`[id="/*"]`, `[id='/*']`, `span:contains("/*")`, `[id="a/*b"]`} {
		if _, err := Find(root.Root, selector, FindOpts{}); err != nil {
			t.Errorf("Find(%q) should compile: %v", selector, err)
		}
	}
}

// A post-filter can only describe the last compound, so peeling has to stop at a combinator.
// "li:first :contains(x)" applied both filters to "li" and returned the li, not its descendant.
func TestRegressionPeelStaysInOneCompound(t *testing.T) {
	root := doc(t, `<li id="a"><span id="s">x</span></li><li id="b"><span id="t">x</span></li>`)

	for _, selector := range []string{
		"li:first :contains(x)", "li:contains(x) :first", "li:first > span", "li:first span",
	} {
		_, err := Find(root.Root, selector, FindOpts{})
		var unsupported *UnsupportedSelector
		if !errors.As(err, &unsupported) {
			t.Errorf("Find(%q) = %v, want a refusal for a non-terminal positional", selector, err)
		}
	}

	// Adjacent peels in one compound are still fine.
	got, err := Find(root.Root, "li:contains(x):first", FindOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || nodeID(got[0]) != "a" {
		t.Errorf("li:contains(x):first = %v, want [a]", got)
	}
}

// css-what does not trim a pseudo argument, so the whitespace is part of the search string.
func TestRegressionContainsKeepsWhitespace(t *testing.T) {
	const fx = `<p id="a">foo</p><p id="b"> foo </p><p id="c">foo </p><p id="d"> foo</p>`
	for _, c := range []struct {
		selector string
		want     string
	}{
		{"p:contains( foo )", "b"},
		{"p:contains( foo)", "b,d"},
		{"p:contains(foo )", "b,c"},
		{"p:contains(foo)", "a,b,c,d"},
		{`p:contains(" foo ")`, "b"},
	} {
		got, err := Find(doc(t, fx).Root, c.selector, FindOpts{})
		if err != nil {
			t.Fatalf("Find(%q): %v", c.selector, err)
		}
		var ids []string
		for _, n := range got {
			ids = append(ids, nodeID(n))
		}
		if strings.Join(ids, ",") != c.want {
			t.Errorf("Find(%q) = %v, want [%s]", c.selector, ids, c.want)
		}
	}
}

// cascadia folds the DOCUMENT's attribute value with EqualFold and css-select with JS toLowerCase.
// The compile-time guard only ever saw the selector's value, which is the wrong half.
func TestRegressionNonASCIIDocumentValueRefused(t *testing.T) {
	for _, c := range []struct{ html, selector string }{
		{`<div id="d" type="İ"></div>`, "[type^=i]"},
		{`<div id="e" type="ſ"></div>`, "[type=s]"},
		{`<div id="e" type="ſ"></div>`, "[type~=s]"},
		{`<div id="e" type="ſ"></div>`, "[type|=s]"},
		{`<div id="e" data-x="ſ"></div>`, "[data-x=s i]"},
	} {
		_, err := Find(doc(t, c.html).Root, c.selector, FindOpts{})
		var unsupported *UnsupportedSelector
		if !errors.As(err, &unsupported) {
			t.Errorf("Find(%q) on %s = %v, want a refusal", c.selector, c.html, err)
		}
	}

	// A non-ASCII value on an attribute nobody is folding is fine: the comparison is exact.
	if _, err := Find(doc(t, `<div data-x="İ"></div>`).Root, "[data-x=İ]", FindOpts{}); err != nil {
		t.Errorf("case-SENSITIVE matching on a non-ASCII value should work: %v", err)
	}
}

// x/net/html sorts a formatting element's attributes at parse time. Nothing in the renderer can
// undo it, so tree.go recovers the order from the source bytes.
func TestRegressionAttributeOrderIsSourceOrder(t *testing.T) {
	for _, c := range []struct{ html, rule, want string }{
		{`<a id="a1" href="h" rel="r">L</a>`, `{o:"a@outerHTML"}`,
			`{"o":"<a id=\"a1\" href=\"h\" rel=\"r\">L</a>"}`},
		{`<a b="1" a="2" c="3">L</a>`, `{o:"a@outerHTML"}`,
			`{"o":"<a b=\"1\" a=\"2\" c=\"3\">L</a>"}`},
		{`<p><em z="1" a="2">x</em></p>`, `{o:"p@innerHTML"}`,
			`{"o":"<em z=\"1\" a=\"2\">x</em>"}`},
		{`<b y="1" x="2"><i q="1" p="2">d</i></b>`, `{o:"b@outerHTML"}`,
			`{"o":"<b y=\"1\" x=\"2\"><i q=\"1\" p=\"2\">d</i></b>"}`},
		// A repeated attribute: the parser keeps the first and drops the second, and the signature
		// has to be built the same way or the node would never be found.
		{`<a z="1" a="2" z="3">L</a>`, `{o:"a@outerHTML"}`, `{"o":"<a z=\"1\" a=\"2\">L</a>"}`},
		// Non-formatting elements were never sorted and must not be touched.
		{`<div id="d1" href="h" rel="r">L</div>`, `{o:"div@outerHTML"}`,
			`{"o":"<div id=\"d1\" href=\"h\" rel=\"r\">L</div>"}`},
		{`<span z="1" a="2">L</span>`, `{o:"span@outerHTML"}`,
			`{"o":"<span z=\"1\" a=\"2\">L</span>"}`},
		// One attribute cannot be out of order, and is the common case; make sure it is untouched.
		{`<a href="h">L</a>`, `{o:"a@outerHTML"}`, `{"o":"<a href=\"h\">L</a>"}`},
	} {
		if got := extractJSON(t, c.html, c.rule); got != c.want {
			t.Errorf("%s\n got: %s\nwant: %s", c.html, got, c.want)
		}
	}
}

// The adoption agency algorithm can clone one source tag into several tree nodes. Keying the
// recovered order on the SORTED attribute names is what makes every clone restore identically.
func TestRegressionAttributeOrderSurvivesCloning(t *testing.T) {
	// <a> around block content is the classic case: the parser splits it across the <p>s.
	got := extractJSON(t, `<a z="1" a="2"><p>one</p><p>two</p></a>`, `{o:"a@outerHTML"}`)
	if !strings.Contains(got, `z=\"1\" a=\"2\"`) {
		t.Errorf("cloned <a> lost source order: %s", got)
	}
}

// David's call: sel@ with an empty property name returns null everywhere, rather than reproducing
// cheerio's .attr("") returning the whole attribute map. Recorded as a deliberate divergence.
func TestRegressionEmptyPropertyNameIsNull(t *testing.T) {
	for _, c := range []struct{ html, rule, want string }{
		{`<div id="d" class="c" data-y="1">a</div>`, `{a:"div@"}`, `{"a":null}`},
		{`<div>a</div>`, `{a:"div@"}`, `{"a":null}`},
		{`<div id="d" class="c">a</div>`, `{a:"div@class"}`, `{"a":"c"}`},
	} {
		if got := extractJSON(t, c.html, c.rule); got != c.want {
			t.Errorf("%s with %s = %s, want %s", c.html, c.rule, got, c.want)
		}
	}
}

// cheerio-select's index rule is abs(idx) < len, which makes position 0 unreachable from the
// negative side. jQuery's idx += len is off by exactly one there.
func TestRegressionEqNegativeBoundary(t *testing.T) {
	for _, c := range []struct {
		html, selector, want string
	}{
		{`<li id="a">x</li>`, "li:eq(-1)", ""},
		{`<li id="a">x</li>`, "li:eq(0)", "a"},
		{`<li id="a">x</li><li id="b">y</li>`, "li:eq(-1)", "b"},
		{`<li id="a">x</li><li id="b">y</li>`, "li:eq(-2)", ""},
		{`<li id="a">x</li><li id="b">y</li><li id="c">z</li>`, "li:eq(-2)", "b"},
		{`<li id="a">x</li><li id="b">y</li><li id="c">z</li>`, "li:eq(-3)", ""},
		// parseInt semantics, not strconv.Atoi.
		{`<li id="a">x</li><li id="b">y</li>`, "li:eq(1.9)", "b"},
		{`<li id="a">x</li><li id="b">y</li>`, "li:eq(1abc)", "b"},
		{`<li id="a">x</li><li id="b">y</li>`, "li:eq()", ""},
		// :nth is an exact alias.
		{`<li id="a">x</li><li id="b">y</li>`, "li:nth(1)", "b"},
	} {
		got, err := Find(doc(t, c.html).Root, c.selector, FindOpts{})
		if err != nil {
			t.Fatalf("Find(%q): %v", c.selector, err)
		}
		var ids []string
		for _, n := range got {
			ids = append(ids, nodeID(n))
		}
		if strings.Join(ids, ",") != c.want {
			t.Errorf("Find(%q) on %d elements = %v, want [%s]",
				c.selector, strings.Count(c.html, "<li"), ids, c.want)
		}
	}
}

// Found by FuzzScanSelector's closure property rather than by review, which is the point of having
// it. The blank-selector short-circuit used strings.TrimSpace, and Go's whitespace set is not CSS's,
// so the selector "\v" was read as blank and answered "no matches" where cheerio throws.
//
// The fix generalised: the gate now default-denies the plain-compound alphabet, so any character it
// does not model is a refusal rather than a guess. The three runes below are the entire measured
// disagreement between the two engines, found by probing each candidate from JS's whitespace set
// one at a time.
func TestRegressionGoWhitespaceIsNotCSSWhitespace(t *testing.T) {
	root := doc(t, `<div id=root><p id=p1>a</p></div>`)

	// cheerio errors on all three, alone and trailing. It reads them as separators; cascadia reads
	// U+0085 and U+00A0 as identifier characters, so the two engines see a different selector.
	for _, r := range []rune{0x000B, 0x0085, 0x00A0} {
		for _, selector := range []string{string(r), "p" + string(r), "div" + string(r) + "p"} {
			var unsupported *UnsupportedSelector
			if _, err := Find(root.Root, selector, FindOpts{}); !errors.As(err, &unsupported) {
				t.Errorf("Find(%q) = %v, want a refusal for U+%04X", selector, err, r)
			}
		}
	}

	// Everything else from JS's whitespace set is an identifier character to BOTH engines, so
	// refusing it would be a false alarm on selectors cheerio answers.
	for _, r := range []rune{0x1680, 0x180E, 0x2000, 0x200A, 0x200B, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF} {
		got, err := Find(root.Root, "div"+string(r)+"p", FindOpts{})
		if err != nil {
			t.Errorf("Find(div U+%04X p) = %v, want cheerio's empty set", r, err)
		}
		if len(got) != 0 {
			t.Errorf("Find(div U+%04X p) matched %d nodes, want 0", r, len(got))
		}
	}

	// Real CSS whitespace still short-circuits to cheerio's empty set, not to an error.
	for _, selector := range []string{"", " ", "\t\n\r\f"} {
		got, err := Find(root.Root, selector, FindOpts{})
		if err != nil {
			t.Errorf("Find(%q) = %v, want cheerio's empty set", selector, err)
		}
		if len(got) != 0 {
			t.Errorf("Find(%q) matched %d nodes, want 0", selector, len(got))
		}
	}
}

// `a@href[]` is an authoring trap in both engines, not a regression: the "[]" suffix is stripped
// first, so `a@href` reaches the selector engine where "@" is not CSS. The reference returns [] and
// this port errors, which is the loud half of the same trap, so only the message is worth fixing.
func TestListRuleWithAtGetsATargetedMessage(t *testing.T) {
	const fx = `<div id=root><a href="/one" data-k="x@y">1</a><a href="/two">2</a></div>`

	rules, err := ParseRelaxed(`{v:"a@href[]"}`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = doc(t, fx).Extract(rules)
	var unsupported *UnsupportedSelector
	if !errors.As(err, &unsupported) {
		t.Fatalf(`{v:"a@href[]"} = %v, want an UnsupportedSelector`, err)
	}
	if !strings.Contains(unsupported.Reason, `["sel", {"name": "@name"}]`) {
		t.Errorf("the message should name the array form, got: %s", unsupported.Reason)
	}

	// An "@" inside a bracket clause is data, and the rule works in both engines. Refusing it would
	// be a false alarm, which is why the hint reads the scanner's spans instead of the raw text.
	if got := extractJSON(t, fx, `{v:'[data-k="x@y"][]'}`); got != `{"v":["1"]}` {
		t.Errorf(`{v:'[data-k="x@y"][]'} = %s, want {"v":["1"]}`, got)
	}

	// The working spelling, which the message points at.
	if got := extractJSON(t, fx, `{v:["a",{href:"@href"}]}`); got != `{"v":[{"href":"/one"},{"href":"/two"}]}` {
		t.Errorf(`the array form = %s`, got)
	}
}

// The scanner's last silent skip. A byte where an operator belongs that it could not name used to be
// stepped over, leaving `[a%b]` looking like the presence check `[a]` to the gate: the skip lands on
// ']' so the clause even reads as terminated. cascadia rejects all of these today, which is exactly
// the assumption that produced the phase-4 bugs, so the scanner now refuses them itself.
func TestRegressionUnknownAttributeOperatorRefused(t *testing.T) {
	root := doc(t, `<div id=root><a a="b" id=x>A</a></div>`)

	// The assertion has to be the GATE's own refusal (UnsupportedSelector), not just any error.
	// cascadia rejects all of these too, so asserting "some error" passes with the check deleted —
	// measured, by deleting it — and would pin nothing but the status quo.
	for _, selector := range []string{"[a%b]", "[a?b]", "[a@b]", "[a%=b]", "[a$b]"} {
		var unsupported *UnsupportedSelector
		if _, err := Find(root.Root, selector, FindOpts{}); !errors.As(err, &unsupported) {
			t.Errorf("Find(%q) = %v, want the gate's own refusal rather than cascadia's", selector, err)
		}
	}

	// `-` is an identifier character, so `[a-=b]` is the attribute "a-" and both engines answer [].
	got, err := Find(root.Root, "[a-=b]", FindOpts{})
	if err != nil {
		t.Errorf(`Find("[a-=b]") = %v, want cheerio's empty set`, err)
	}
	if len(got) != 0 {
		t.Errorf(`Find("[a-=b]") matched %d nodes, want 0`, len(got))
	}
}
