package dataapi

import (
	"errors"
	"testing"
)

// TestFindIsDescendantsOnly: cheerio's .find() never matches the context node, and neither does
// this. It matters for array rules, where the context is already a match of the outer selector.
func TestFindIsDescendantsOnly(t *testing.T) {
	root := doc(t, `<div class="x"><div class="x"><p>inner</p></div></div>`)

	outer, err := Find(root.Root, ".x", FindOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(outer) != 2 {
		t.Fatalf("found %d .x from root, want 2", len(outer))
	}

	inner, err := Find(outer[0], ".x", FindOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(inner) != 1 {
		t.Errorf("found %d .x under the first .x, want 1 (self excluded)", len(inner))
	}
}

// TestCMSTemplateSkippedByDefault: seed elements are excluded without anyone asking, and the
// exclusion covers the element itself AND everything under it.
func TestCMSTemplateSkippedByDefault(t *testing.T) {
	root := doc(t, `<ul>
<li class="row" cms-template><span class="cell">SEED</span></li>
<li class="row"><span class="cell">REAL</span></li>
</ul>`)

	for _, c := range []struct {
		selector string
		want     int
	}{
		{".row", 1},  // the seed itself
		{".cell", 1}, // a descendant of the seed
	} {
		got, err := Find(root.Root, c.selector, FindOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != c.want {
			t.Errorf("Find(%q) returned %d, want %d", c.selector, len(got), c.want)
		}
	}

	if got := extractJSON(t, `<ul><li class="row" cms-template>SEED</li><li class="row">REAL</li></ul>`,
		`{v:".row[]"}`); got != `{"v":["REAL"]}` {
		t.Errorf("got %s", got)
	}
}

// TestRulesTagsAreFilteredOut: a page's own endpoint definition is not data. Only the rules-tag
// lookup opts back in, and it must, or it would filter out the tag it is looking for.
func TestRulesTagsAreFilteredOut(t *testing.T) {
	root := doc(t, `<script id="a" data-rules-name="api">{}</script>`+
		`<script id="b">var x = 1;</script>`+
		`<script id="c" data-rules-name="">{}</script>`)

	visible, err := Find(root.Root, "script", FindOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) != 1 {
		t.Errorf("found %d scripts, want 1 (both data-rules-name scripts hidden)", len(visible))
	} else if id, _ := attrValue(visible[0], "id"); id != "b" {
		t.Errorf("visible script is %q, want \"b\"", id)
	}

	all, err := Find(root.Root, "script", FindOpts{IncludeRulesTag: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("with IncludeRulesTag found %d, want 3", len(all))
	}
}

// TestTemplateContentIsInvisible: three engines, three answers, and htmlclay picks the browser's.
// A browser matches nothing inside a <template> because its content is a separate
// DocumentFragment; cheerio matches it with a simple selector but not through a combinator, so it
// disagrees with itself; cascadia over a raw x/net/html tree matches it both ways.
func TestTemplateContentIsInvisible(t *testing.T) {
	root := doc(t, `<ul><li class="i">one</li></ul><template><li class="i">tpl</li></template>`)

	for _, selector := range []string{".i", "template .i", "template > .i", "li"} {
		got, err := Find(root.Root, selector, FindOpts{})
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		if selector == ".i" || selector == "li" {
			want = 1 // the one outside the template
		}
		if len(got) != want {
			t.Errorf("Find(%q) = %d, want %d", selector, len(got), want)
		}
	}
}

// TestTemplateContentIsInvisibleToStructuralPseudos is the reason detaching beats skipping. A skip
// inside find() filters the RESULTS, but relational and structural pseudos still observe the
// skipped nodes, so the wrong ancestor comes back and nothing downstream can tell.
func TestTemplateContentIsInvisibleToStructuralPseudos(t *testing.T) {
	root := doc(t, `<body><template><b id="inside">x</b></template></body>`)

	for _, selector := range []string{"body:has(b)", "body:has(#inside)", "b", "#inside"} {
		got, err := Find(root.Root, selector, FindOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("Find(%q) = %d, want 0", selector, len(got))
		}
	}

	// The template really is childless now, which a results-filter could never achieve: a browser
	// agrees, since template content lives in a DocumentFragment rather than in childNodes. The
	// probe is "template > *" rather than ":empty" because the gate rejects :empty outright — the
	// two engines disagree about whitespace-only text. (body is NOT empty either way; it still
	// holds the <template> element, only its children moved.)
	got, err := Find(root.Root, "template > *", FindOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("template > * = %d, want 0", len(got))
	}
}

// TestTemplateMarkupStillSerializes: the content is off the tree for matching but must still come
// back from @innerHTML and @outerHTML, because that is what a browser does.
func TestTemplateMarkupStillSerializes(t *testing.T) {
	root := doc(t, `<div id="w"><template><li class="i">tpl</li></template></div>`)

	if got := rule(t, root, "#w@innerHTML"); got != `"<template><li class=\"i\">tpl</li></template>"` {
		t.Errorf("innerHTML = %s", got)
	}
	// ...while text does NOT include it, transitively, because it is not on the tree.
	if got := rule(t, root, "#w@textContent"); got != `""` {
		t.Errorf("textContent = %s, want \"\"", got)
	}
	if got := rule(t, root, "#w"); got != `""` {
		t.Errorf("trimmed text = %s", got)
	}
}

// A template nested inside another template's content is already unreachable, so only the outer one
// is detached. Serialization has to reproduce both.
func TestNestedTemplateSerializes(t *testing.T) {
	root := doc(t, `<div id="w"><template><p>a</p><template><b>deep</b></template></template></div>`)

	want := `"<template><p>a</p><template><b>deep</b></template></template>"`
	if got := rule(t, root, "#w@innerHTML"); got != want {
		t.Errorf("innerHTML\n got %s\nwant %s", got, want)
	}
	for _, selector := range []string{"p", "b", "template template"} {
		got, err := Find(root.Root, selector, FindOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("Find(%q) = %d, want 0", selector, len(got))
		}
	}
}

func TestFindRulesIn(t *testing.T) {
	const page = `<!doctype html><html><body>
<script data-rules-name="other" data-rules-version="1">{a:"h1"}</script>
<script data-rules-name="api extra" data-rules-version="1">{title:"h1"}</script>
<script data-rules-name="api" data-rules-version="1">{second:"h1"}</script>
</body></html>`

	t.Run("space separated names and first wins", func(t *testing.T) {
		found, err := doc(t, page).FindRulesIn("api")
		if err != nil {
			t.Fatal(err)
		}
		if found == nil {
			t.Fatal("no rules tag found")
		}
		if got := marshal(t, found.Rules); got != `{"title":"h1"}` {
			t.Errorf("got %s, want the FIRST matching tag", got)
		}
	})

	t.Run("missing tag is not an error", func(t *testing.T) {
		found, err := doc(t, page).FindRulesIn("nosuch")
		if err != nil {
			t.Fatalf("missing tag should not error: %v", err)
		}
		if found != nil {
			t.Error("expected nil for a page with no matching tag")
		}
	})

	t.Run("wrong version", func(t *testing.T) {
		_, err := doc(t, `<script data-rules-name="api" data-rules-version="2">{}</script>`).FindRulesIn("api")
		var ver *UnknownRulesVersion
		if !errors.As(err, &ver) {
			t.Fatalf("got %v, want UnknownRulesVersion", err)
		}
		if ver.Version != "2" {
			t.Errorf("version = %q, want \"2\"", ver.Version)
		}
	})

	t.Run("missing version", func(t *testing.T) {
		_, err := doc(t, `<script data-rules-name="api">{}</script>`).FindRulesIn("api")
		var ver *UnknownRulesVersion
		if !errors.As(err, &ver) {
			t.Fatalf("got %v, want UnknownRulesVersion", err)
		}
	})

	// The token is validated before it is interpolated, which is what forecloses injection
	// through the ~= match rather than any escaping.
	t.Run("token is rejected not sanitised", func(t *testing.T) {
		for _, token := range []string{`api"]`, "", "a b", `a"`, "*", "api]"} {
			_, err := doc(t, page).FindRulesIn(token)
			var bad *InvalidRulesToken
			if !errors.As(err, &bad) {
				t.Errorf("token %q gave %v, want InvalidRulesToken", token, err)
			}
		}
		for _, token := range []string{"api", "a-b", "a_b", "A1"} {
			if _, err := doc(t, page).FindRulesIn(token); err != nil {
				var bad *InvalidRulesToken
				if errors.As(err, &bad) {
					t.Errorf("token %q should be valid", token)
				}
			}
		}
	})

	// The body is trimmed before parsing, because the JS reads it through adapter.text().
	t.Run("relaxed body is trimmed then parsed", func(t *testing.T) {
		found, err := doc(t, "<script data-rules-name=\"api\" data-rules-version=\"1\">\n  {title: h1,}\n</script>").FindRulesIn("api")
		if err != nil {
			t.Fatal(err)
		}
		if got := marshal(t, found.Rules); got != `{"title":"h1"}` {
			t.Errorf("got %s", got)
		}
	})
}
