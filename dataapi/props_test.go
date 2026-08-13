package dataapi

import (
	"strings"
	"testing"
)

// fixture is the same document the JS measurements were taken against, so every `want` in this
// file can be compared to a recorded reference value rather than to an opinion.
const fixture = `<!doctype html><html><body>
<input id="in" type="text" value="v" class="c1 c2" title="t" readonly disabled data-empty="" data-k="dv">
<a id="lnk" href="/x" title="at">  link  text  </a>
<div id="dv" class="a b">  outer <b>bold</b> tail  </div>
<select id="sel"><option id="o1" selected>one</option><option id="o2">two</option></select>
<p id="empty"></p>
<div id="tpl-parent"><span cms-template class="seed">SEED</span><span class="seed">REAL</span></div>
<template id="tmpl"><span class="intpl">HIDDEN</span></template>
<script id="rt" data-rules-name="api" data-rules-version="1">{a:h1}</script>
</body></html>`

func doc(t *testing.T, source string) *Document {
	t.Helper()
	d, err := Parse(strings.NewReader(source))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return d
}

// rule runs a single scalar rule against a document and returns it marshalled, so null and "" are
// distinguishable in the assertion.
func rule(t *testing.T, root *Document, r string) string {
	t.Helper()
	v, err := root.Extract(r)
	if err != nil {
		t.Fatalf("Extract(%q): %v", r, err)
	}
	return marshal(t, v)
}

// TestPropsMeasuredAgainstJS is the allowlist read path. Every want was produced by running the
// same rule through the JS engine on the same fixture.
func TestPropsMeasuredAgainstJS(t *testing.T) {
	root := doc(t, fixture)

	cases := []struct{ rule, want string }{
		// Present attributes, read through the property interface.
		{"#in@value", `"v"`},
		{"#in@id", `"in"`},
		{"#in@title", `"t"`},
		{"#in@className", `"c1 c2"`},
		{"#dv@className", `"a b"`},
		{"#lnk@className", `null`},

		// Boolean properties report presence as a STRING, and "false" is a value, not null.
		{"#in@disabled", `"true"`},
		{"#in@checked", `"false"`},
		{"#o1@selected", `"true"`},
		{"#o2@selected", `"false"`},
		{"#dv@disabled", `"false"`},

		{"#in@tagName", `"INPUT"`},
		{"#in@nodeName", `"INPUT"`},
		{"#in@nodeType", `"1"`},
		{"#in@nodeValue", `null`},

		// Allowlisted but unimplementable outside a browser. They are on the list precisely so
		// they do NOT fall through to the attribute path.
		{"#in@classList", `null`},
		{"#in@baseURI", `null`},
		{"#in@dataset", `null`},
		{"#in@offsetWidth", `null`},
		{"#in@childElementCount", `null`},
		{"#in@currentSrc", `null`},
		{"#in@contentType", `null`},

		// An <option> with no value attribute reports its text; an input keeps its own.
		{"#o1@value", `"one"`},
		{"#o2@value", `"two"`},
		{"#sel@value", `null`},

		// The property branch keeps "", the attribute branch turns it into null.
		{"#in@data-empty", `null`},
		{"#in@data-k", `"dv"`},
		{"#in@data-missing", `null`},

		// A boolean attribute read as an ATTRIBUTE returns its own name.
		{"#in@readonly", `"readonly"`},

		// href and src are deliberately NOT on the allowlist, so they read as raw attributes with
		// no URL resolution.
		{"#lnk@href", `"/x"`},
	}

	for _, c := range cases {
		t.Run(c.rule, func(t *testing.T) {
			if got := rule(t, root, c.rule); got != c.want {
				t.Errorf("%s = %s, want %s", c.rule, got, c.want)
			}
		})
	}
}

// TestPropsDeliberateDivergences covers the two warts htmlclay ships FIXED. The corpus cannot
// assert these — it is generated from the JS, so its expectations hold the bug — which is why the
// two cases carry `skip: htmlclay=` and point here.
func TestPropsDeliberateDivergences(t *testing.T) {
	root := doc(t, `<!doctype html><html><body>
<input id="t" type="text" readonly>
<input id="u">
<script id="s">var a = 1;</script>
<style id="y">.a{}</style>
</body></html>`)

	// @type. JS returns the PARSER's node kind — "tag", "script", "style" — never the author's
	// type attribute. Fixed here to mean what it looks like it means.
	for _, c := range []struct{ rule, want, js string }{
		{"#t@type", `"text"`, "tag"},
		{"#s@type", `null`, "script"},
		{"#y@type", `null`, "style"},
	} {
		if got := rule(t, root, c.rule); got != c.want {
			t.Errorf("%s = %s, want %s (JS returns %q)", c.rule, got, c.want, c.js)
		}
	}

	// @readOnly. JS is always "false", because the allowlist spells it in camelCase and the
	// parser lower-cased the attribute, so the two can never meet.
	if got := rule(t, root, "#t@readOnly"); got != `"true"` {
		t.Errorf(`#t@readOnly = %s, want "true" (JS always returns "false")`, got)
	}
	if got := rule(t, root, "#u@readOnly"); got != `"false"` {
		t.Errorf(`#u@readOnly = %s, want "false"`, got)
	}

	// The fix must not leak into the other booleans, which are already lower-case.
	if got := rule(t, root, "#t@disabled"); got != `"false"` {
		t.Errorf(`#t@disabled = %s, want "false"`, got)
	}
}

// TestInputValueDefaultsToOn pins cheerio's third getAttr fallback. The type comparison is
// case-sensitive on the VALUE, which no parser normalises, so type="RADIO" does not qualify.
func TestInputValueDefaultsToOn(t *testing.T) {
	root := doc(t, `<input id="r" type="radio"><input id="c" type="checkbox">`+
		`<input id="v" type="radio" value="x"><input id="u" type="RADIO"><input id="p" type="text">`)

	for _, c := range []struct{ rule, want string }{
		{"#r@value", `"on"`},
		{"#c@value", `"on"`},
		{"#v@value", `"x"`},
		{"#u@value", `null`},
		{"#p@value", `null`},
	} {
		if got := rule(t, root, c.rule); got != c.want {
			t.Errorf("%s = %s, want %s", c.rule, got, c.want)
		}
	}
}

// TestTrimAsymmetry is the one that would rot silently. Selector-shaped rules go through
// adapter.text(), which trims; @textContent does not. A port that trimmed uniformly would look
// correct and quietly change every value carrying deliberate whitespace.
func TestTrimAsymmetry(t *testing.T) {
	root := doc(t, fixture)

	for _, c := range []struct{ rule, want string }{
		{"#lnk", `"link  text"`},
		{"#lnk@textContent", `"  link  text  "`},
		{"#lnk@innerText", `"  link  text  "`},
		{".seed[]", `["REAL"]`},
	} {
		if got := rule(t, root, c.rule); got != c.want {
			t.Errorf("%s = %s, want %s", c.rule, got, c.want)
		}
	}

	// Trimming uses JS's whitespace class, so U+FEFF goes and U+0085 stays.
	ws := doc(t, "<p id=\"a\">\ufeff x \ufeff</p><p id=\"b\">\u0085y\u0085</p>")
	if got := rule(t, ws, "#a"); got != `"x"` {
		t.Errorf("U+FEFF should be trimmed: %s", got)
	}
	if got := rule(t, ws, "#b"); got != "\"\u0085y\u0085\"" {
		t.Errorf("U+0085 should NOT be trimmed: %s", got)
	}
}

// TestSerialization compares Go's html.Render against values recorded from cheerio's serializer.
// These are two independent implementations, so this is the assertion most likely to drift.
func TestSerialization(t *testing.T) {
	root := doc(t, fixture)

	for _, c := range []struct{ rule, want string }{
		{"#dv@innerHTML", `"  outer <b>bold</b> tail  "`},
		{"#empty@outerHTML", `"<p id=\"empty\"></p>"`},
		{"#empty@innerHTML", `""`},
		{"#in@outerHTML", `"<input id=\"in\" type=\"text\" value=\"v\" class=\"c1 c2\" title=\"t\" readonly=\"\" disabled=\"\" data-empty=\"\" data-k=\"dv\">"`},
	} {
		if got := rule(t, root, c.rule); got != c.want {
			t.Errorf("%s\n got %s\nwant %s", c.rule, got, c.want)
		}
	}
}
