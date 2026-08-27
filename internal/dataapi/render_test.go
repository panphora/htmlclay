package dataapi

import "testing"

// TestSerializerMatchesCheerio is a recorded differential. Every innerHTML and outerHTML below was
// produced by running the same fragment through the JS engine on cheerio 1.2.0, so this compares
// two independent serializers rather than the port against itself.
//
// It is the reason render.go exists instead of a call to html.Render: on this table html.Render
// disagreed on 14 of 36 comparisons, across void tags, U+00A0, and attribute quote escaping.
func TestSerializerMatchesCheerio(t *testing.T) {
	cases := []struct {
		name      string
		fragment  string
		innerHTML string
		outerHTML string
	}{
		{
			name:      "bare_attr",
			fragment:  "<a data-x>y</a>",
			innerHTML: "<a data-x=\"\">y</a>",
			outerHTML: "<div id=\"w\"><a data-x=\"\">y</a></div>",
		},
		{
			name:      "boolean_attr",
			fragment:  "<input disabled>",
			innerHTML: "<input disabled=\"\">",
			outerHTML: "<div id=\"w\"><input disabled=\"\"></div>",
		},
		{
			name:      "comment",
			fragment:  "a<!-- c -->b",
			innerHTML: "a<!-- c -->b",
			outerHTML: "<div id=\"w\">a<!-- c -->b</div>",
		},
		{
			name:      "empty_attr",
			fragment:  "<a data-x=\"\">y</a>",
			innerHTML: "<a data-x=\"\">y</a>",
			outerHTML: "<div id=\"w\"><a data-x=\"\">y</a></div>",
		},
		{
			name:      "entities_amp",
			fragment:  "a &amp; b &lt; c",
			innerHTML: "a &amp; b &lt; c",
			outerHTML: "<div id=\"w\">a &amp; b &lt; c</div>",
		},
		{
			name:      "entities_raw",
			fragment:  "a & b < c > d",
			innerHTML: "a &amp; b &lt; c &gt; d",
			outerHTML: "<div id=\"w\">a &amp; b &lt; c &gt; d</div>",
		},
		{
			name:      "nbsp",
			fragment:  "a\u00a0b",
			innerHTML: "a&nbsp;b",
			outerHTML: "<div id=\"w\">a&nbsp;b</div>",
		},
		{
			name:      "nested",
			fragment:  "<p>one<span>two</span></p>",
			innerHTML: "<p>one<span>two</span></p>",
			outerHTML: "<div id=\"w\"><p>one<span>two</span></p></div>",
		},
		{
			name:      "quotes_in_attr",
			fragment:  "<a title='he said \"hi\"'>x</a>",
			innerHTML: "<a title=\"he said &quot;hi&quot;\">x</a>",
			outerHTML: "<div id=\"w\"><a title=\"he said &quot;hi&quot;\">x</a></div>",
		},
		{
			name:      "raw_script",
			fragment:  "<script>if (a < b && c > d) {}</script>",
			innerHTML: "<script>if (a < b && c > d) {}</script>",
			outerHTML: "<div id=\"w\"><script>if (a < b && c > d) {}</script></div>",
		},
		{
			name:      "raw_style",
			fragment:  "<style>a::before{content:'<'}</style>",
			innerHTML: "<style>a::before{content:'<'}</style>",
			outerHTML: "<div id=\"w\"><style>a::before{content:'<'}</style></div>",
		},
		{
			name:      "single_quote_attr",
			fragment:  "<a title=\"it's\">x</a>",
			innerHTML: "<a title=\"it's\">x</a>",
			outerHTML: "<div id=\"w\"><a title=\"it's\">x</a></div>",
		},
		{
			name:      "textarea",
			fragment:  "<textarea>a < b</textarea>",
			innerHTML: "<textarea>a &lt; b</textarea>",
			outerHTML: "<div id=\"w\"><textarea>a &lt; b</textarea></div>",
		},
		{
			name:      "unicode",
			fragment:  "café 中文 😀",
			innerHTML: "café 中文 😀",
			outerHTML: "<div id=\"w\">café 中文 😀</div>",
		},
		{
			name:      "uppercase_tag",
			fragment:  "<DIV CLASS=x>y</DIV>",
			innerHTML: "<div class=\"x\">y</div>",
			outerHTML: "<div id=\"w\"><div class=\"x\">y</div></div>",
		},
		{
			name:      "void_br",
			fragment:  "a<br>b",
			innerHTML: "a<br>b",
			outerHTML: "<div id=\"w\">a<br>b</div>",
		},
		{
			name:      "void_hr",
			fragment:  "<hr>",
			innerHTML: "<hr>",
			outerHTML: "<div id=\"w\"><hr></div>",
		},
		{
			name:      "void_img",
			fragment:  "<img src=\"x.png\" alt=\"a>b\">",
			innerHTML: "<img src=\"x.png\" alt=\"a>b\">",
			outerHTML: "<div id=\"w\"><img src=\"x.png\" alt=\"a>b\"></div>",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := doc(t, `<!doctype html><html><body><div id="w">`+c.fragment+`</div></body></html>`)

			if got := rule(t, root, "#w@innerHTML"); got != marshalString(t, c.innerHTML) {
				t.Errorf("innerHTML\n got %s\nwant %s", got, marshalString(t, c.innerHTML))
			}
			if got := rule(t, root, "#w@outerHTML"); got != marshalString(t, c.outerHTML) {
				t.Errorf("outerHTML\n got %s\nwant %s", got, marshalString(t, c.outerHTML))
			}
		})
	}
}

// marshalString renders an expected string through the package's own marshaller, so the
// comparison is against the bytes a response would actually carry.
func marshalString(t *testing.T, s string) string {
	t.Helper()
	return marshal(t, s)
}
