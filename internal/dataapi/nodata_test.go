package dataapi

import (
	"testing"
)

// TestNoDataRegions pins the read-path half of the region capabilities: a no-data element is not
// data, so find() drops it, text reads past it, and @innerHTML renders without it. Every case is
// the port's answer to a measured one from the reference, whose chevron is capabilitySelector.
func TestNoDataRegions(t *testing.T) {
	const htmlDoc = `<p id="x">Hi <span no-data>secret</span>there</p>`
	const clayDoc = `<p id="x">Hi <span clay="no-data">secret</span>there</p>`

	cases := []struct{ name, html, rules, want string }{
		{
			"bare attribute hides the list item",
			`<ul><li editor-ui>Add</li><li>A</li><li>B</li></ul>`,
			`{items:"li[]"}`,
			`{"items":["A","B"]}`,
		},
		{
			"text reads past a no-data span",
			htmlDoc,
			`{t:"#x"}`,
			`{"t":"Hi there"}`,
		},
		{
			"clay token hides the same span",
			clayDoc,
			`{t:"#x"}`,
			`{"t":"Hi there"}`,
		},
		{
			"innerHTML renders without the no-data span",
			htmlDoc,
			`{h:"#x@innerHTML"}`,
			`{"h":"Hi there"}`,
		},
		{
			"an ancestor hides the subtree from a descendant selector",
			`<section editor-ui><p>Tools</p></section><p id="o">Out</p>`,
			`{p:"section p", o:"#o"}`,
			`{"p":null,"o":"Out"}`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rules, err := ParseRelaxed(c.rules)
			if err != nil {
				t.Fatalf("ParseRelaxed(%q): %v", c.rules, err)
			}
			d, err := ParseBytes([]byte(c.html))
			if err != nil {
				t.Fatalf("ParseBytes: %v", err)
			}
			got, err := d.Extract(rules)
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			if s := marshal(t, got); s != c.want {
				t.Errorf("%s = %s, want %s", c.rules, s, c.want)
			}
		})
	}
}
