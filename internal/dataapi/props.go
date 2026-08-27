package dataapi

import (
	"strings"

	"golang.org/x/net/html"
)

// domProperties is the allowlist of names read through the property interface rather than as
// literal attributes. Membership matters twice: it picks the read path, and it SHADOWS any
// same-named attribute, so `@dataset` never returns a `dataset="…"` attribute.
//
// Ported from dom-properties.js. href, src and action are deliberately absent — they read as plain
// attributes so the round-trip stays byte-stable.
var domProperties = map[string]bool{
	"textContent": true, "innerText": true, "innerHTML": true, "outerHTML": true,
	"value": true, "checked": true, "selected": true, "disabled": true, "readOnly": true,
	"type":    true,
	"tagName": true, "nodeName": true, "nodeType": true, "nodeValue": true,
	"childElementCount": true,
	"id":                true, "className": true, "classList": true,
	"baseURI":     true,
	"offsetWidth": true, "offsetHeight": true, "clientWidth": true, "clientHeight": true,
	"scrollWidth": true, "scrollHeight": true,
	"dataset":    true,
	"currentSrc": true, "duration": true, "paused": true,
	"title": true, "documentURI": true, "contentType": true,
}

// booleanAttrs is cheerio's rboolean. A name in this set is read as presence rather than value,
// and — separately — reading it as a plain ATTRIBUTE returns the name itself, not the stored value.
// Matched case-insensitively, exactly as the /i regex does.
var booleanAttrs = map[string]bool{
	"autofocus": true, "autoplay": true, "async": true, "checked": true, "controls": true,
	"defer": true, "disabled": true, "hidden": true, "loop": true, "multiple": true,
	"open": true, "readonly": true, "required": true, "scoped": true, "selected": true,
}

func isBooleanAttr(name string) bool { return booleanAttrs[strings.ToLower(name)] }

// readPropOrAttr is the whole read contract for `@name`, ported from extract.js.
//
// The two branches treat emptiness differently and that asymmetry is real: the property branch
// keeps an empty string, the attribute branch turns it into null, because the JS is `return v ? v
// : null` and "" is falsy. So `@className` on class="" is "" while `@data-x` on data-x="" is null.
func readPropOrAttr(d *Document, n *html.Node, name string) Value {
	if domProperties[name] {
		v, ok := propValue(d, n, name)
		if !ok {
			return nil
		}
		return v
	}
	v, ok := getAttr(n, name)
	if !ok || v == "" {
		return nil
	}
	return v
}

// propValue ports cheerio's prop() switch plus getProp, with the four names the hyper-html-api
// adapter intercepts before cheerio ever sees them (innerHTML, textContent, innerText, className)
// folded in. Returns ok=false where JS produced null or undefined.
func propValue(d *Document, n *html.Node, name string) (string, bool) {
	if n == nil {
		return "", false
	}

	switch name {
	// Intercepted by the adapter, because cheerio's .prop() does not expose them properly.
	// NOTE the trim asymmetry: these are NOT trimmed, while adapter.text() — which every
	// selector-shaped rule goes through — is. Trimming uniformly here would be invisible and wrong.
	case "textContent", "innerText":
		return textContent(n), true
	case "innerHTML":
		return innerHTML(d, n), true
	case "className":
		return attrOrEmpty(n, "class")

	// cheerio's own prop() switch.
	case "outerHTML":
		return outerHTML(d, n), true
	case "tagName", "nodeName":
		if n.Type != html.ElementNode {
			return "", false
		}
		return strings.ToUpper(n.Data), true

	// getProp's `name in el` branch: these resolve against the parser's node object, not the
	// document. nodeValue is null for elements, which is every node a selector can return.
	case "nodeType":
		return nodeTypeString(n)
	case "nodeValue":
		return "", false

	// DIVERGENCE (tier 3, deliberate). In JS `'type' in el` is true, so this returns the PARSER's
	// node type — "tag" for every element, "script" inside a script — and never the type
	// attribute anyone asking for `@type` wants. htmlclay has no existing users of this API, so
	// preserving the bug buys nothing. Fixed: read it as the ordinary attribute it looks like.
	case "type":
		return attrOrNull(n, "type")
	}

	// DIVERGENCE (tier 3, deliberate). readOnly reaches the boolean branch because rboolean is
	// case-insensitive, but the lookup that follows is case-SENSITIVE against attribute names the
	// parser has already lower-cased, so `readonly` is never found and `@readOnly` is always
	// "false". Fixed by folding the name for the presence test — a no-op for every other boolean,
	// since they are already lower-case.
	if isBooleanAttr(name) {
		_, present := getAttr(n, strings.ToLower(name))
		if present {
			return "true", true
		}
		return "false", true
	}

	return getAttr(n, name)
}

// getAttr ports cheerio's getAttr, including three fallbacks that are not obvious from the DOM it
// is imitating and that a from-scratch implementation would miss.
func getAttr(n *html.Node, name string) (string, bool) {
	if n == nil || n.Type != html.ElementNode {
		return "", false
	}

	if v, ok := attrValue(n, name); ok {
		// A present boolean attribute reads back as its own NAME, not its stored value. So
		// `@hidden` on <div hidden> is "hidden", and on <div hidden="x"> it is still "hidden".
		if isBooleanAttr(name) {
			return name, true
		}
		return v, true
	}

	// An <option> with no value attribute reports its text, mimicking the DOM.
	if n.Data == "option" && name == "value" {
		return textContent(n), true
	}

	// A radio or checkbox with no value attribute reports "on", also mimicking the DOM. The type
	// comparison is case-sensitive on the VALUE, which parsers do not normalise, so type="RADIO"
	// does not qualify. Faithful to cheerio.
	if n.Data == "input" && name == "value" {
		if t, _ := attrValue(n, "type"); t == "radio" || t == "checkbox" {
			return "on", true
		}
	}

	return "", false
}

func attrOrNull(n *html.Node, name string) (string, bool) {
	return getAttr(n, name)
}

// attrOrEmpty is the className read: cheerio returns node.attr('class'), so a missing attribute is
// null but an empty one is the empty string.
func attrOrEmpty(n *html.Node, name string) (string, bool) {
	v, ok := attrValue(n, name)
	if !ok {
		return "", false
	}
	return v, true
}

func nodeTypeString(n *html.Node) (string, bool) {
	switch n.Type {
	case html.ElementNode:
		return "1", true
	case html.TextNode:
		return "3", true
	case html.CommentNode:
		return "8", true
	case html.DocumentNode:
		return "9", true
	case html.DoctypeNode:
		return "10", true
	}
	return "", false
}

// textContent concatenates every descendant text node, including the contents of <script>,
// <style> and <template>, and excluding comments. That is domutils' textContent, which is what
// cheerio's .text() uses.
func textContent(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(cur *html.Node) {
		for c := cur.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.TextNode {
				b.WriteString(c.Data)
			}
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

// jsTrim is String.prototype.trim, which strips the same character class the tokenizer skips.
// strings.TrimSpace would strip U+0085 and keep U+FEFF, wrong in both directions. See isJSSpace.
func jsTrim(s string) string {
	return strings.TrimFunc(s, isJSSpace)
}

// trimmedText is adapter.text(): the trimmed form every selector-shaped rule returns.
func trimmedText(n *html.Node) string { return jsTrim(textContent(n)) }
