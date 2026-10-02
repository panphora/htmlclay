package dataapi

import (
	"bytes"

	"golang.org/x/net/html"
)

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
	return writeDocument(src, data, func(d *Document) (Value, error) {
		found, err := d.FindRulesIn(token)
		if err != nil {
			return nil, err
		}
		if found == nil {
			return nil, &NoRulesTag{Token: token}
		}
		return found.Rules, nil
	})
}

// WriteDocumentWithRules applies caller rules without reading a document rules tag.
func WriteDocumentWithRules(src []byte, data Value, rules Value) (*WriteResult, error) {
	switch rules.(type) {
	case string, []Value, *Object:
	default:
		return nil, &RulesParseError{Message: "Invalid extraction rules: expected a selector, array, or object."}
	}
	return writeDocument(src, data, func(_ *Document) (Value, error) { return rules, nil })
}

// writeDocument is the shared write pipeline. resolve turns the parsed document into the rules to
// apply, so the tag face and the caller-rules face run the same plan, policy and splice.
func writeDocument(src []byte, data Value, resolve func(*Document) (Value, error)) (*WriteResult, error) {
	// The BOM is not part of the document and x/net/html would parse it as text, so the whole write
	// runs on the bytes after it and the mark is put back on the output. Spans and the splice edit
	// the same BOM-less body; a no-op write returns the original src, mark included.
	var bom []byte
	body := src
	if bytes.HasPrefix(src, utf8BOM) {
		bom = src[:len(utf8BOM)]
		body = src[len(utf8BOM):]
	}

	d, err := ParseBytes(body)
	if err != nil {
		return nil, err
	}

	rules, err := resolve(d)
	if err != nil {
		return nil, err
	}

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
	spans, owner := sourceSpans(d, body)

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
	result := w.splice(body, []byte(outerHTML(d, d.Root)))
	if len(bom) > 0 {
		result.HTML = append(append([]byte{}, bom...), result.HTML...)
	}
	return result, nil
}
