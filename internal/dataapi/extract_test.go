package dataapi

import (
	"errors"
	"strings"
	"testing"
)

// extractJSON runs a full rule tree and returns the marshalled result.
func extractJSON(t *testing.T, source string, rules string) string {
	t.Helper()
	parsed, err := ParseRelaxed(rules)
	if err != nil {
		t.Fatalf("ParseRelaxed(%q): %v", rules, err)
	}
	v, err := doc(t, source).Extract(parsed)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return marshal(t, v)
}

// TestArrayRuleShapes pins the destructuring, all of it measured. A short array leaves the shape
// undefined, extra elements are ignored, and a non-string selector does NOT error — cheerio's find
// returns an empty set for it, so the rule quietly yields [].
func TestArrayRuleShapes(t *testing.T) {
	const source = `<div><i class="i">a</i><i class="i">b</i></div>`

	cases := []struct{ name, rules, want string }{
		{"selector and shape", `{v:[".i",{t:"."}]}`, `{"v":[{"t":"a"},{"t":"b"}]}`},
		{"extra elements ignored", `{v:[".i",{t:"."},"ignored"]}`, `{"v":[{"t":"a"},{"t":"b"}]}`},
		{"shape missing", `{v:[".i"]}`, `{"v":[null,null]}`},
		{"empty array", `{v:[]}`, `{"v":[]}`},
		{"non-string selector", `{v:[5,{t:"."}]}`, `{"v":[]}`},
		// Written as strict JSON: in relaxed syntax the "[n" lookahead swallows the whole bracket
		// as an attribute selector, so this shape is unreachable there.
		{"null selector", `{"v":[null,{"t":"."}]}`, `{"v":[]}`},
		{"selector matches nothing", `{v:[".none",{t:"."}]}`, `{"v":[]}`},
		{"shape is a string", `{v:[".i","."]}`, `{"v":["a","b"]}`},
		{"nested array shape", `{v:[".i",[".x",{t:"."}]]}`, `{"v":[[],[]]}`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := extractJSON(t, source, c.rules); got != c.want {
				t.Errorf("%s = %s, want %s", c.rules, got, c.want)
			}
		})
	}
}

// TestPrimitiveRulesExtractToNull: a rule that is not a string, array or object is not an error.
func TestPrimitiveRulesExtractToNull(t *testing.T) {
	got := extractJSON(t, `<p>x</p>`, `{a:1,b:true,c:null,d:{},e:-2.5}`)
	if want := `{"a":null,"b":null,"c":null,"d":{},"e":null}`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// TestProtoKeyOmittedFromOutput is the other half of the __proto__ story. The key survives parsing
// and its selector IS resolved; it vanishes only when the result object is built, because the JS
// assigns with result[key] = and hits the prototype setter.
func TestProtoKeyOmittedFromOutput(t *testing.T) {
	got := extractJSON(t, `<h1>Hello</h1>`, `{"__proto__":{"x":"h1"},"ok":"h1"}`)
	if want := `{"ok":"Hello"}`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}

	// And because it IS resolved on the way, a bad selector under __proto__ still errors even
	// though nothing it produces can reach the output.
	parsed, err := ParseRelaxed(`{"__proto__":"h1:nth-childx(2)","ok":"h1"}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := doc(t, `<h1>x</h1>`).Extract(parsed); err == nil {
		t.Error("expected the discarded __proto__ branch to still raise its selector error")
	}
}

// TestDepthBoundary: the check is `depth > 20`, so twenty levels pass and the twenty-first fails.
// Both numbers measured against the JS.
func TestDepthBoundary(t *testing.T) {
	nest := func(depth int) string {
		return strings.Repeat(`{"k":`, depth) + `"h1"` + strings.Repeat(`}`, depth)
	}

	for _, depth := range []int{1, 19, 20} {
		if _, err := ParseStrict(nest(depth)); err != nil {
			t.Fatal(err)
		}
		parsed, _ := ParseStrict(nest(depth))
		if _, err := doc(t, `<h1>x</h1>`).Extract(parsed); err != nil {
			t.Errorf("depth %d should extract: %v", depth, err)
		}
	}

	parsed, _ := ParseStrict(nest(21))
	_, err := doc(t, `<h1>x</h1>`).Extract(parsed)
	var depthErr *MaxRuleDepthExceeded
	if !errors.As(err, &depthErr) {
		t.Fatalf("depth 21 gave %v, want MaxRuleDepthExceeded", err)
	}
	if want := strings.Repeat("k.", 20) + "k"; strings.Join(depthErr.Path, ".") != want {
		t.Errorf("path = %q, want %q", strings.Join(depthErr.Path, "."), want)
	}
}

// TestDepthPathIsPerBranch would pass by accident if the trace shared one backing array between
// siblings: the reported path would name whichever branch appended last.
func TestDepthPathIsPerBranch(t *testing.T) {
	deep := strings.Repeat(`{"deep":`, 21) + `"h1"` + strings.Repeat(`}`, 21)
	parsed, err := ParseStrict(`{"shallow":"h1","branch":` + deep + `}`)
	if err != nil {
		t.Fatal(err)
	}

	_, err = doc(t, `<h1>x</h1>`).Extract(parsed)
	var depthErr *MaxRuleDepthExceeded
	if !errors.As(err, &depthErr) {
		t.Fatalf("got %v, want MaxRuleDepthExceeded", err)
	}
	if got := depthErr.Path[0]; got != "branch" {
		t.Errorf("path starts at %q, want \"branch\": %v", got, depthErr.Path)
	}
	for _, seg := range depthErr.Path[1:] {
		if seg != "deep" {
			t.Errorf("path contains %q, so branches are sharing a backing array: %v", seg, depthErr.Path)
			break
		}
	}
}

// TestScalarRuleForms walks the extractScalar ladder in the order it tests.
func TestScalarRuleForms(t *testing.T) {
	const source = `<div id="d" class="c">  hi <b>there</b>  </div><i class="i">1</i><i class="i">2</i>`

	cases := []struct{ name, rules, want string }{
		{"list", `{v:".i[]"}`, `{"v":["1","2"]}`},
		{"list with no matches stays a list", `{v:".none[]"}`, `{"v":[]}`},
		{"property of the context node", `{v:"@nodeType"}`, `{"v":"9"}`},
		{"selector then property", `{v:"#d@class"}`, `{"v":"c"}`},
		{"dot is the context node's text", `{v:[".i",{t:"."}]}`, `{"v":[{"t":"1"},{"t":"2"}]}`},
		{"plain selector takes the first match", `{v:".i"}`, `{"v":"1"}`},
		{"no match is null", `{v:"#nope"}`, `{"v":null}`},
		{"property of a missing node is null", `{v:"#nope@id"}`, `{"v":null}`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := extractJSON(t, source, c.rules); got != c.want {
				t.Errorf("%s = %s, want %s", c.rules, got, c.want)
			}
		})
	}
}

// TestBadSelectorIsTyped: every selector failure is one type, so the server can map it to 400
// without reading the message. The JS hosts sniff message text here and answer 400 or 500
// depending on which upstream phrasing they hit; that is the divergence, and it is deliberate.
func TestBadSelectorIsTyped(t *testing.T) {
	// The last one is "sel[]" with an @ in it: the [] suffix is stripped FIRST, leaving "#d@class"
	// as the selector, which is not one.
	for _, rules := range []string{`{v:"h1:nth-childx(2)"}`, `{v:"a["}`, `{v:">>>"}`, `{v:"#d@class[]"}`} {
		parsed, err := ParseRelaxed(rules)
		if err != nil {
			continue // rejected earlier, at parse; also fine
		}
		_, err = doc(t, `<h1>x</h1>`).Extract(parsed)
		// Either selector failure is correct here and both answer 400. Which one fires depends on
		// whether the gate recognises the construct before cascadia sees it, and that boundary is
		// allowed to move as names are added to the allow-list.
		var selErr SelectorFailure
		if !errors.As(err, &selErr) {
			t.Errorf("%s gave %v (%T), want a SelectorFailure", rules, err, err)
		}
	}
}
