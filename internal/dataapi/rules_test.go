package dataapi

import (
	"errors"
	"strings"
	"testing"
)

// Every `want` below was measured by running the same input through the JS parseRelaxed, not
// derived from reading the code. Where the two disagree the JS is right by definition.
func TestParseRelaxed(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "strict json fast path", input: `{"a":"h1"}`, want: `{"a":"h1"}`},
		{name: "unquoted key and value", input: `{a:h1}`, want: `{"a":"h1"}`},
		{name: "top-level number", input: `5`, want: `5`},
		{name: "top-level null", input: `null`, want: `null`},
		{name: "empty object", input: `{}`, want: `{}`},
		{name: "primitives survive", input: `{a: 5, b: -1.5, c: true, d: null}`, want: `{"a":5,"b":-1.5,"c":true,"d":null}`},
		{name: "trailing comma in object", input: `{a:1,}`, want: `{"a":1}`},
		{name: "trailing comma in array", input: `{a:[1,2,]}`, want: `{"a":[1,2]}`},
		{name: "quoted array stays an array", input: `{a:["x","y"]}`, want: `{"a":["x","y"]}`},
		{name: "universal selector", input: `{a:*}`, want: `{"a":"*"}`},
		{name: "attribute shorthand", input: `{a:@href}`, want: `{"a":"@href"}`},

		// Whitespace is skipped BEFORE a token and never trimmed from one, so it clings to the
		// right-hand end of whatever run is being read. Both keys and values are affected.
		{name: "whitespace clings", input: `{x:.i, y : .i }`, want: `{"x":".i","y ":".i "}`},
		{name: "trailing space in value", input: `{a:h1 }`, want: `{"a":"h1 "}`},
		{name: "space before comma clings", input: `{a:h1 , b:h2}`, want: `{"a":"h1 ","b":"h2"}`},

		// Pseudo-classes on the allowlist keep the run going. Order in the list decides which
		// prefix matches, but since the scan continues either way the visible result is the same.
		{name: "pseudo first", input: `{a:h1:first}`, want: `{"a":"h1:first"}`},
		{name: "pseudo first-child matches :first prefix", input: `{a:h1:first-child}`, want: `{"a":"h1:first-child"}`},
		{name: "pseudo last-child matches :last prefix", input: `{a:h1:last-child}`, want: `{"a":"h1:last-child"}`},
		{name: "pseudo with argument", input: `{a:h1:nth-child(2)}`, want: `{"a":"h1:nth-child(2)"}`},
		{name: "pseudo not", input: `{a:h1:not(.x)}`, want: `{"a":"h1:not(.x)"}`},
		// ":is" is not on the list, so the run stops at the colon and the rest becomes garbage.
		{name: "unlisted pseudo breaks the run", input: `{a:h1:is(.a)}`, wantErr: true},

		// '[' is an array only when the next non-space character is not a letter or underscore.
		// This is why a bracket holding a bare selector silently collapses into a string.
		{name: "bracket with letters is an attribute selector", input: `{a:[b,c]}`, want: `{"a":"[b,c]"}`},
		{name: "bracket with attribute name", input: `{a:[href]}`, want: `{"a":"[href]"}`},
		// The lookahead decides on 'f' of "form", so the whole thing — braces, commas and all —
		// is swallowed as one attribute selector. It parses fine and fails later at extraction.
		{name: "list-of-object with unquoted head collapses to a string", input: `{a:[form,{b:@action}]}`, want: `{"a":"[form,{b:@action}]"}`},

		// A bare run is interpolated into JSON raw, so any '"' or control character it picked up
		// produces invalid JSON. Quoting the selector is what makes these work.
		{name: "unquoted selector with quoted attribute", input: `{a:div[data-x="1"] span}`, wantErr: true},
		{name: "unquoted attribute containing @", input: `{a:a[href*="@x"]}`, wantErr: true},
		{name: "literal tab inside a run", input: "{a:h1\there}", wantErr: true},
		{name: "missing value", input: `{a:}`, wantErr: true},
		{name: "unterminated string", input: `{a:'unterminated}`, wantErr: true},

		// Single-quoted strings are re-wrapped in double quotes, so \' becomes a plain ' and any
		// bare " has to be escaped on the way. A backslash run is counted to decide "bare".
		{name: "single-quoted with escaped quote", input: `{a:'it\'s'}`, want: `{"a":"it's"}`},
		{name: "single-quoted containing double quotes", input: `{a:'say "hi"'}`, want: `{"a":"say \"hi\""}`},
		{name: "single-quoted with escaped backslash then quote", input: `{a:'a\\"b'}`, want: `{"a":"a\\\"b"}`},
		{name: "mixed quoting", input: `{a:'x','b':'y'}`, want: `{"a":"x","b":"y"}`},

		// __proto__ survives parsing as a real own key, in source order. It disappears later, at
		// extraction, which is a different mechanism in a different file.
		{name: "proto key is kept by the parser", input: `{__proto__:h1,ok:h1}`, want: `{"__proto__":"h1","ok":"h1"}`},
		{name: "duplicate keys keep first position last value", input: `{a:1,a:2}`, want: `{"a":2}`},

		// Whitespace only matters at a token boundary. Inside a run every one of these is just a
		// character, including the ones JS calls \s.
		{name: "U+FEFF inside a run is kept", input: "{a:h1\ufeff}", want: "{\"a\":\"h1\ufeff\"}"},
		{name: "U+0085 is not JS whitespace", input: "{a:h1\u0085}", want: "{\"a\":\"h1\u0085\"}"},
		{name: "U+200B is not JS whitespace", input: "{a:h1\u200b}", want: "{\"a\":\"h1\u200b\"}"},
		{name: "U+3000 inside a run is kept", input: "{a:h1\u3000}", want: "{\"a\":\"h1\u3000\"}"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseRelaxed(c.input)

			if c.wantErr {
				if err == nil {
					t.Fatalf("ParseRelaxed(%q) succeeded with %s, want an error", c.input, marshal(t, got))
				}
				var rpe *RulesParseError
				if !errors.As(err, &rpe) {
					t.Fatalf("error is %T, want *RulesParseError", err)
				}
				if !strings.HasPrefix(rpe.Message, "Invalid extraction rules syntax: ") {
					t.Errorf("message %q lost its prefix", rpe.Message)
				}
				return
			}

			if err != nil {
				t.Fatalf("ParseRelaxed(%q) = %v", c.input, err)
			}
			if got := marshal(t, got); got != c.want {
				t.Errorf("ParseRelaxed(%q)\n got %s\nwant %s", c.input, got, c.want)
			}
		})
	}
}

// Leading whitespace IS skipped, and the class that decides has to be JS's, not Go's. Go's
// unicode.IsSpace would skip U+0085 and stop skipping U+FEFF, which is wrong in both directions.
func TestLeadingWhitespaceClass(t *testing.T) {
	skipped := []struct {
		name string
		char string
	}{
		{"space", " "},
		{"tab", "\t"},
		{"newline", "\n"},
		{"vertical tab", "\v"},
		{"form feed", "\f"},
		{"carriage return", "\r"},
		{"no-break space", "\u00a0"},
		{"ogham space mark", "\u1680"},
		{"en quad", "\u2000"},
		{"hair space", "\u200a"},
		{"line separator", "\u2028"},
		{"paragraph separator", "\u2029"},
		{"narrow no-break space", "\u202f"},
		{"medium mathematical space", "\u205f"},
		{"ideographic space", "\u3000"},
		{"byte order mark", "\ufeff"},
	}
	for _, c := range skipped {
		t.Run("skipped/"+c.name, func(t *testing.T) {
			// The character sits where whitespace would be skipped: before the value token. If
			// it were not skipped it would cling to the front of the value and show up here.
			got, err := ParseRelaxed("{a:" + c.char + "h1}")
			if err != nil {
				t.Fatalf("ParseRelaxed: %v", err)
			}
			if got := marshal(t, got); got != `{"a":"h1"}` {
				t.Errorf("leading %s was not skipped: %s", c.name, got)
			}
		})
	}

	// The two JS does NOT treat as whitespace. They cling to the value instead.
	notSkipped := []struct {
		name string
		char string
	}{
		{"next line U+0085", "\u0085"},
		{"zero width space U+200B", "\u200b"},
	}
	for _, c := range notSkipped {
		t.Run("kept/"+c.name, func(t *testing.T) {
			got, err := ParseRelaxed("{a:" + c.char + "h1}")
			if err != nil {
				t.Fatalf("ParseRelaxed: %v", err)
			}
			want := "{\"a\":\"" + c.char + "h1\"}"
			if got := marshal(t, got); got != want {
				t.Errorf("%s should have clung to the value: got %s, want %s", c.name, got, want)
			}
		})
	}
}

func TestParseStrict(t *testing.T) {
	if got, err := ParseStrict(`{"a":"h1"}`); err != nil {
		t.Fatalf("ParseStrict: %v", err)
	} else if got := marshal(t, got); got != `{"a":"h1"}` {
		t.Errorf("ParseStrict = %s", got)
	}

	// The relaxed forms are exactly what strict has to reject.
	for _, input := range []string{`{a:h1}`, `{"a":"h1",}`, `{'a':'h1'}`, ``, `{`} {
		_, err := ParseStrict(input)
		if err == nil {
			t.Errorf("ParseStrict(%q) succeeded, want an error", input)
			continue
		}
		var rpe *RulesParseError
		if !errors.As(err, &rpe) {
			t.Errorf("ParseStrict(%q) error is %T, want *RulesParseError", input, err)
		} else if !strings.HasPrefix(rpe.Message, "Invalid strict JSON: ") {
			t.Errorf("message %q lost its prefix", rpe.Message)
		}
	}
}

// JSON.parse rejects trailing content and so must this. A bare json.Decoder would read the first
// value and stop, quietly accepting `{"a":"h1"} garbage`.
func TestTrailingContentIsRejected(t *testing.T) {
	for _, input := range []string{`{"a":"h1"} {"b":"h2"}`, `{"a":"h1"} garbage`, `5 6`} {
		if _, err := decodeJSON(input); err == nil {
			t.Errorf("decodeJSON(%q) succeeded, want an error", input)
		}
	}
}

// Deep nesting must come back as an error, the way V8's RangeError does. Without the guard this
// recurses until the Go stack dies, which takes the whole app down rather than one request.
func TestDeepNestingErrorsRatherThanCrashing(t *testing.T) {
	deep := strings.Repeat("[", maxParseDepth+50) + strings.Repeat("]", maxParseDepth+50)
	if _, err := ParseStrict(deep); err == nil {
		t.Error("deeply nested rules parsed, want an error")
	}
	// Nesting just under the cap still has to work.
	ok := strings.Repeat("[", 100) + strings.Repeat("]", 100)
	if _, err := ParseStrict(ok); err != nil {
		t.Errorf("100 levels should parse: %v", err)
	}
}

// FuzzParseRelaxed exists for one specific failure: the tokenizer advances by hand, and a run that
// neither consumes a character nor terminates is an infinite loop, not a wrong answer. It also
// covers panics from the several places the port slices by index.
func FuzzParseRelaxed(f *testing.F) {
	seeds := []string{
		`{"a":"h1"}`, `{a:h1}`, `{x:.i, y : .i }`, `{a:[form,{b:@action}]}`, `{a:'it\'s'}`,
		`{a:h1:nth-child(2)}`, `{a:[href]}`, `{a:}`, `{`, `[`, `:`, `,`, `'`, `"`, `\`,
		`{a:'unterminated}`, `{a:h1\`, `{__proto__:1}`, "{a:h1\ufeff}", `{a:a[href*="@x"]}`,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, input string) {
		v, err := ParseRelaxed(input)
		if err != nil {
			return
		}
		// Whatever came back must survive being written out, since that is what the server does
		// with it next.
		if _, err := Marshal(v); err != nil {
			t.Fatalf("parsed %q but could not marshal it: %v", input, err)
		}
	})
}
