package dataapi

import (
	"bytes"
	"math"
	"sort"

	"golang.org/x/net/html"
)

// This file ports hyper-html-api/src/engine/splice.js. A write that touched a few elements must
// change only those elements' bytes: a file posted through the data API keeps its author's line
// endings, quoting, entities and void-tag style everywhere else.
//
// The spliced output is proved before it is returned. The result is reparsed and rendered again,
// and only if that render is byte-identical to the whole-document render of the mutated tree is the
// splice trusted. Otherwise the whole-document render is returned with Spliced false, which is
// always correct, just uglier.

// splice replaces the source span of every topmost element the write touched with a fresh
// serialization of that element. full is the whole-document render of the mutated tree, which is
// both the fallback and the proof.
func (w *writer) splice(src, full []byte) *WriteResult {
	fallback := &WriteResult{HTML: full, Changed: !bytes.Equal(full, src), Spliced: false}

	targets := map[*html.Node]bool{}
	for r := range w.dirty {
		if !w.attached(r) {
			continue
		}
		at := w.located(r)
		if at == nil {
			return fallback
		}
		targets[at] = true
	}

	top := make([]*html.Node, 0, len(targets))
	for r := range targets {
		if !w.hasAncestorIn(r, targets) {
			top = append(top, r)
		}
	}
	// Backwards, so an earlier replacement never shifts a later span.
	sort.Slice(top, func(i, j int) bool { return w.spans[top[i]].start > w.spans[top[j]].start })

	out := src
	prevStart := math.MaxInt
	for _, r := range top {
		sp := w.spans[r]
		if sp.end > prevStart {
			return fallback
		}
		buf := make([]byte, 0, len(out)+64)
		buf = append(buf, out[:sp.start]...)
		buf = append(buf, outerHTML(w.d, r)...)
		buf = append(buf, out[sp.end:]...)
		out = buf
		prevStart = sp.start
	}

	check, err := ParseBytes(out)
	if err != nil {
		return fallback
	}
	if outerHTML(check, check.Root) != string(full) {
		return fallback
	}
	return &WriteResult{HTML: out, Changed: !bytes.Equal(out, src), Spliced: true}
}

// attached reports whether r still hangs off the document root. A node inside a detached
// <template> content list is attached for these purposes: the write engine can reach it, and its
// bytes are part of the file.
func (w *writer) attached(r *html.Node) bool {
	n := r
	for {
		p := w.treeParent(n)
		if p == nil {
			break
		}
		n = p
	}
	return n == w.d.Root
}

// located is the nearest node at or above r that has a span, skipping the nodes the write itself
// created. The reference copies a node's source location onto its clones, so it has to skip them by
// hand; here the spans are keyed by the nodes the parse built and a clone never has one, and the
// check is kept so both ports drop exactly the same nodes.
func (w *writer) located(r *html.Node) *html.Node {
	for n := r; n != nil && n.Type != html.DocumentNode; n = w.treeParent(n) {
		if w.cloned[n] {
			continue
		}
		if _, ok := w.spans[n]; ok {
			return n
		}
	}
	return nil
}

func (w *writer) hasAncestorIn(r *html.Node, set map[*html.Node]bool) bool {
	for n := w.treeParent(r); n != nil; n = w.treeParent(n) {
		if set[n] {
			return true
		}
	}
	return false
}

// treeParent is n's parent in the document's own tree. A <template>'s content is detached so that
// no selector can see it, which clears its children's parent links; the template is still their
// real parent, and is where a walk has to continue.
func (w *writer) treeParent(n *html.Node) *html.Node {
	if n.Parent != nil {
		return n.Parent
	}
	return w.owner[n]
}
