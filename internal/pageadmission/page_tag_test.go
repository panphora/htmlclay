package pageadmission

import (
	"strings"
	"testing"
)

func pageTagYJS(seed string) string {
	return `<script type="application/json" merge="page" clay="live">{"v":1,"y":{"seed":"` + seed + `"}}</script>`
}

func TestPageTagRoundTrips(t *testing.T) {
	tag := pageTagYJS("state-1")
	cases := []struct {
		name string
		with string
	}{
		{
			name: "inline",
			with: `<!DOCTYPE html><html><body><p>outside</p>` + tag +
				`<article data-page data-rev="1"><p>text</p></article></body></html>`,
		},
		{
			name: "own line",
			with: "<!DOCTYPE html>\n<html>\n<body>\n  <p>outside</p>\n  " + tag +
				"\n  <article data-page data-rev=\"1\"><p>text</p></article>\n</body>\n</html>",
		},
		{
			name: "crlf",
			with: "<!DOCTYPE html>\r\n<html>\r\n<body>\r\n  <p>outside</p>\r\n  " + tag +
				"\r\n  <article data-page data-rev=\"1\"><p>text</p></article>\r\n</body>\r\n</html>",
		},
		{
			name: "inside main",
			with: "<!DOCTYPE html>\n<html>\n<body>\n<main>\n  " + tag +
				"\n  <article data-page data-rev=\"1\"><p>text</p></article>\n</main>\n</body>\n</html>",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			without := StripPageTag(tc.with)
			if without == tc.with {
				t.Fatalf("StripPageTag left the tag in place: %q", without)
			}
			got := InsertPageTag(without, tag)
			if got != tc.with {
				t.Fatalf("round trip = %q, want %q", got, tc.with)
			}
			if got, ok := PageTag(got); !ok || got != tag {
				t.Fatalf("PageTag = %q, %v", got, ok)
			}
		})
	}
}

func TestPageTagRefusesNonPageDocuments(t *testing.T) {
	tag := pageTagYJS("state-1")
	sources := map[string]string{
		"no article": "<!DOCTYPE html>\n<html>\n<body>\n  <p>text</p>\n</body>\n</html>",
		"two articles": "<!DOCTYPE html>\n<html>\n<body>\n  <article data-page><p>a</p></article>\n" +
			"  <article data-page><p>b</p></article>\n</body>\n</html>",
		"only in template": "<!DOCTYPE html>\n<html>\n<body>\n  <template><article data-page><p>a</p></article></template>\n" +
			"</body>\n</html>",
		"already tagged": pageTagYJS("state-1") + "\n<article data-page><p>a</p></article>",
	}
	for name, source := range sources {
		t.Run(name, func(t *testing.T) {
			if got := InsertPageTag(source, tag); got != source {
				t.Fatalf("InsertPageTag changed a document it should refuse: %q", got)
			}
		})
	}
}

func TestPageTagReadsOnlyAYjsTag(t *testing.T) {
	hybrid := "<!DOCTYPE html>\n<html>\n<body>\n  " +
		`<script type="application/json" merge="page" clay="live">{"blocks":{}}</script>` +
		"\n  <article data-page><p>a</p></article>\n</body>\n</html>"
	if tag, ok := PageTag(hybrid); ok {
		t.Fatalf("PageTag read a hybrid store: %q", tag)
	}
	notPage := "<!DOCTYPE html>\n<html>\n<body>\n  <p>outside</p>\n  " + pageTagYJS("state-1") + "\n</body>\n</html>"
	if tag, ok := PageTag(notPage); ok {
		t.Fatalf("PageTag read a tag out of a non-page document: %q", tag)
	}
	if tag, ok := PageTag(strings.Replace(notPage, pageTagYJS("state-1"), "", 1)); ok {
		t.Fatalf("PageTag read a document with no tag: %q", tag)
	}
}
