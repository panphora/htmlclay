package dataapi

import (
	"encoding/json"
	"os"
	"testing"
)

// TestDifferential is the Go half of scripts/differential.mjs. It is driven by that script, which
// always sets HTMLCLAY_DIFFERENTIAL, so the skip below fires only in CI — where parity coverage
// comes from the vendored conformance corpus instead.
//
// The skip message says so out loud on purpose. A cross-language test that quietly skips when node
// is missing is the most expensive kind of green: it reads as parity coverage in every CI summary
// while checking nothing at all.
func TestDifferential(t *testing.T) {
	path := os.Getenv("HTMLCLAY_DIFFERENTIAL")
	if path == "" {
		t.Skip("no case file: this is the development differential, driven by " +
			"scripts/differential.mjs. It is NOT parity coverage for CI — the conformance corpus is.")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cases: %v", err)
	}

	var cases []struct {
		HTML      string          `json:"html"`
		Rules     json.RawMessage `json:"rules"`
		CheerioOK bool            `json:"cheerioOK"`
		Cheerio   json.RawMessage `json:"cheerio"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("parse cases: %v", err)
	}

	var agreed, divergent, invented, mismatched int
	for i, c := range cases {
		rules, err := ParseStrict(string(c.Rules))
		if err != nil {
			t.Fatalf("case %d: the generator emitted rules this engine cannot parse: %v", i, err)
		}

		doc, err := ParseBytes([]byte(c.HTML))
		if err != nil {
			t.Fatalf("case %d: parse document: %v", i, err)
		}

		got, err := doc.Extract(rules)

		switch {
		case err != nil && !c.CheerioOK:
			// Both refused. The messages differ by design; agreeing on refusal is the claim.
			agreed++

		case err != nil && c.CheerioOK:
			// The gate refusing a construct cheerio answers is the divergence it exists to create.
			// Counted, not failed, so a change that widens the set is visible.
			divergent++

		case err == nil && !c.CheerioOK:
			// Answering where the reference errors means inventing data, which is never acceptable.
			invented++
			t.Errorf("DIFFERENTIAL case %d: cheerio errored, this port answered\nrules: %s\nhtml: %s",
				i, c.Rules, c.HTML)

		default:
			mine, mErr := Marshal(got)
			if mErr != nil {
				t.Fatalf("case %d: marshal: %v", i, mErr)
			}
			want, wErr := canonicalJSON(c.Cheerio)
			if wErr != nil {
				t.Fatalf("case %d: canonicalise cheerio answer: %v", i, wErr)
			}
			if string(mine) != want {
				mismatched++
				t.Errorf("DIFFERENTIAL case %d: both answered, differently\nrules: %s\nhtml: %s\n  go: %s\ncheerio: %s",
					i, c.Rules, c.HTML, mine, want)
			} else {
				agreed++
			}
		}
	}

	t.Logf("DIFFERENTIAL %d cases: %d agreed, %d deliberate divergences, %d invented, %d mismatched",
		len(cases), agreed, divergent, invented, mismatched)
}

// canonicalJSON re-encodes cheerio's answer through the same writer the Go side uses, so the
// comparison is about the DATA rather than about key spacing or escaping conventions.
func canonicalJSON(raw json.RawMessage) (string, error) {
	v, err := decodeJSON(string(raw))
	if err != nil {
		return "", err
	}
	out, err := Marshal(v)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
