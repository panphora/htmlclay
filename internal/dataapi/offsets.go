package dataapi

import (
	"bytes"

	"golang.org/x/net/html"
)

// Source offsets. x/net/html reports no positions, and a splice needs them: a write that touches
// only two elements must replace only those two elements' bytes, so every element it can touch has
// to know the byte range it occupies in the file.
//
// The ranges are recovered by tokenizing the same bytes the tree was parsed from and pairing each
// element with the start tag the parser made it from. parse5, the reference's parser, hands the
// reference the same ranges, and the rules below were read off it. A tag written in the source has
// the exact extent of its start tag and its end tag; an element the parser implied (a <tbody>, say)
// has no location at all; and an element closed implicitly ends where the token that closed it
// begins.
//
// Pairing is all or nothing. The adoption agency algorithm, foster parenting and dropped tags all
// break the correspondence between the tree and the token stream, and then the document gets no
// offsets at all and every write falls back to a whole-document render -- which is what the
// reference does when a node has no location.

// span is a half-open byte range into the source the document was parsed from.
type span struct {
	start int
	end   int
}

// sourceToken is one token of a tokenizer pass with the byte range it occupies. The tokenizer hands
// out raw slices of the input with no positions of their own, so the ranges are measured: every
// token starts where the previous one ended.
type sourceToken struct {
	kind  html.TokenType
	name  string
	attrs map[string]bool
	start int
	end   int
}

// impliedElements are built by the parser with no start tag of their own, so a tree element of one
// of these names with nothing to pair with is ordinary rather than evidence that the tree and the
// token stream have come apart.
var impliedElements = map[string]bool{
	"html": true, "head": true, "body": true, "tbody": true, "tr": true, "colgroup": true,
}

// tokenizeSource walks src once and records every token's kind, tag name and byte range.
func tokenizeSource(src []byte) []sourceToken {
	z := html.NewTokenizer(bytes.NewReader(src))
	var out []sourceToken
	at := 0
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			return out
		}
		t := sourceToken{kind: tt, start: at, end: at + len(z.Raw())}
		at = t.end

		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			t.name = string(name)
			if hasAttr {
				t.attrs = map[string]bool{}
				for {
					key, _, more := z.TagAttr()
					t.attrs[string(key)] = true
					if !more {
						break
					}
				}
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			t.name = string(name)
		}

		out = append(out, t)
	}
}

// pairing lines the elements of a parsed document up with the start tags in its source.
type pairing struct {
	d      *Document
	tokens []sourceToken
	// starts indexes the start-tag and self-closing tokens, which is the stream the walk consumes.
	starts []int
	// next is the cursor into starts.
	next int
	// byToken maps a start-tag token's index in tokens onto the element paired with it.
	byToken map[int]*html.Node
	spans   map[*html.Node]span
	// owner is the parent link a detached <template> content list no longer carries itself.
	owner map[*html.Node]*html.Node
	ok    bool
}

// sourceSpans returns each element's byte range, empty when the tree could not be paired. owner is
// the side map that keeps template content reachable through its template.
func sourceSpans(d *Document, src []byte) (map[*html.Node]span, map[*html.Node]*html.Node) {
	tokens := tokenizeSource(src)
	p := &pairing{
		d:       d,
		tokens:  tokens,
		byToken: map[int]*html.Node{},
		spans:   map[*html.Node]span{},
		owner:   map[*html.Node]*html.Node{},
		ok:      true,
	}
	for i, t := range tokens {
		if t.kind == html.StartTagToken || t.kind == html.SelfClosingTagToken {
			p.starts = append(p.starts, i)
		}
	}

	p.walk(d.Root)
	if p.next != len(p.starts) {
		p.ok = false
	}
	if !p.ok {
		return map[*html.Node]span{}, p.owner
	}
	p.endSpans(len(src))
	return p.spans, p.owner
}

// walk visits the elements in document order, taking each one's start tag off the front of the
// token stream. Template content is visited at its template's position, because that is where its
// tokens sit in the source.
func (p *pairing) walk(n *html.Node) {
	if !p.ok {
		return
	}
	if n.Type == html.ElementNode && !p.match(n) {
		return
	}
	if kids := p.d.templateChildren(n); kids != nil {
		for _, c := range kids {
			p.owner[c] = n
			p.walk(c)
		}
		return
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		p.walk(c)
	}
}

// match consumes the next start tag for n, and reports whether the walk can continue. An implied
// element with no tag of its own is left without a span; any other mismatch means the tree and the
// tokens no longer describe the same document.
func (p *pairing) match(n *html.Node) bool {
	if p.next >= len(p.starts) {
		if impliedElements[n.Data] {
			return true
		}
		p.ok = false
		return false
	}
	t := p.tokens[p.starts[p.next]]
	if t.name != n.Data || !sameAttrSet(n, t.attrs) {
		if impliedElements[n.Data] {
			return true
		}
		p.ok = false
		return false
	}
	p.spans[n] = span{start: t.start}
	p.byToken[p.starts[p.next]] = n
	p.next++
	return true
}

// sameAttrSet reports whether the element carries exactly the attribute names its start tag wrote.
// Only the names are compared, and as a set: the parser drops a repeated name and sorts a
// formatting element's attributes, and neither is visible in the source.
func sameAttrSet(n *html.Node, want map[string]bool) bool {
	if len(n.Attr) != len(want) {
		return false
	}
	for _, a := range n.Attr {
		name := a.Key
		if a.Namespace != "" {
			name = a.Namespace + ":" + a.Key
		}
		if !want[name] {
			return false
		}
	}
	return true
}

// endSpans replays the token stream with a stack of paired elements to find where each one ends.
func (p *pairing) endSpans(srcLen int) {
	var stack []*html.Node

	for i, t := range p.tokens {
		switch t.kind {
		case html.StartTagToken, html.SelfClosingTagToken:
			e := p.byToken[i]
			if e == nil {
				continue
			}
			// Anything the stack still holds that is not an ancestor of this element was
			// closed by this tag: that is where its bytes stop.
			for len(stack) > 0 && !p.ancestor(stack[len(stack)-1], e) {
				top := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				p.close(top, t.start)
			}
			if t.kind == html.SelfClosingTagToken || voidElements[t.name] {
				p.close(e, t.end)
				continue
			}
			stack = append(stack, e)

		case html.EndTagToken:
			at := -1
			for j := len(stack) - 1; j >= 0; j-- {
				if stack[j].Data == t.name {
					at = j
					break
				}
			}
			if at < 0 {
				continue
			}
			for j := at + 1; j < len(stack); j++ {
				p.close(stack[j], t.start)
			}
			p.close(stack[at], t.end)
			stack = stack[:at]
		}
	}

	for _, e := range stack {
		p.close(e, srcLen)
	}
}

func (p *pairing) close(n *html.Node, end int) {
	sp := p.spans[n]
	sp.end = end
	p.spans[n] = sp
}

// ancestor reports whether a is an ancestor of n. Template content hangs off its template through
// the document's side map rather than a parent link.
func (p *pairing) ancestor(a, n *html.Node) bool {
	for cur := p.parentOf(n); cur != nil; cur = p.parentOf(cur) {
		if cur == a {
			return true
		}
	}
	return false
}

func (p *pairing) parentOf(n *html.Node) *html.Node {
	if n.Parent != nil {
		return n.Parent
	}
	return p.owner[n]
}
