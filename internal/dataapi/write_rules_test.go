package dataapi

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestWriteDocumentWithRulesRejectsNonRules pins the gate on the caller's rules value. The reference
// tests `typeof rules` for string or object, so null, a boolean and a number are all refused before
// the document is consulted at all. The document below has a valid rules tag and the body would
// otherwise write, so a write that happened here would be exactly the regression this covers.
func TestWriteDocumentWithRulesRejectsNonRules(t *testing.T) {
	src := `<script data-rules-name="api" data-rules-version="1">{t:"h1"}</script><h1>Hi</h1>`
	data, err := decodeJSON(`{"t":"Yo"}`)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		rules Value
	}{
		{"nil", nil},
		{"bool", true},
		{"number", json.Number("42")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result, err := WriteDocumentWithRules([]byte(src), data, c.rules)
			var rpe *RulesParseError
			if !errors.As(err, &rpe) {
				t.Fatalf("WriteDocumentWithRules(%v) error = %v, want *RulesParseError", c.rules, err)
			}
			if result != nil {
				t.Errorf("result = %+v, want nil", result)
			}
			want := "Invalid extraction rules: expected a selector, array, or object."
			if got := rpe.Error(); got != want {
				t.Errorf("message = %q, want %q", got, want)
			}
		})
	}
}

// TestWriteDocumentWithRulesBOM pins the caller-rules face against the same BOM handling the tag
// face has: the write runs on the BOM-less body, a no-op returns the original src with its mark, and
// a changed write splices the body and puts the mark back on the output.
func TestWriteDocumentWithRulesBOM(t *testing.T) {
	src := "\xEF\xBB\xBF<!DOCTYPE html><html lang=en><head><meta charset=utf-8>" +
		"<title>T</title></head><body>\r\n<h1>Hi</h1>\r\n<p class='x'>&eacute;</p></body></html>"
	rules, err := ParseRelaxed(`{t:"h1"}`)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("no-op", func(t *testing.T) {
		data, err := decodeJSON(`{"t":"Hi"}`)
		if err != nil {
			t.Fatal(err)
		}
		res, err := WriteDocumentWithRules([]byte(src), data, rules)
		if err != nil {
			t.Fatalf("WriteDocumentWithRules: %v", err)
		}
		if res.Changed {
			t.Errorf("Changed = true, want false")
		}
		if !res.Spliced {
			t.Errorf("Spliced = false, want true")
		}
		if string(res.HTML) != src {
			t.Errorf("bytes:\n got: %q\nwant: %q", res.HTML, src)
		}
	})

	t.Run("changed", func(t *testing.T) {
		data, err := decodeJSON(`{"t":"Yo"}`)
		if err != nil {
			t.Fatal(err)
		}
		res, err := WriteDocumentWithRules([]byte(src), data, rules)
		if err != nil {
			t.Fatalf("WriteDocumentWithRules: %v", err)
		}
		if !res.Changed {
			t.Errorf("Changed = false, want true")
		}
		if !res.Spliced {
			t.Errorf("Spliced = false, want true")
		}
		if !strings.HasPrefix(string(res.HTML), "\xEF\xBB\xBF") {
			t.Errorf("output lost the BOM: %q", res.HTML)
		}
		want := strings.Replace(src, "<h1>Hi</h1>", "<h1>Yo</h1>", 1)
		if string(res.HTML) != want {
			t.Errorf("bytes:\n got: %q\nwant: %q", res.HTML, want)
		}
	})
}
