package dataapi

import (
	"bytes"
	"io"
	"sort"
	"strings"

	"golang.org/x/net/html"
)

// Document is a parsed page with its <template> content detached.
//
// Detaching is the only way to get browser semantics. A skip inside find() is not enough, because
// relational and structural pseudos still OBSERVE the skipped nodes: measured on
// <body><template><b id=inside>x</b></template></body>, cascadia matches body:has(b) and
// body:has(#inside) and does not consider the body empty. Filtering <b> out of the results still
// returns the wrong <body>. Once the children are off the tree there is no mechanism left that can
// see them.
//
// The content is kept here rather than thrown away because @innerHTML and @outerHTML must still
// serialize the template markup, exactly as a browser does. So the accessor matrix falls out:
//
//	find() and every pseudo        never see template content
//	text(), @textContent           exclude it, transitively, because it is off the tree
//	@innerHTML, @outerHTML         keep it, because the renderer consults this map
type Document struct {
	Root *html.Node

	// templateContent maps a <template> element to the children detached from it, in order.
	// Only templates reachable in the live tree are keyed: a template nested inside another
	// template's content is already invisible, and its children stay attached so the renderer
	// reproduces it without a second lookup.
	templateContent map[*html.Node][]*html.Node
}

// Parse reads a document, detaches its template content, and undoes the parser's attribute sort.
//
// The source bytes are buffered because restoreAttrOrder needs a second, independent pass over them.
func Parse(r io.Reader) (*Document, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return ParseBytes(b)
}

// ParseBytes is Parse over a byte slice, which is how the server has the file.
func ParseBytes(b []byte) (*Document, error) {
	root, err := html.Parse(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	d := &Document{Root: root, templateContent: map[*html.Node][]*html.Node{}}
	d.restoreAttrOrder(b)
	d.detachTemplates(root)
	return d, nil
}

// formattingElements are the tags x/net/html routes through addFormattingElement, taken from the
// three cases at parse.go:1001-1012 that call it. Every one of them gets its attributes sorted.
var formattingElements = map[string]bool{
	"a": true, "b": true, "big": true, "code": true, "em": true, "font": true, "i": true,
	"nobr": true, "s": true, "small": true, "strike": true, "strong": true, "tt": true, "u": true,
}

// restoreAttrOrder undoes an optimisation in the parser that is observable through the API.
//
// x/net/html sorts a formatting element's attributes alphabetically at parse time
// (parse.go:382-384, "Sort the attributes to optimize future identical-element searches"), and the
// sorted order survives into the tree. So <a id href rel> serialises as <a href id rel> and every
// @outerHTML containing a link, a <b> or an <em> with two or more attributes breaks byte parity
// with the reference on completely ordinary markup. <div> and <span> are untouched, which is what
// makes it easy to miss.
//
// The order is destroyed before the tree exists, so the renderer cannot recover it; the only source
// left is the bytes. This walks them again with the tokenizer, which does no such sorting.
func (d *Document) restoreAttrOrder(src []byte) {
	order := sourceAttrOrder(src)
	if len(order) == 0 {
		return
	}

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode && len(c.Attr) > 1 && formattingElements[c.Data] {
				if want, ok := order[attrOrderKey(c.Data, attrNames(c))]; ok {
					reorderAttrs(c, want)
				}
			}
			walk(c)
		}
	}
	walk(d.Root)
}

// sourceAttrOrder maps every formatting start tag in the source to its attribute names in the order
// they were written.
//
// The key is the tag name plus its attribute names SORTED, which is what makes this correct under
// the adoption agency algorithm: one source tag can be cloned into several tree nodes, and every
// clone carries the same sorted signature, so each one is restored to the same source order. Two
// distinct source tags sharing an attribute set collide harmlessly, since they would be restored
// identically anyway. First writer wins, matching how the parser resolves a repeated tag.
func sourceAttrOrder(src []byte) map[string][]string {
	out := map[string][]string{}
	z := html.NewTokenizer(bytes.NewReader(src))
	for {
		switch z.Next() {
		case html.ErrorToken:
			return out

		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			tag := string(name)
			if !hasAttr || !formattingElements[tag] {
				continue
			}
			var names []string
			seen := map[string]bool{}
			for {
				k, _, more := z.TagAttr()
				// A repeated attribute is dropped by the parser, so drop it here too or the
				// signature would never match the node.
				if key := string(k); !seen[key] {
					seen[key] = true
					names = append(names, key)
				}
				if !more {
					break
				}
			}
			if len(names) < 2 {
				continue
			}
			if key := attrOrderKey(tag, names); out[key] == nil {
				out[key] = names
			}
		}
	}
}

func attrOrderKey(tag string, names []string) string {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	return tag + "\x00" + strings.Join(sorted, "\x00")
}

func attrNames(n *html.Node) []string {
	names := make([]string, 0, len(n.Attr))
	for _, a := range n.Attr {
		names = append(names, a.Key)
	}
	return names
}

// reorderAttrs rewrites n.Attr into want's order, and does nothing at all unless the two describe
// exactly the same set. Anything ambiguous (a namespaced name the parser rewrote, a duplicate)
// leaves the node alone: the sorted order is wrong, but inventing an order would be worse.
func reorderAttrs(n *html.Node, want []string) {
	if len(want) != len(n.Attr) {
		return
	}
	byName := make(map[string]html.Attribute, len(n.Attr))
	for _, a := range n.Attr {
		if _, dup := byName[a.Key]; dup {
			return
		}
		byName[a.Key] = a
	}
	out := make([]html.Attribute, 0, len(want))
	for _, k := range want {
		a, ok := byName[k]
		if !ok {
			return
		}
		out = append(out, a)
	}
	copy(n.Attr, out)
}

// detachTemplates walks the live tree and empties every <template> it finds. It does NOT descend
// into content it has just detached: anything in there is already unreachable, and leaving it
// attached is what lets the renderer serialize a nested template with one map lookup.
func (d *Document) detachTemplates(n *html.Node) {
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling

		if c.Type == html.ElementNode && c.Data == "template" {
			var kids []*html.Node
			for k := c.FirstChild; k != nil; {
				kn := k.NextSibling
				c.RemoveChild(k)
				// RemoveChild clears the links, which is what makes the node invisible to
				// every traversal. Parent is restored only for rendering, below.
				kids = append(kids, k)
				k = kn
			}
			if len(kids) > 0 {
				d.templateContent[c] = kids
			}
		} else {
			d.detachTemplates(c)
		}

		c = next
	}
}

// templateChildren returns the detached children of a template element, or nil.
func (d *Document) templateChildren(n *html.Node) []*html.Node {
	if d == nil {
		return nil
	}
	return d.templateContent[n]
}
