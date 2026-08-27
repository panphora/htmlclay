package dataapi

import (
	"golang.org/x/net/html"
)

// cmsTemplateAttr marks a CMS seed element: a node kept in the document purely as something to
// clone. The JS adapter skips these BY DEFAULT rather than on opt-in, because a seed is never data
// for any consumer. Only the CMS's own grow-from-zero lookup passes templateAttr:null to see them,
// and that is a write path, which htmlclay does not have. So this is always on here.
const cmsTemplateAttr = "cms-template"

// FindOpts is the read-path subset of the JS adapter's find options. The adapter also takes `skip`
// and `templateAttr`, both of which exist only for the CMS write path; leaving them out keeps the
// one caller that matters honest rather than carrying dead configuration.
type FindOpts struct {
	// IncludeRulesTag keeps script[data-rules-name] elements in the result. Only the rules-tag
	// lookup sets it, and it MUST: isRulesTag matches any such script, so without the flag the
	// lookup would filter out the very tag it selects.
	IncludeRulesTag bool
}

// Find returns the descendants of ctx matching selector, in document order. Self is never
// included, matching cheerio's .find().
func Find(ctx *html.Node, selector string, opts FindOpts) ([]*html.Node, error) {
	if ctx == nil {
		return nil, nil
	}

	sel, err := compileSelector(selector)
	if err != nil {
		return nil, err
	}

	// The adapter's own exclusions run AFTER the positional filters, not before, because that is the
	// order cheerio applies them in: `.row:first` means the first .row in the document, and if that
	// one happens to be a [cms-template] seed the answer is nothing, not "the first non-seed row".
	matched, err := sel.match(ctx)
	if err != nil {
		return nil, err
	}

	var out []*html.Node
	for _, n := range matched {
		if !opts.IncludeRulesTag && isRulesTag(n) {
			continue
		}
		if hasSelfOrAncestorAttr(n, cmsTemplateAttr) {
			continue
		}
		out = append(out, n)
	}
	return out, nil
}

// isRulesTag reports whether n is a rules script. It matches ANY script carrying
// data-rules-name, present-but-empty included, which is why the attribute is tested for presence
// rather than for a value.
func isRulesTag(n *html.Node) bool {
	if n == nil || n.Type != html.ElementNode || n.Data != "script" {
		return false
	}
	_, ok := attrValue(n, "data-rules-name")
	return ok
}

// hasSelfOrAncestorAttr is cheerio's closest('[attr]').length !== 0: the node itself counts, not
// just its ancestors.
func hasSelfOrAncestorAttr(n *html.Node, attr string) bool {
	for cur := n; cur != nil; cur = cur.Parent {
		if cur.Type != html.ElementNode {
			continue
		}
		if _, ok := attrValue(cur, attr); ok {
			return true
		}
	}
	return false
}

// attrValue reads a raw attribute. The lookup is case-SENSITIVE against the parsed name, which is
// correct because both html parsers lower-case attribute names as they parse: a rule asking for
// "@readOnly" genuinely does not find a `readonly` attribute, and that is the bug behind one of
// the two warts this port fixes. See props.go.
func attrValue(n *html.Node, name string) (string, bool) {
	if n == nil {
		return "", false
	}
	for _, a := range n.Attr {
		if a.Key == name {
			return a.Val, true
		}
	}
	return "", false
}
