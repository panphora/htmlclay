package dataapi

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

// missing is JSON's absent value, which is not the same as null and has to be told apart: the
// reference skips a rule whose body key is absent, but validates and writes a rule whose key holds
// null. Every value that flows here comes from decodeJSON, so this sentinel is the only absent.
type missingValue struct{}

var missing Value = missingValue{}

func isUndefined(v Value) bool {
	_, ok := v.(missingValue)
	return ok
}

// booleanProps is apply.js's BOOLEAN_PROPS: names coerced to a boolean before the compare and
// written as attribute presence rather than as a value. readOnly is camelCase here and matched
// case-sensitively, exactly as the reference set is.
var booleanProps = map[string]bool{
	"checked": true, "selected": true, "disabled": true, "readOnly": true, "paused": true,
}

// domPropertiesWrite is DOM_PROPERTIES_WRITE_SET: names a `sel@name` rule may write through the
// property interface. Everything outside it is a literal attribute.
var domPropertiesWrite = map[string]bool{
	"textContent": true, "innerText": true, "innerHTML": true,
	"value": true, "checked": true, "selected": true, "disabled": true, "readOnly": true,
	"type": true, "id": true, "className": true, "title": true,
}

// domPropertiesReadOnly is DOM_PROPERTIES_READ_ONLY_SET: properties the DOM itself refuses to
// write, refused before the adapter sees them so the error type is the same on every engine.
var domPropertiesReadOnly = map[string]bool{
	"tagName": true, "nodeName": true, "nodeType": true, "nodeValue": true,
	"childElementCount": true, "classList": true, "baseURI": true, "documentURI": true,
	"contentType": true, "offsetWidth": true, "offsetHeight": true, "clientWidth": true,
	"clientHeight": true, "scrollWidth": true, "scrollHeight": true, "currentSrc": true,
	"duration": true, "paused": true, "dataset": true,
}

// writer carries the document being written, the refusals the policy collected, and the nodes the
// writes touched. Every write is checked before it happens, so a refusal always means the write was
// skipped rather than undone.
type writer struct {
	d        *Document
	refusals []Refusal
	dirty    map[*html.Node]bool
	cloned   map[*html.Node]bool
}

// validateShape checks the body against the rule tree. It walks every branch rather than stopping
// at the first problem, because the caller reports all of them in one error.
func validateShape(rule, value Value, path []any, mismatches *[]Mismatch) {
	if isUndefined(value) {
		return
	}

	switch r := rule.(type) {
	case string:
		if strings.HasSuffix(r, "[]") {
			items, ok := value.([]Value)
			if !ok {
				pushMismatch(mismatches, path, "array", value)
				return
			}
			for i, item := range items {
				if isNonScalar(item) {
					pushMismatch(mismatches, appendPath(path, i), "scalar", item)
				}
			}
			return
		}
		if isNonScalar(value) {
			pushMismatch(mismatches, path, "scalar", value)
		}

	case []Value:
		items, ok := value.([]Value)
		if !ok {
			pushMismatch(mismatches, path, "array", value)
			return
		}
		var shape Value
		if len(r) > 1 {
			shape = r[1]
		}
		for i, item := range items {
			validateShape(shape, item, appendPath(path, i), mismatches)
		}

	case *Object:
		valueObj, ok := value.(*Object)
		if !ok {
			pushMismatch(mismatches, path, "object", value)
			return
		}
		for _, key := range r.Keys() {
			sub, _ := r.Get(key)
			v, has := valueObj.Get(key)
			if !has {
				v = missing
			}
			validateShape(sub, v, appendPath(path, key), mismatches)
		}
	}
}

func pushMismatch(mismatches *[]Mismatch, path []any, expected string, got Value) {
	*mismatches = append(*mismatches, Mismatch{
		Path:     pathString(path),
		Expected: expected,
		Got:      typeofX(got),
	})
}

// isNonScalar reports whether a value is a nested object or array, the only shapes a scalar rule
// rejects. null, "", numbers and booleans are all valid scalars: they clear or set the value.
func isNonScalar(v Value) bool {
	if v == nil {
		return false
	}
	switch v.(type) {
	case *Object, []Value:
		return true
	}
	return false
}

func typeofX(v Value) string {
	switch v.(type) {
	case nil:
		return "null"
	case []Value:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64, int:
		return "number"
	case missingValue:
		return "undefined"
	}
	return "object"
}

// applyAt walks the rule tree against the body, writing as it goes, and returns the context node
// most rules leave alone.
func (w *writer) applyAt(ctx *html.Node, rule, value Value, depth int, path []string) (*html.Node, error) {
	if depth > maxRuleDepth {
		return nil, &MaxRuleDepthExceeded{Path: path}
	}
	if isUndefined(value) {
		return ctx, nil
	}

	switch r := rule.(type) {
	case string:
		return w.applyScalar(ctx, r, value, depth, path)

	case []Value:
		// JS destructures [selector, shape]; a shorter array leaves the shape undefined and any
		// extra elements are ignored. Undefined and null are both "no shape", but only null is a
		// scalar list: the reference walks an undefined shape as a rule-less list, which writes
		// nothing. The missing sentinel keeps the two apart.
		var selector Value
		if len(r) > 0 {
			selector = r[0]
		}
		shape := Value(missing)
		if len(r) > 1 {
			shape = r[1]
		}
		items, ok := value.([]Value)
		if !ok {
			return ctx, nil
		}
		return ctx, w.listDiff(ctx, selector, shape, items, depth, path)

	case *Object:
		for _, key := range r.Keys() {
			sub, _ := r.Get(key)
			subValue := Value(missing)
			if value == nil {
				subValue = nil
			} else if valueObj, ok := value.(*Object); ok {
				if v, has := valueObj.Get(key); has {
					subValue = v
				}
			}
			next, err := w.applyAt(ctx, sub, subValue, depth+1, appendString(path, key))
			if err != nil {
				return nil, err
			}
			if next != nil && next != ctx {
				ctx = next
			}
		}
	}
	return ctx, nil
}

func (w *writer) applyScalar(ctx *html.Node, rule string, value Value, depth int, path []string) (*html.Node, error) {
	if strings.HasSuffix(rule, "[]") {
		items, ok := value.([]Value)
		if !ok {
			return ctx, nil
		}
		return ctx, w.listDiff(ctx, strings.TrimSuffix(rule, "[]"), nil, items, depth, path)
	}

	if strings.HasPrefix(rule, "@") {
		return ctx, w.writePropOrAttr(ctx, rule[1:], value)
	}

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
			return ctx, nil
		}
		return ctx, w.writePropOrAttr(matches[0], name, value)
	}

	if rule == "." {
		w.setText(ctx, value)
		return ctx, nil
	}

	matches, err := Find(ctx, rule, FindOpts{})
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return ctx, nil
	}
	w.setText(matches[0], value)
	return ctx, nil
}

// setText writes a plain text rule. The comparison is against the TRIMMED text, so re-applying the
// value already on the page is a no-op rather than a childList record on every keystroke.
func (w *writer) setText(n *html.Node, value Value) {
	target := ""
	if value != nil {
		target = jsString(value)
	}
	if jsTrim(contentText(n)) == target {
		return
	}
	if !w.allowText(n) {
		return
	}
	w.replaceText(n, target)
	w.dirty[n] = true
}

// writePropOrAttr writes `sel@name`. Read-only properties throw, the two HTML rule forms are
// refused, and everything else is either a property write or a literal attribute write. Every
// branch compares before writing: a write of the value already there still rebuilds innerHTML or
// emits a mutation record in the reference, so it is skipped here too.
func (w *writer) writePropOrAttr(n *html.Node, name string, value Value) error {
	if domPropertiesReadOnly[name] {
		return &RuleTargetReadOnly{Name: name}
	}
	if name == "outerHTML" {
		w.refuse(n, `"@outerHTML" writes HTML, only text is allowed`)
		return nil
	}
	if name == "innerHTML" {
		if !sameString(contentInnerHTML(w.d, n), propNext(value)) {
			w.refuse(n, `"@innerHTML" writes HTML, only text is allowed`)
		}
		return nil
	}
	if name == "textContent" || name == "innerText" {
		next := propNext(value)
		if !sameString(contentText(n), next) && w.allowText(n) {
			w.replaceText(n, jsString(next))
			w.dirty[n] = true
		}
		return nil
	}
	if !domPropertiesWrite[name] {
		next := jsStringOrEmpty(value)
		if cur, ok := getAttr(n, name); !ok || cur != next {
			if w.allowAttr(n, name, next) {
				setAttr(n, name, next)
				w.dirty[n] = true
			}
		}
		return nil
	}

	next := coercePropValue(name, value)
	if w.propMatches(n, name, next) {
		return nil
	}
	attrName := name
	if attrName == "className" {
		attrName = "class"
	}
	if !w.allowAttr(n, attrName, next) {
		return nil
	}
	w.setProp(n, name, next)
	w.dirty[n] = true
	return nil
}

// coercePropValue is apply.js's coercion, which exists because a form round-trip delivers a boolean
// as the STRING "false" and Boolean("false") would check an unchecked box.
func coercePropValue(name string, value Value) Value {
	if value == nil {
		if booleanProps[name] {
			return false
		}
		return ""
	}
	if booleanProps[name] {
		if s, ok := value.(string); ok && s == "false" {
			return false
		}
		return truthy(value)
	}
	return value
}

// propMatches is the compare-before-write for a property, with the read the reference makes for that
// name: presence for a boolean property, the class attribute for className, and the property read
// everywhere else.
func (w *writer) propMatches(n *html.Node, name string, next Value) bool {
	if booleanProps[name] {
		want, ok := next.(bool)
		if !ok {
			return false
		}
		cur, _ := propValue(w.d, n, name)
		return (cur == "true") == want
	}
	if name == "className" {
		cur, ok := attrValue(n, "class")
		return ok && sameString(cur, next)
	}
	cur, ok := propValue(w.d, n, name)
	return ok && sameString(cur, next)
}

// setProp is cheerio's setProp: the four boolean attribute names (case-insensitively) become
// attribute PRESENCE, an empty value when truthy and no attribute at all when falsy, and every
// other name becomes a string attribute.
func (w *writer) setProp(n *html.Node, name string, value Value) {
	if isBooleanAttr(name) {
		if b, _ := value.(bool); b {
			setAttr(n, name, "")
		} else {
			removeAttr(n, name)
		}
		return
	}
	target := name
	if name == "className" {
		target = "class"
	}
	setAttr(n, target, jsString(value))
}

// replaceText replaces n's children with one text node. An element holding no-data children keeps
// them: only the projected content is replaced, so a UI control inside a text region survives an
// unrelated write.
func (w *writer) replaceText(n *html.Node, text string) {
	if !hasNoData(n) {
		for c := n.FirstChild; c != nil; {
			next := c.NextSibling
			n.RemoveChild(c)
			c = next
		}
		n.AppendChild(&html.Node{Type: html.TextNode, Data: text})
		return
	}

	// Mirrors replaceProjected: remove only the children that are not no-data, then insert the
	// new content where the first removed child was, so the no-data children keep their places.
	all := childNodes(n)
	included := make([]*html.Node, 0, len(all))
	for _, c := range all {
		if !isNoData(c) {
			included = append(included, c)
		}
	}
	includedSet := map[*html.Node]bool{}
	for _, c := range included {
		includedSet[c] = true
	}
	var anchor *html.Node
	if len(included) > 0 {
		idx := indexOfNode(all, included[0])
		for idx != -1 && idx < len(all) && includedSet[all[idx]] {
			idx++
		}
		if idx != -1 && idx < len(all) {
			anchor = all[idx]
		}
	}
	for _, c := range included {
		n.RemoveChild(c)
	}
	if text == "" {
		return
	}
	textNode := &html.Node{Type: html.TextNode, Data: text}
	if anchor != nil {
		n.InsertBefore(textNode, anchor)
	} else {
		n.AppendChild(textNode)
	}
}

func (w *writer) refuse(n *html.Node, reason string) bool {
	w.refusals = append(w.refusals, Refusal{Target: describe(n), Reason: reason})
	return false
}

func (w *writer) allowTarget(n *html.Node) bool {
	if tag := refusedAncestorTag(n); tag != "" {
		return w.refuse(n, "inside <"+tag+">, which is not content")
	}
	return true
}

func (w *writer) allowText(n *html.Node) bool {
	if !w.allowTarget(n) {
		return false
	}
	if w.holdsRulesTag(n) {
		return w.refuse(n, "a text write here would delete the data rules tag")
	}
	return true
}

func (w *writer) allowAttr(n *html.Node, name string, value Value) bool {
	if !w.allowTarget(n) {
		return false
	}
	if reason := checkAttribute(name, value, writePolicy); reason != "" {
		return w.refuse(n, reason)
	}
	return true
}

// holdsRulesTag reports whether a rules tag is a DESCENDANT of n, which is what makes a text write
// on n destructive: it would replace the tag's body along with the content.
func (w *writer) holdsRulesTag(n *html.Node) bool {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if inNoData(c) {
			continue
		}
		if isRulesTag(c) || w.holdsRulesTag(c) {
			return true
		}
	}
	return false
}

func refusedAncestorTag(n *html.Node) string {
	for cur := n; cur != nil; cur = cur.Parent {
		if cur.Type != html.ElementNode {
			continue
		}
		if containsString(writePolicy.RefusedTargets, cur.Data) {
			return cur.Data
		}
	}
	return ""
}

// describe is the target a refusal names, as the reference spells it: <tag#id>, or <tag>.
func describe(n *html.Node) string {
	tag := ""
	if n != nil && n.Type == html.ElementNode {
		tag = n.Data
	}
	if tag == "" {
		tag = "node"
	}
	if id, ok := attrValue(n, "id"); ok && id != "" {
		return "<" + tag + "#" + id + ">"
	}
	return "<" + tag + ">"
}

func setAttr(n *html.Node, name, value string) {
	for i := range n.Attr {
		if n.Attr[i].Key == name {
			n.Attr[i].Val = value
			return
		}
	}
	n.Attr = append(n.Attr, html.Attribute{Key: name, Val: value})
}

func removeAttr(n *html.Node, name string) {
	for i := range n.Attr {
		if n.Attr[i].Key == name {
			n.Attr = append(n.Attr[:i], n.Attr[i+1:]...)
			return
		}
	}
}

// childNodes returns n's children, which is cheerio's contents(): text and comments included.
func childNodes(n *html.Node) []*html.Node {
	var out []*html.Node
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		out = append(out, c)
	}
	return out
}

func indexOfNode(nodes []*html.Node, want *html.Node) int {
	for i, n := range nodes {
		if n == want {
			return i
		}
	}
	return -1
}

// hasNoData is the write path's own question: is this element, or anything under it, no-data? It
// decides whether a text write can replace the children wholesale or has to project around them.
func hasNoData(n *html.Node) bool {
	return n != nil && n.Type == html.ElementNode && (isNoData(n) || hasNoDataDescendant(n))
}

// propNext is coercePropValue for the two text-forming props: null and undefined become "", and
// nothing else is coerced, so a number compares as a number.
func propNext(value Value) Value {
	if value == nil {
		return ""
	}
	return value
}

func sameString(cur string, next Value) bool {
	s, ok := next.(string)
	return ok && cur == s
}

func jsStringOrEmpty(v Value) string {
	if v == nil {
		return ""
	}
	return jsString(v)
}

func jsString(v Value) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case missingValue:
		return "undefined"
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return jsNumberString(t)
	case int:
		return strconv.Itoa(t)
	case []Value:
		parts := make([]string, len(t))
		for i, item := range t {
			if item != nil && !isUndefined(item) {
				parts[i] = jsString(item)
			}
		}
		return strings.Join(parts, ",")
	case *Object:
		return "[object Object]"
	}
	return fmt.Sprint(v)
}

// jsNumberString is String(n). encoding/json's float formatting is the ES6 Number::toString
// algorithm: positional notation down to 1e-6, an exponent outside [1e-6, 1e21), and an exponent
// with no leading zero ("1e-7"). FormatFloat's 'g' agrees with it on ordinary magnitudes but
// switches to an exponent below 1e-4 and writes the exponent as "1e-07", so a body carrying
// 0.00001 or 1e-7 was written with different text than the reference's.
//
// The two values JSON cannot hold are handled here because json.Marshal errors on them, while
// String() has a spelling for each: a literal that overflows to Infinity (decodeJSON's ParseFloat
// range error) and negative zero, which String() writes as "0".
func jsNumberString(f float64) string {
	if math.IsNaN(f) {
		return "NaN"
	}
	if math.IsInf(f, 1) {
		return "Infinity"
	}
	if math.IsInf(f, -1) {
		return "-Infinity"
	}
	if f == 0 {
		return "0"
	}
	b, err := json.Marshal(f)
	if err != nil {
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	return string(b)
}

func truthy(v Value) bool {
	switch t := v.(type) {
	case nil:
		return false
	case missingValue:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	case int:
		return t != 0
	}
	return true
}

func appendString(path []string, part string) []string {
	out := make([]string, len(path)+1)
	copy(out, path)
	out[len(path)] = part
	return out
}
