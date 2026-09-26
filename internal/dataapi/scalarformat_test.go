package dataapi

import "testing"

// TestScalarFormatting pins String(value) for the scalar types a JSON body can carry. A number is
// not Go's default formatting: 1e21 is "1e+21" (positional notation only below it) and 1.5 keeps
// its fraction, so a body that writes one has to produce the reference's text exactly.
func TestScalarFormatting(t *testing.T) {
	src := []byte(`<script data-rules-name="api" data-rules-version="1">{t:"#t"}</script><p id=t>x</p>`)

	cases := []struct {
		data string
		want string
	}{
		{`{"t":1.5}`, `{"t":"1.5"}`},
		{`{"t":100}`, `{"t":"100"}`},
		{`{"t":1e21}`, `{"t":"1e+21"}`},
		{`{"t":true}`, `{"t":"true"}`},
	}

	for _, c := range cases {
		t.Run(c.data, func(t *testing.T) {
			data, err := decodeJSON(c.data)
			if err != nil {
				t.Fatalf("decodeJSON: %v", err)
			}
			result, err := WriteDocument(src, data, "api")
			if err != nil {
				t.Fatalf("WriteDocument: %v", err)
			}
			if !result.Changed {
				t.Fatalf("WriteDocument wrote nothing")
			}

			d, err := ParseBytes(result.HTML)
			if err != nil {
				t.Fatalf("reparse written output: %v", err)
			}
			found, err := d.FindRulesIn("api")
			if err != nil || found == nil {
				t.Fatalf("written output has no rules tag: %v", err)
			}
			got, err := d.Extract(found.Rules)
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			assertSameJSON(t, c.data, got, c.want)
		})
	}
}
