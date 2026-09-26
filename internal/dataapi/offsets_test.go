package dataapi

import (
	"testing"

	"golang.org/x/net/html"
)

// expectedSpan is one element's sourceCodeLocation as parse5 reported it, with -1 for the elements
// parse5 has no location for. The documents below were printed with:
//
//	cd hyper-html-api && node -e '
//	const cheerio=require("cheerio");
//	for (const src of process.argv.slice(1)) {
//	  const $=cheerio.load(src,{sourceCodeLocationInfo:true});
//	  const out=[];
//	  $("*").each((i,e)=>{const l=e.sourceCodeLocation; out.push([e.name, l?l.startOffset:null, l?l.endOffset:null])});
//	  console.log(JSON.stringify(src), JSON.stringify(out));
//	}' "<ul>\n<li>A\n<li>B</ul><p>x<div>y</div><table><tr><td>1</table>" "<!doctype html><html><head><title>T</title></head><body><h1 class=x>Hi</h1><br><img src=a.png/><p>a<b>b</b></p></body></html>" "<body><script>if (a<b) x()</script><style>p>a{}</style><textarea>t</textarea></body>" "<body><template><li>x</li></template><ul><li>A</li></ul></body>" "<svg><path d=\"M0\"/><g><rect/></g></svg><p>x" "<body><noscript><p>x</p></noscript></body>"
type expectedSpan struct {
	tag        string
	start, end int
}

type offsetDoc struct {
	name string
	src  string
	want []expectedSpan
}

var offsetDocs = []offsetDoc{
	{
		name: "implied ends and an implied tbody",
		src:  "<ul>\n<li>A\n<li>B</ul><p>x<div>y</div><table><tr><td>1</table>",
		want: []expectedSpan{
			{"html", -1, -1}, {"head", -1, -1}, {"body", -1, -1},
			{"ul", 0, 21}, {"li", 5, 11}, {"li", 11, 16},
			{"p", 21, 25}, {"div", 25, 37},
			{"table", 37, 61}, {"tbody", -1, -1}, {"tr", 44, 53}, {"td", 48, 53},
		},
	},
	{
		name: "everything written, void tags included",
		src:  `<!doctype html><html><head><title>T</title></head><body><h1 class=x>Hi</h1><br><img src=a.png/><p>a<b>b</b></p></body></html>`,
		want: []expectedSpan{
			{"html", 15, 125}, {"head", 21, 50}, {"title", 27, 43}, {"body", 50, 118},
			{"h1", 56, 75}, {"br", 75, 79}, {"img", 79, 95}, {"p", 95, 111}, {"b", 99, 107},
		},
	},
	{
		name: "raw text elements are one text token",
		src:  "<body><script>if (a<b) x()</script><style>p>a{}</style><textarea>t</textarea></body>",
		want: []expectedSpan{
			{"html", -1, -1}, {"head", -1, -1}, {"body", 0, 84},
			{"script", 6, 35}, {"style", 35, 55}, {"textarea", 55, 77},
		},
	},
	{
		name: "template content sits where its tokens do",
		src:  "<body><template><li>x</li></template><ul><li>A</li></ul></body>",
		want: []expectedSpan{
			{"html", -1, -1}, {"head", -1, -1}, {"body", 0, 63},
			{"template", 6, 37}, {"li", 16, 26}, {"ul", 37, 56}, {"li", 41, 51},
		},
	},
	{
		name: "foreign content",
		src:  `<svg><path d="M0"/><g><rect/></g></svg><p>x`,
		want: []expectedSpan{
			{"html", -1, -1}, {"head", -1, -1}, {"body", -1, -1},
			{"svg", 0, 39}, {"path", 5, 19}, {"g", 19, 33}, {"rect", 22, 29}, {"p", 39, 43},
		},
	},
	{
		name: "noscript is raw text with scripting on",
		src:  "<body><noscript><p>x</p></noscript></body>",
		want: []expectedSpan{
			{"html", -1, -1}, {"head", -1, -1}, {"body", 0, 42}, {"noscript", 6, 35},
		},
	},
}

// TestOffsetsMatchParse5 pins every paired element's byte range against parse5's, element by
// element in document order. Template content is part of that order at its template's position,
// which is where its bytes are.
//
// The elements parse5 has no location for are the ones the parser implied: a missing span is the
// expected answer, and a write reaching one of them falls back to the whole-document render.
func TestOffsetsMatchParse5(t *testing.T) {
	for _, doc := range offsetDocs {
		t.Run(doc.name, func(t *testing.T) {
			d, err := ParseBytes([]byte(doc.src))
			if err != nil {
				t.Fatalf("ParseBytes: %v", err)
			}
			spans, _ := sourceSpans(d, []byte(doc.src))

			nodes := sourceOrderElements(d)
			if len(nodes) != len(doc.want) {
				t.Fatalf("document order has %d elements, parse5 reports %d", len(nodes), len(doc.want))
			}
			for i, n := range nodes {
				want := doc.want[i]
				if n.Data != want.tag {
					t.Errorf("element %d is <%s>, want <%s>", i, n.Data, want.tag)
					continue
				}
				got, ok := spans[n]
				switch {
				case want.start < 0 && ok:
					t.Errorf("<%s> has span [%d,%d], parse5 has none", n.Data, got.start, got.end)
				case want.start >= 0 && !ok:
					t.Errorf("<%s> has no span, parse5 has [%d,%d]", n.Data, want.start, want.end)
				case ok && (got.start != want.start || got.end != want.end):
					t.Errorf("<%s> span = [%d,%d], parse5 = [%d,%d]", n.Data, got.start, got.end, want.start, want.end)
				}
			}
		})
	}
}

// TestOffsetsAdoptionAgency is the document the pairing cannot line up: <b> is closed by the second
// <p> and reopened after it, so the tree holds a <b> that no single source tag stands for. The
// reference still splices it, off parse5's own duplicated locations; this port must either pair the
// document the way parse5 did or refuse to pair it, never pair it wrongly.
func TestOffsetsAdoptionAgency(t *testing.T) {
	const src = "<p><b>1<p>2</b>3</p>"
	want := []expectedSpan{
		{"html", -1, -1}, {"head", -1, -1}, {"body", -1, -1},
		{"p", 0, 7}, {"b", 3, 7}, {"p", 7, 20}, {"b", 3, 15},
	}

	d, err := ParseBytes([]byte(src))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	spans, _ := sourceSpans(d, []byte(src))
	nodes := sourceOrderElements(d)
	if len(spans) == 0 {
		t.Log("the document could not be paired, so every write here falls back")
		return
	}

	if len(nodes) != len(want) {
		t.Fatalf("%d elements with spans, parse5 reports %d", len(nodes), len(want))
	}
	for i, n := range nodes {
		if n.Data != want[i].tag {
			t.Errorf("element %d is <%s>, want <%s>", i, n.Data, want[i].tag)
			continue
		}
		got, ok := spans[n]
		if want[i].start < 0 {
			if ok {
				t.Errorf("<%s> has span [%d,%d], parse5 has none", n.Data, got.start, got.end)
			}
			continue
		}
		if !ok {
			t.Errorf("<%s> has no span, parse5 has [%d,%d]", n.Data, want[i].start, want[i].end)
			continue
		}
		if got.start != want[i].start || got.end != want[i].end {
			t.Errorf("<%s> span = [%d,%d], parse5 = [%d,%d]", n.Data, got.start, got.end, want[i].start, want[i].end)
		}
	}
}

// TestTokenizerRawText records what one tokenizer pass makes of the raw-text elements. Their
// contents have to come back as a single text token: the parser builds one text node for them, so a
// token stream that still held their markup would have more start tags than the tree has elements
// and no document containing one could be paired.
//
// <plaintext> is the exception in the other direction: it has no end tag, so its text runs to EOF
// and the element ends with the file.
func TestTokenizerRawText(t *testing.T) {
	tags := []string{"script", "style", "textarea", "title", "xmp", "iframe", "noembed", "noframes", "noscript"}
	for _, tag := range tags {
		src := "<" + tag + "><p>a<b></" + tag + ">"
		tokens := tokenizeSource([]byte(src))
		want := []sourceToken{
			{kind: html.StartTagToken, name: tag},
			{kind: html.TextToken},
			{kind: html.EndTagToken, name: tag},
		}
		if len(tokens) != len(want) {
			t.Errorf("<%s>: %d tokens, want %d: %v", tag, len(tokens), len(want), tokens)
			continue
		}
		for i, w := range want {
			if tokens[i].kind != w.kind || tokens[i].name != w.name {
				t.Errorf("<%s>: token %d = %v %q, want %v %q", tag, i, tokens[i].kind, tokens[i].name, w.kind, w.name)
			}
		}
	}

	src := "<plaintext><p>a<b>"
	tokens := tokenizeSource([]byte(src))
	if len(tokens) != 2 {
		t.Fatalf("<plaintext>: %d tokens, want a start tag and one text token to EOF: %v", len(tokens), tokens)
	}
	if tokens[0].kind != html.StartTagToken || tokens[0].name != "plaintext" {
		t.Errorf("<plaintext>: first token = %v %q", tokens[0].kind, tokens[0].name)
	}
	if tokens[1].kind != html.TextToken || tokens[1].end != len(src) {
		t.Errorf("<plaintext>: second token = %v ending at %d, want text to %d", tokens[1].kind, tokens[1].end, len(src))
	}
}

// sourceOrderElements lists the elements the way the pairing walk visits them: template content at
// the template's position, since its tokens are there in the source.
func sourceOrderElements(d *Document) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			out = append(out, n)
		}
		if kids := d.templateChildren(n); kids != nil {
			for _, c := range kids {
				walk(c)
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(d.Root)
	return out
}
