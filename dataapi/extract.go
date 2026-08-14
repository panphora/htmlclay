package dataapi

import (
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

// maxRuleDepth is errors.js's MAX_RULE_DEPTH. The check is `depth > max`, so twenty levels of
// nesting are fine and the twenty-first is the one that fails.
const maxRuleDepth = 20

// Extract turns a document and a rule tree into a Value. Ported from extract.js.
//
// The context node for a top-level rule is the document root, and matching runs over the shadow
// tree — template content was detached at parse time, so no selector here can observe it.
func (d *Document) Extract(rules Value) (Value, error) {
	return extractAt(d, d.Root, rules, trace{})
}

// trace carries the depth counter and the path used in the depth error's message.
type trace struct {
	depth int
	path  []string
}

// child returns the trace one level down. It copies the path rather than appending to it: sibling
// branches share a backing array otherwise, and the error message would name whichever branch
// wrote last instead of the one that failed.
func (t trace) child(segment string) trace {
	path := make([]string, len(t.path)+1)
	copy(path, t.path)
	path[len(t.path)] = segment
	return trace{depth: t.depth + 1, path: path}
}

func extractAt(d *Document, ctx *html.Node, rule Value, tr trace) (Value, error) {
	if tr.depth > maxRuleDepth {
		return nil, &MaxRuleDepthExceeded{Path: tr.path}
	}

	switch r := rule.(type) {
	case string:
		return extractScalar(d, ctx, r)

	case []Value:
		// JS destructures [selector, shape]; a shorter array leaves them undefined and any extra
		// elements are ignored. A non-string selector does NOT throw — cheerio's find returns an
		// empty set for it — so the whole rule quietly yields [].
		var selector, shape Value
		if len(r) > 0 {
			selector = r[0]
		}
		if len(r) > 1 {
			shape = r[1]
		}

		var matches []*html.Node
		if s, ok := selector.(string); ok {
			var err error
			if matches, err = Find(ctx, s, FindOpts{}); err != nil {
				return nil, err
			}
		}

		out := make([]Value, 0, len(matches))
		for i, n := range matches {
			v, err := extractAt(d, n, shape, tr.child(strconv.Itoa(i)))
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil

	case *Object:
		result := NewObject()
		for _, key := range r.Keys() {
			sub, _ := r.Get(key)
			v, err := extractAt(d, ctx, sub, tr.child(key))
			if err != nil {
				return nil, err
			}
			// Set, not Define: the JS builds this with `result[key] =`, so a "__proto__" rule key
			// is swallowed by the prototype setter and never reaches the output. Its selector is
			// still resolved on the way, errors included.
			result.Set(key, v)
		}
		return result, nil
	}

	// Numbers, booleans and null are not rules. They extract to null rather than erroring.
	return nil, nil
}

// listRuleAtHint replaces a bare syntax error with the sentence the author actually needs when a
// list rule looks like it reads an attribute. `a@href[]` reads as "the href of every a", but the
// "[]" suffix is stripped first, so `a@href` reaches the selector engine, where "@" is not CSS.
// Neither engine supports the shape: the reference quietly returns [] and this port raises a syntax
// error, so the divergence stays and only the message improves.
//
// An "@" inside a bracket clause is ordinary data — `[data-k="x@y"][]` works in both engines — so
// the check uses the scanner's attribute spans rather than strings.Contains.
func listRuleAtHint(selector string, err error) error {
	if !strings.Contains(selector, "@") {
		return err
	}
	covered := make([]bool, len(selector))
	for _, g := range scanSelector(selector) {
		for _, a := range g.attrs {
			for i := max(a.start, 0); i < a.end && i < len(selector); i++ {
				covered[i] = true
			}
		}
	}
	for i := 0; i < len(selector); i++ {
		if selector[i] == '@' && !covered[i] {
			return &UnsupportedSelector{
				Selector: selector,
				Reason: "a list rule cannot read an attribute: \"sel@name[]\" is not supported by " +
					"either engine. Use the array form, which reads one object per match: " +
					"[\"sel\", {\"name\": \"@name\"}]",
			}
		}
	}
	return err
}

func extractScalar(d *Document, ctx *html.Node, rule string) (Value, error) {
	// "sel[]" — every match's trimmed text, as a list. Empty stays [], never null.
	if strings.HasSuffix(rule, "[]") {
		selector := strings.TrimSuffix(rule, "[]")
		matches, err := Find(ctx, selector, FindOpts{})
		if err != nil {
			return nil, listRuleAtHint(selector, err)
		}
		out := make([]Value, 0, len(matches))
		for _, n := range matches {
			out = append(out, trimmedText(n))
		}
		return out, nil
	}

	// "@name" — a property of the context node itself.
	if strings.HasPrefix(rule, "@") {
		return readPropOrAttr(d, ctx, rule[1:]), nil
	}

	// "sel@name" — split at the last @ outside brackets, parens and quotes, so a selector may
	// carry its own @ (a mailto href, a container query) and still read as one selector.
	if at := ruleAttrIndex(rule); at != -1 {
		selector, name := rule[:at], rule[at+1:]

		matches := []*html.Node{ctx}
		if selector != "" {
			var err error
			if matches, err = Find(ctx, selector, FindOpts{}); err != nil {
				return nil, err
			}
		}
		if len(matches) == 0 {
			return nil, nil
		}
		return readPropOrAttr(d, matches[0], name), nil
	}

	if rule == "." {
		return trimmedText(ctx), nil
	}

	matches, err := Find(ctx, rule, FindOpts{})
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, nil
	}
	return trimmedText(matches[0]), nil
}
