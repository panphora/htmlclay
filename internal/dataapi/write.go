package dataapi

import "golang.org/x/net/html"

// WriteResult is what a write produced. Changed reports whether the body moved anything; Spliced
// reports whether the output is the original bytes with the changed spans replaced rather than a
// whole-document render. A splice is only reported when it could be proved, so a false Spliced with
// a true Changed means the file was written correctly but re-serialized in full.
type WriteResult struct {
	HTML    []byte
	Changed bool
	Spliced bool
}

// WriteDocument applies data to src through the document's own rules tag under the content-only
// policy. It never returns partial output: every error is returned before any bytes are produced.
func WriteDocument(src []byte, data Value, token string) (*WriteResult, error) {
	d, err := ParseBytes(src)
	if err != nil {
		return nil, err
	}

	found, err := d.FindRulesIn(token)
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, &NoRulesTag{Token: token}
	}
	rules := found.Rules

	unknownKeys, unmatched, err := planWrite(d.Root, rules, data)
	if err != nil {
		return nil, err
	}
	if len(unknownKeys) > 0 || len(unmatched) > 0 {
		return nil, &WriteRejected{UnknownKeys: unknownKeys, Unmatched: unmatched}
	}

	var mismatches []Mismatch
	validateShape(rules, data, nil, &mismatches)
	if len(mismatches) > 0 {
		return nil, &ShapeMismatch{Mismatches: mismatches}
	}

	// The spans describe the tree as it was parsed, so they have to be taken before apply runs:
	// a write that clones an element would otherwise find a byte range for the clone, copied from
	// the source it was cloned from. Nothing above this line touched the tree, and a write the
	// plan rejected never gets here at all.
	spans, owner := sourceSpans(d, src)

	w := &writer{
		d:      d,
		dirty:  map[*html.Node]bool{},
		cloned: map[*html.Node]bool{},
		spans:  spans,
		owner:  owner,
	}
	if _, err := w.applyAt(d.Root, rules, data, 0, nil); err != nil {
		return nil, err
	}
	if len(w.refusals) > 0 {
		return nil, &WriteRefused{Refusals: w.refusals}
	}

	if len(w.dirty) == 0 {
		return &WriteResult{HTML: src, Changed: false, Spliced: true}, nil
	}
	return w.splice(src, []byte(outerHTML(d, d.Root))), nil
}
