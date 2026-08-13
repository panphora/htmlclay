package dataapi

import (
	"strings"

	"golang.org/x/net/html"
)

// This file exists because golang.org/x/net/html.Render and the JS side's dom-serializer disagree
// in four systematic ways, and @innerHTML / @outerHTML would otherwise differ on almost any real
// document. Measured across eighteen fragments: 22 of 36 comparisons already agreed, and every
// disagreement fell into one of these:
//
//	                      html.Render      dom-serializer
//	void elements         <br/>            <br>
//	U+00A0 (text + attr)  raw byte         &nbsp;
//	" in an attribute     &#34;            &quot;
//	' in an attribute     &#39;            '        (not escaped)
//	> in an attribute     &gt;             >        (not escaped)
//
// Everything else matched, including the parts most likely to go wrong in a hand-rolled
// serializer: raw text inside <script> and <style> left unescaped, entity normalisation, tag-name
// lower-casing, valueless attributes becoming ="", comments, and non-ASCII text. So this replaces
// only the escaping and void-tag layer and keeps the same tree walk.
//
// The tables below are dom-serializer's escapeAttribute and escapeText, which is what cheerio uses
// with its default decodeEntities:true.

// rawTextElements have their children emitted verbatim. This is dom-serializer's
// unencodedElements; escaping inside them would corrupt the script or stylesheet.
var rawTextElements = map[string]bool{
	"style": true, "script": true, "xmp": true, "iframe": true,
	"noembed": true, "noframes": true, "plaintext": true, "noscript": true,
}

// voidElements have no closing tag and, unlike html.Render's output, no self-closing slash.
var voidElements = map[string]bool{
	"area": true, "base": true, "basefont": true, "bgsound": true, "br": true, "col": true,
	"embed": true, "frame": true, "hr": true, "img": true, "input": true, "keygen": true,
	"link": true, "meta": true, "param": true, "source": true, "track": true, "wbr": true,
}

// escapeText escapes &, <, > and U+00A0. Note > IS escaped in text and is NOT in attributes.
func escapeText(b *strings.Builder, s string) {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case 0xC2:
			// U+00A0 is C2 A0 in UTF-8. A lone C2 is invalid and passes through unchanged.
			if i+1 < len(s) && s[i+1] == 0xA0 {
				b.WriteString("&nbsp;")
				i++
			} else {
				b.WriteByte(c)
			}
		default:
			b.WriteByte(c)
		}
	}
}

// escapeAttribute escapes &, " and U+00A0 — and nothing else. Values are always double-quoted, so
// a bare ' is safe, and > carries no meaning inside a quoted value.
func escapeAttribute(b *strings.Builder, s string) {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '&':
			b.WriteString("&amp;")
		case '"':
			b.WriteString("&quot;")
		case 0xC2:
			if i+1 < len(s) && s[i+1] == 0xA0 {
				b.WriteString("&nbsp;")
				i++
			} else {
				b.WriteByte(c)
			}
		default:
			b.WriteByte(c)
		}
	}
}

// renderNode writes one node and its subtree. rawText is set once inside a raw-text element and
// inherited by its descendants, so nested text is never escaped.
func renderNode(d *Document, b *strings.Builder, n *html.Node, rawText bool) {
	switch n.Type {
	case html.TextNode:
		if rawText {
			b.WriteString(n.Data)
		} else {
			escapeText(b, n.Data)
		}
		return

	case html.CommentNode:
		b.WriteString("<!--")
		b.WriteString(n.Data)
		b.WriteString("-->")
		return

	case html.DoctypeNode:
		b.WriteString("<!DOCTYPE ")
		b.WriteString(n.Data)
		b.WriteString(">")
		return

	case html.DocumentNode:
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			renderNode(d, b, c, rawText)
		}
		return

	case html.ElementNode:
		// fall through

	default:
		return
	}

	b.WriteString("<")
	b.WriteString(n.Data)
	for _, a := range n.Attr {
		b.WriteString(" ")
		if a.Namespace != "" {
			b.WriteString(a.Namespace)
			b.WriteString(":")
		}
		b.WriteString(a.Key)
		// A valueless attribute serialises as ="", matching both engines.
		b.WriteString(`="`)
		escapeAttribute(b, a.Val)
		b.WriteString(`"`)
	}
	b.WriteString(">")

	if voidElements[n.Data] && n.Namespace == "" {
		return
	}

	childRaw := rawText || rawTextElements[n.Data]
	// A <template>'s children live in the document's side map, having been detached so no
	// selector can reach them. Serialization is the one accessor that must still see them.
	if kids := d.templateChildren(n); kids != nil {
		for _, c := range kids {
			renderNode(d, b, c, childRaw)
		}
	} else {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			renderNode(d, b, c, childRaw)
		}
	}

	b.WriteString("</")
	b.WriteString(n.Data)
	b.WriteString(">")
}

// isInRawText reports whether n sits inside a raw-text element, which decides whether its own
// children get escaped when rendering starts partway down the tree.
func isInRawText(n *html.Node) bool {
	for cur := n.Parent; cur != nil; cur = cur.Parent {
		if cur.Type == html.ElementNode && rawTextElements[cur.Data] {
			return true
		}
	}
	return false
}

func innerHTML(d *Document, n *html.Node) string {
	var b strings.Builder
	raw := rawTextElements[n.Data] || isInRawText(n)
	if kids := d.templateChildren(n); kids != nil {
		for _, c := range kids {
			renderNode(d, &b, c, raw)
		}
		return b.String()
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		renderNode(d, &b, c, raw)
	}
	return b.String()
}

func outerHTML(d *Document, n *html.Node) string {
	var b strings.Builder
	renderNode(d, &b, n, isInRawText(n))
	return b.String()
}
