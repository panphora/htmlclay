package dataapi

import (
	"sort"
	"strconv"

	"golang.org/x/net/html"
)

// This file ports hyper-html-api/src/engine/diff.js and the adapter primitives listDiff calls. The
// list forms are `"sel[]"` (a scalar list, items are strings) and `["sel", {shape}]` (an object
// list). Both reconcile the incoming array against the rows the page already has, cloning the
// template row only when the list grows.

// listDiff is the reconciliation itself. ctx is the node the rule was written against, so the rows
// are its descendants; shape is null for a scalar list, the undefined sentinel for the `["sel"]`
// form (which the reference walks as a rule-less list), and the rule shape otherwise.
func (w *writer) listDiff(ctx *html.Node, selector, shape Value, newItems []Value, depth int, path []string) error {
	oldNodes, err := findListRows(ctx, selector)
	if err != nil {
		return err
	}

	if len(newItems) == 0 {
		for _, n := range oldNodes {
			w.remove(n)
		}
		return nil
	}

	needsTemplate := len(newItems) > len(oldNodes)
	var templateSource *html.Node
	if len(oldNodes) > 0 {
		templateSource = oldNodes[0]
	}
	if needsTemplate && templateSource == nil {
		templateSource, err = w.findFallbackTemplate(ctx, selector)
		if err != nil {
			return err
		}
		if templateSource == nil {
			return &EmptyListInsert{Path: path}
		}
	}

	oldValues := make([]Value, len(oldNodes))
	for i, n := range oldNodes {
		v, err := w.extractItem(n, shape)
		if err != nil {
			return err
		}
		oldValues[i] = v
	}

	// Only a list that grew can need a clone, because matchRows pairs every row it can.
	var template *html.Node
	if needsTemplate && templateSource != nil {
		template = w.cloneNode(templateSource)
		removeAttr(template, cmsTemplateAttr)
		w.dirty[template] = true
		w.stripIds(template)
	}

	matches := matchRows(newItems, oldValues, shape, nil)

	referenceNode := templateSource
	if len(oldNodes) > 0 {
		referenceNode = oldNodes[0]
	}
	parent := referenceNode.Parent
	anchorIdx := 0
	if len(oldNodes) > 0 {
		anchorIdx = indexInParent(parent, referenceNode)
	}
	contiguous := isContiguousRun(oldNodes)

	used := map[int]bool{}
	fresh := make([]bool, 0, len(newItems))
	finalNodes := make([]*html.Node, len(newItems))
	for i := range newItems {
		if oldIdx := matches[i]; oldIdx >= 0 {
			used[oldIdx] = true
			fresh = append(fresh, false)
			finalNodes[i] = oldNodes[oldIdx]
			continue
		}
		fresh = append(fresh, true)
		cloned := w.cloneNode(template)
		w.stripIds(cloned)
		finalNodes[i] = cloned
	}

	// Remove only unmatched old nodes. Matched nodes stay attached so DOM identity (focus,
	// observers, animations) survives a reorder.
	for i, n := range oldNodes {
		if !used[i] {
			w.remove(n)
		}
	}

	if contiguous {
		// Place each final node at its target index. A node already there is left alone, and
		// insertAt moves an attached node, which both engines treat as "move to here".
		for i, node := range finalNodes {
			targetIdx := anchorIdx + i
			if indexInParent(parent, node) == targetIdx {
				continue
			}
			w.insertAt(parent, node, targetIdx)
		}
	} else {
		// The matched nodes are not a contiguous run of siblings, so the list has no DOM
		// order to restore and only the grown rows are placed.
		w.placeGrownItems(finalNodes, fresh, parent, anchorIdx)
	}

	return w.writeItems(finalNodes, shape, newItems, depth, path)
}

// findListRows is adapter.find(parentCtx, selector, opts): a non-string selector matches nothing,
// which is what cheerio answers for one.
func findListRows(ctx *html.Node, selector Value) ([]*html.Node, error) {
	s, ok := selector.(string)
	if !ok {
		return nil, nil
	}
	return Find(ctx, s, FindOpts{})
}

// findFallbackTemplate looks for a template-marked node matching the selector, walking up from
// parentCtx so a template defined once in an ancestor is found even when the immediate container
// has none. IncludeCMSTemplates is the reference's templateAttr:null: the fallback lookup MUST see
// the seed element to clone it for grow-from-zero.
func (w *writer) findFallbackTemplate(parentCtx *html.Node, selector Value) (*html.Node, error) {
	s, ok := selector.(string)
	if !ok {
		return nil, nil
	}
	scope := parentCtx
	for scope != nil {
		candidates, err := Find(scope, s, FindOpts{IncludeCMSTemplates: true})
		if err != nil {
			return nil, err
		}
		for _, n := range candidates {
			if _, ok := attrValue(n, cmsTemplateAttr); ok {
				return n, nil
			}
		}
		scope = scope.Parent
	}
	return nil, nil
}

// extractItem reads one row back for matching. A scalar list reads the trimmed text; anything else
// goes through the read engine with the row as context.
func (w *writer) extractItem(node *html.Node, shape Value) (Value, error) {
	if shape == nil {
		return trimmedText(node), nil
	}
	return extractAt(w.d, node, shape, trace{})
}

// writeItems applies the per-item value. A scalar list writes text, so the policy applies; an
// object list recurses through the same rule walk.
func (w *writer) writeItems(finalNodes []*html.Node, shape Value, newItems []Value, depth int, path []string) error {
	for i, node := range finalNodes {
		if shape == nil {
			w.setText(node, newItems[i])
			continue
		}
		// applyAt may replace the node (@outerHTML on the item itself); capture the return so
		// finalNodes stays current for later passes.
		newNode, err := w.applyAt(node, shape, newItems[i], depth+1, appendString(path, strconv.Itoa(i)))
		if err != nil {
			return err
		}
		if newNode != nil && newNode != node {
			finalNodes[i] = newNode
		}
	}
	return nil
}

// placeGrownItems puts each grown row after the surviving node before it. With none before it, the
// row falls back to where the list started.
func (w *writer) placeGrownItems(finalNodes []*html.Node, fresh []bool, fallbackParent *html.Node, fallbackIdx int) {
	var anchor *html.Node
	nextFallbackIdx := fallbackIdx
	for i := 0; i < len(finalNodes); i++ {
		if !fresh[i] {
			anchor = finalNodes[i]
			continue
		}
		parent := fallbackParent
		if anchor != nil {
			parent = anchor.Parent
		}
		if parent == nil {
			continue
		}
		at := nextFallbackIdx
		if anchor != nil {
			at = indexInParent(parent, anchor) + 1
		} else {
			nextFallbackIdx++
		}
		w.insertAt(parent, finalNodes[i], at)
		anchor = finalNodes[i]
	}
}

// isContiguousRun reports whether the rows occupy a contiguous run of element siblings under one
// parent. Two containers, or an unowned sibling between two rows, has no list order to restore.
func isContiguousRun(nodes []*html.Node) bool {
	if len(nodes) <= 1 {
		return true
	}
	parent := nodes[0].Parent
	if parent == nil {
		return false
	}
	siblings := adapterChildren(parent)
	indices := make([]int, 0, len(nodes))
	for _, n := range nodes {
		i := indexOfNode(siblings, n)
		if i == -1 {
			return false
		}
		indices = append(indices, i)
	}
	sort.Ints(indices)
	return indices[len(indices)-1]-indices[0] == len(indices)-1
}

// indexInParent is adapter.children(parent).findIndex(sameNode).
func indexInParent(parent, target *html.Node) int {
	return indexOfNode(adapterChildren(parent), target)
}

// adapterChildren is cheerioAdapter.children: element children only, and when any of them is
// no-data the no-data ones are left out. It is not recursive — a no-data descendant of a child
// does not change the answer.
func adapterChildren(n *html.Node) []*html.Node {
	if n == nil {
		return nil
	}
	all := elementChildren(n)
	for _, c := range all {
		if isNoData(c) {
			out := make([]*html.Node, 0, len(all))
			for _, k := range all {
				if !isNoData(k) {
					out = append(out, k)
				}
			}
			return out
		}
	}
	return all
}

// elementChildren is cheerio's children(): element nodes only, text and comments left out, no
// no-data filtering.
func elementChildren(n *html.Node) []*html.Node {
	var out []*html.Node
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode {
			out = append(out, c)
		}
	}
	return out
}

// insertAt is cheerioAdapter.insertAt: the index counts element children, and with a no-data child
// anywhere in the list the no-data ones are stepped over instead of counted. A node that is
// already attached is moved.
func (w *writer) insertAt(parent, node *html.Node, index int) {
	w.dirty[parent] = true
	if node.Parent != nil {
		w.dirty[node.Parent] = true
	}

	all := elementChildren(parent)
	noData := false
	for _, c := range all {
		if isNoData(c) {
			noData = true
			break
		}
	}

	var anchor *html.Node
	if !noData {
		if index < len(all) {
			anchor = all[index]
		}
	} else {
		content := make([]*html.Node, 0, len(all))
		for _, c := range all {
			if !isNoData(c) {
				content = append(content, c)
			}
		}
		switch {
		case index < len(content):
			anchor = content[index]
		case len(content) > 0:
			anchor = content[len(content)-1].NextSibling
		case len(all) > 0:
			anchor = all[0]
		}
	}

	if anchor == node {
		return
	}
	detach(node)
	if anchor != nil && anchor.Parent == parent {
		parent.InsertBefore(node, anchor)
		return
	}
	parent.AppendChild(node)
}

// remove is cheerioAdapter.remove: detach the node and mark the parent, whose children changed.
func (w *writer) remove(node *html.Node) {
	if node == nil {
		return
	}
	if node.Parent != nil {
		w.dirty[node.Parent] = true
	}
	detach(node)
}

func detach(n *html.Node) {
	if n != nil && n.Parent != nil {
		n.Parent.RemoveChild(n)
	}
}

// cloneNode is cheerioAdapter.clone: a deep copy whose no-data descendants are left out. A copied
// <template> gets its own deep-copied content entry, because the document's renderer reads template
// markup from that map rather than from the children.
func (w *writer) cloneNode(n *html.Node) *html.Node {
	if n == nil || isNoData(n) {
		return nil
	}

	c := &html.Node{
		Type:      n.Type,
		DataAtom:  n.DataAtom,
		Data:      n.Data,
		Namespace: n.Namespace,
	}
	if len(n.Attr) > 0 {
		c.Attr = append([]html.Attribute(nil), n.Attr...)
	}
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		if cc := w.cloneNode(child); cc != nil {
			c.AppendChild(cc)
		}
	}
	if kids := w.d.templateChildren(n); kids != nil {
		var copied []*html.Node
		for _, k := range kids {
			if ck := w.cloneNode(k); ck != nil {
				copied = append(copied, ck)
			}
		}
		if len(copied) > 0 {
			w.d.templateContent[c] = copied
		}
	}

	w.cloned[c] = true
	return c
}

// stripIds is cheerioAdapter.stripIds. The node itself is only stripped when its id has a value —
// cheerio reads it through attr(), where id="" is falsy — while descendants are matched by the
// [id] selector, so an empty id there is removed too.
func (w *writer) stripIds(node *html.Node) int {
	if node == nil {
		return 0
	}
	count := 0
	if v, ok := attrValue(node, "id"); ok && v != "" {
		removeAttr(node, "id")
		count++
	}

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if _, ok := attrValue(c, "id"); ok {
				removeAttr(c, "id")
				count++
			}
			walk(c)
		}
		for _, k := range w.d.templateChildren(n) {
			if _, ok := attrValue(k, "id"); ok {
				removeAttr(k, "id")
				count++
			}
			walk(k)
		}
	}
	walk(node)
	return count
}
