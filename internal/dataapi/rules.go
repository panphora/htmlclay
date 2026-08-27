package dataapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// This file is a line-by-line port of hyper-html-api/src/engine/rules.js. It is deliberately not
// idiomatic Go in places: the tokenizer's quirks ARE the contract, so the structure follows the
// JavaScript rather than what a fresh implementation would look like. Every place the port has to
// make a choice JavaScript made implicitly (whitespace class, string indexing, __proto__) is
// commented, because those are the places a plausible-looking rewrite silently diverges.

// ParseStrict parses rules as strict JSON: no unquoted keys, no single quotes, no trailing commas.
// The script-tag face uses ParseRelaxed instead; this exists for callers that want strict
// semantics, matching the JS engine's public export.
func ParseStrict(body string) (Value, error) {
	v, err := decodeJSON(body)
	if err != nil {
		return nil, &RulesParseError{Message: "Invalid strict JSON: " + err.Error(), Cause: err}
	}
	return v, nil
}

// ParseRelaxed parses the ?data= parameter and script-tag bodies. Strict JSON is tried first and
// returned as-is when it works; anything else goes through a tokenizer that rewrites the input
// into JSON and parses that. The tokenizer came from hyperclay's legacy data-extractor.js and its
// behavior on odd input is load-bearing, not incidental — see the rules-* conformance cases.
func ParseRelaxed(queryString string) (Value, error) {
	if v, err := decodeJSON(queryString); err == nil {
		return v, nil
	}

	v, err := decodeJSON(tokensToJSON(tokenize(queryString)))
	if err != nil {
		return nil, &RulesParseError{Message: "Invalid extraction rules syntax: " + err.Error(), Cause: err}
	}
	return v, nil
}

// isJSSpace reports whether r is in JavaScript's \s class, spelled out because no Go helper matches
// it. unicode.IsSpace differs at both ends: it accepts U+0085 (NEL), which \s does not, and rejects
// U+FEFF (BOM), which \s accepts. Measured against node, not read off a table.
func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ',
		0x00a0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

func isASCIILetterOrUnderscore(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

const (
	tokBraceOpen    = "{"
	tokBraceClose   = "}"
	tokBracketOpen  = "["
	tokBracketClose = "]"
	tokColon        = ":"
	tokComma        = ","
	tokString       = "STRING"
	tokSelector     = "SELECTOR"
	tokIdentifier   = "IDENTIFIER"
	tokNumber       = "NUMBER"
	tokBoolean      = "BOOLEAN"
)

type token struct {
	typ         string
	value       string
	quoted      bool
	sourceQuote byte
}

// pseudoSelectors is an allowlist the tokenizer consults to decide whether a ':' starts a pseudo
// class (part of the current selector) or separates a key from its value. ORDER IS SIGNIFICANT and
// must stay exactly as the JS has it: the scan takes the first prefix match, so ":first" wins over
// ":first-child" and ":last" over ":last-child". A tidier alphabetical or longest-first list would
// change how real selectors tokenize.
//
// Anything absent — ":has", ":is", ":where", ":not(" is present but ":scope" is not — terminates
// the token early and generally produces a broken rule rather than an error. That is the
// rules-unwhitelisted-pseudo-breaks-tokenizer case.
var pseudoSelectors = []string{
	":first", ":last", ":nth-child", ":nth-of-type", ":first-child", ":last-child",
	":first-of-type", ":last-of-type", ":only-child", ":only-of-type", ":hover", ":focus",
	":active", ":visited", ":disabled", ":enabled", ":checked", ":empty", ":root", ":target",
	":not", ":before", ":after", ":nth-last-child", ":nth-last-of-type",
}

var numberRe = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`)

// tokenize walks the input by rune while making every structural decision on the raw byte at the
// current offset. That combination is what makes the port faithful: JS indexes UTF-16 code units,
// but every character the tokenizer branches on is ASCII, and ASCII bytes never appear inside a
// multi-byte UTF-8 sequence. So byte comparisons land on exactly the same characters JS sees, and
// only the whitespace test — the one branch with non-ASCII members — needs a decoded rune.
func tokenize(input string) []token {
	var tokens []token
	i := 0

	for i < len(input) {
		r, size := utf8.DecodeRuneInString(input[i:])
		if isJSSpace(r) {
			i += size
			continue
		}

		char := input[i]

		if char == '{' || char == '}' {
			tokens = append(tokens, token{typ: string(char), value: string(char)})
			i++
			continue
		}

		// '[' is ambiguous: it opens a JSON array, or it opens an attribute selector inside a
		// bare selector. The tokenizer decides by peeking past whitespace for a letter or
		// underscore, so `[a-b]` is an attribute selector and `["a"]` is an array.
		if char == '[' {
			isAttributeSelector := false
			j := i + 1
			for j < len(input) {
				r, size := utf8.DecodeRuneInString(input[j:])
				if !isJSSpace(r) {
					break
				}
				j += size
			}
			if j < len(input) && isASCIILetterOrUnderscore(input[j]) {
				isAttributeSelector = true
			}
			if !isAttributeSelector {
				tokens = append(tokens, token{typ: tokBracketOpen, value: "["})
				i++
				continue
			}
			// falls through to the selector scan below
		}

		if char == ']' {
			tokens = append(tokens, token{typ: tokBracketClose, value: "]"})
			i++
			continue
		}

		if char == ':' {
			tokens = append(tokens, token{typ: tokColon, value: ":"})
			i++
			continue
		}

		if char == ',' {
			tokens = append(tokens, token{typ: tokComma, value: ","})
			i++
			continue
		}

		if char == '"' || char == '\'' {
			quote := char
			j := i + 1
			for j < len(input) && input[j] != quote {
				if input[j] == '\\' {
					j++
				}
				j++
			}
			// An unterminated string, or one ending in a backslash, leaves j past the end. JS
			// substring clamps; Go slicing panics, so clamp explicitly.
			tokens = append(tokens, token{
				typ:         tokString,
				value:       input[i+1 : min(j, len(input))],
				quoted:      true,
				sourceQuote: quote,
			})
			i = j + 1
			continue
		}

		// Everything else is a bare run: an identifier, a number, or a selector. It ends at the
		// first '{', '}' or ',' — but ':' and '[' are consulted first, because a selector may
		// legally contain both and the run must not stop inside one.
		j := i
	scan:
		for j < len(input) && input[j] != '{' && input[j] != '}' && input[j] != ',' {
			switch input[j] {
			case ':':
				isPseudoSelector := false
				for _, pseudo := range pseudoSelectors {
					pseudoName := pseudo[1:]
					if strings.HasPrefix(input[j+1:], pseudoName) {
						isPseudoSelector = true
						// Advances by the name length only, not past the colon too. The
						// following iteration re-reads the last character of the name and
						// moves on. Odd, and preserved.
						j += len(pseudoName)
						break
					}
				}
				if !isPseudoSelector {
					break scan
				}
			case '[':
				j++
				for j < len(input) && input[j] != ']' {
					if input[j] == '"' || input[j] == '\'' {
						quote := input[j]
						j++
						for j < len(input) && input[j] != quote {
							if input[j] == '\\' {
								j++
							}
							j++
						}
					}
					j++
				}
				if j < len(input) && input[j] == ']' {
					j++
				}
			default:
				j++
			}
		}
		value := input[i:min(j, len(input))]

		typ := tokIdentifier
		switch {
		case numberRe.MatchString(value):
			typ = tokNumber
		case value == "true" || value == "false" || value == "null":
			typ = tokBoolean
		// The JS is /^[.#@\[]|[.#@\[]| /, whose first alternative is subsumed by its second:
		// net effect is "contains '.', '#', '@', '[' or a literal space". Note the space is a
		// plain space, not \s — a tab does not make a run a selector.
		case strings.ContainsAny(value, ".#@[ "):
			typ = tokSelector
		}

		tokens = append(tokens, token{typ: typ, value: value})
		i = j
	}

	return tokens
}

// tokensToJSON rewrites the token stream as JSON text, which is then handed to a real JSON parser.
// Values are interpolated raw, so a bare run containing a '"' or '\' produces invalid JSON and
// surfaces as a parse error rather than being escaped into something that works. That is the
// contract, not an oversight.
func tokensToJSON(tokens []token) string {
	var b strings.Builder

	for i := 0; i < len(tokens); i++ {
		t := tokens[i]

		switch t.typ {
		case tokBraceOpen, tokBraceClose, tokBracketOpen, tokBracketClose, tokColon:
			b.WriteString(t.value)
			continue

		case tokComma:
			// Drop trailing commas. JSON rejects them; relaxed authors expect them to work.
			if i+1 < len(tokens) {
				if next := tokens[i+1].typ; next == tokBraceClose || next == tokBracketClose {
					continue
				}
			}
			b.WriteString(t.value)
			continue

		case tokNumber, tokBoolean:
			b.WriteString(t.value)
			continue

		case tokString:
			if t.quoted {
				v := t.value
				if t.sourceQuote == '\'' {
					// The source was single-quoted, so \' was escaping the delimiter; once
					// re-wrapped in double quotes it is a plain '. Then any " that is not
					// already escaped has to become \".
					v = strings.ReplaceAll(v, `\'`, `'`)
					v = escapeBareQuotes(v)
				}
				b.WriteString(`"` + v + `"`)
				continue
			}
		}

		b.WriteString(`"` + t.value + `"`)
	}

	return b.String()
}

// escapeBareQuotes is the JS /(\\*)"/g replace: a '"' preceded by an even number of backslashes is
// unescaped and needs one added. The backslash run is attributed to the quote that follows it, and
// scanning resumes after that quote, which is why the counter resets on every other character.
func escapeBareQuotes(v string) string {
	var b strings.Builder
	slashes := 0
	for i := 0; i < len(v); i++ {
		switch c := v[i]; c {
		case '\\':
			slashes++
			b.WriteByte(c)
		case '"':
			if slashes%2 == 0 {
				b.WriteByte('\\')
			}
			b.WriteByte(c)
			slashes = 0
		default:
			b.WriteByte(c)
			slashes = 0
		}
	}
	return b.String()
}

// maxParseDepth stops runaway nesting from overflowing the Go stack. This is a parity guard, not a
// resource limit of the kind the plan rejected: V8's JSON.parse throws RangeError on deep nesting
// and the JS engine turns that into a RulesParseError, so erroring here is what matches. Without
// it a nested-enough ?data= would take down the whole htmlclay process rather than one request.
const maxParseDepth = 512

// decodeJSON parses JSON into the ordered Value tree. encoding/json's Unmarshal cannot be used:
// it lands objects in a Go map, which loses the key order the output depends on.
func decodeJSON(s string) (Value, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()

	v, err := decodeValue(dec, 0)
	if err != nil {
		return nil, err
	}
	// JSON.parse rejects trailing content; a bare json.Decoder would happily read a second value.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("unexpected trailing content")
		}
		return nil, err
	}
	return v, nil
}

func decodeValue(dec *json.Decoder, depth int) (Value, error) {
	if depth > maxParseDepth {
		return nil, fmt.Errorf("rules nested deeper than %d levels", maxParseDepth)
	}

	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}

	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := NewObject()
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("object key is not a string")
				}
				val, err := decodeValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				// Define, not Set: JSON.parse uses CreateDataProperty, which bypasses the
				// __proto__ setter and leaves a real own key. Assignment does not. Measured.
				o.Define(key, val)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return o, nil

		case '[':
			list := []Value{}
			for dec.More() {
				val, err := decodeValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				list = append(list, val)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return list, nil
		}
		return nil, fmt.Errorf("unexpected delimiter %q", t)

	case json.Number:
		// ParseFloat's range error yields ±Inf, which is what JSON.parse produces for an
		// overflowing literal. Treating it as a failure instead would drop ParseRelaxed's
		// strict fast path into the tokenizer and change the result.
		f, err := strconv.ParseFloat(t.String(), 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return nil, err
		}
		return f, nil

	case nil:
		return nil, nil

	case string:
		return t, nil

	case bool:
		return t, nil
	}

	return nil, fmt.Errorf("unexpected token %v", tok)
}
