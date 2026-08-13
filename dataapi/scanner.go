package dataapi

import (
	"strings"
	"unicode/utf8"
)

// scanner.go classifies a selector from its TEXT, before cascadia compiles it. Two measured facts
// force that order:
//
//  1. Both parsers unescape CSS escapes in pseudo names and then diverge silently. `div:\6d atches(.a)`
//     gets 1 match in cheerio, where :matches is an alias for :is, and 2 in cascadia, where :matches
//     is a REGEX match — with no error on either side. `div:CONTAINS(…)` slips a case-folded name
//     past a name list, since cascadia lower-cases via toLowerASCII. A gate that greps the raw text
//     for ":matches" misses both.
//  2. cascadia's compiled output is opaque: Parse returns a Sel interface over unexported types, so
//     the result cannot be walked and asked which constructs it used.
//
// So the classification has to happen here, on text that has been unescaped and case-folded the
// same way both parsers do it. This is where a bug becomes a silent bypass rather than a loud
// error, which is why FuzzScanSelector exists.

type pseudoToken struct {
	name    string // unescaped and ASCII-lower-cased: what both parsers will actually match on
	rawName string // as written, for error messages
	colons  int    // 1 for :name, 2 for ::name
	arg     string // raw text between the parentheses
	hasArg  bool
	start   int // offset of the ':' in the whole selector
	end     int // offset just past the token, past ')' when it has an argument
	depth   int // 0 at the top level of a comma group, deeper inside a pseudo's argument
}

type attrToken struct {
	name    string // unescaped and ASCII-lower-cased
	rawName string
	op      string // "", "=", "~=", "|=", "^=", "$=", "*=", "#="
	value   string // unescaped
	flag    string // "", "i", "s" (lower-cased)
	start   int    // offset of '['
	closeAt int    // offset of ']'
	end     int    // offset just past ']'
	depth   int

	// terminated is false for a clause with no closing ']', which means the fields above are
	// salvage rather than fact. The gate must not judge such a clause on meaning — `a[href*="` has
	// an "empty value" only because the text ran out — it has to fall through to cascadia and be
	// reported as the syntax error it is.
	terminated bool

	// unknownOp marks the same problem in the middle of a clause rather than at its end: a byte
	// where an operator belongs that the scanner could not name. It is the only place inside a
	// bracket clause where a byte is skipped without being assigned meaning, which is what makes
	// the closure property in FuzzScanSelector total.
	unknownOp bool
}

// groupScan is one top-level comma group. Groups matter because positionals are peeled per group
// and the results merged, which is how cheerio behaves.
type groupScan struct {
	start, end int // bounds of the group's text within the whole selector
	pseudos    []pseudoToken
	attrs      []attrToken
}

// selectorArgPseudos take another selector as their argument, so the scan recurses into them.
// Anything not listed here has its argument treated as opaque text — `:nth-child(2n+1)` is not a
// selector and must not be scanned as one.
var selectorArgPseudos = map[string]bool{
	"not": true, "has": true, "is": true, "where": true, "matches": true,
	"any": true, "-moz-any": true, "-webkit-any": true, "haschild": true,
}

// scanSelector splits the selector into top-level comma groups and classifies each.
func scanSelector(s string) []groupScan {
	var groups []groupScan
	for _, span := range splitTopLevelCommas(s) {
		g := groupScan{start: span[0], end: span[1]}
		scanInto(s, span[0], span[1], 0, &g)
		groups = append(groups, g)
	}
	return groups
}

// scanInto walks s[from:to] recording pseudos and attribute clauses. Offsets recorded are absolute
// within the whole selector so the rewriter can splice by them.
func scanInto(s string, from, to, depth int, g *groupScan) {
	i := from
	for i < to {
		switch c := s[i]; c {
		case '\\':
			i += escapeLen(s, i)

		case '"', '\'':
			i = skipString(s, i, to)

		case '[':
			tok, next := scanAttr(s, i, to, depth)
			g.attrs = append(g.attrs, tok)
			i = next

		case ':':
			tok, next := scanPseudo(s, i, to, depth)
			g.pseudos = append(g.pseudos, tok)
			// Recurse only where the argument really is a selector. The recursion is what
			// catches a rejected name hidden inside :not(:matches(…)).
			if tok.hasArg && selectorArgPseudos[tok.name] {
				argStart := tok.start + tok.colons + len(tok.rawName) + 1
				scanInto(s, argStart, argStart+len(tok.arg), depth+1, g)
			}
			i = next

		default:
			i++
		}
	}
}

// escapeLen returns the length of the CSS escape beginning at s[i], which is a backslash.
//
// The rule is worth spelling out because it is where a naive scanner loses: a backslash followed by
// hex digits consumes up to SIX of them plus one optional trailing whitespace, so `\6d ` is four
// bytes and means "m". A backslash followed by anything else escapes that one character.
func escapeLen(s string, i int) int {
	if i+1 >= len(s) {
		return 1
	}
	if !isHexDigit(s[i+1]) {
		_, size := utf8.DecodeRuneInString(s[i+1:])
		return 1 + size
	}
	j := i + 1
	for j < len(s) && j-i <= 6 && isHexDigit(s[j]) {
		j++
	}
	// One whitespace character after the hex run is part of the escape, not of the identifier.
	if j < len(s) && isCSSSpace(s[j]) {
		j++
	}
	return j - i
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// isCSSSpace is CSS's whitespace, which is not JS's: no U+00A0, no U+FEFF.
func isCSSSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f'
}

// skipString returns the offset just past the closing quote, or `to` if unterminated.
func skipString(s string, i, to int) int {
	quote := s[i]
	j := i + 1
	for j < to {
		if s[j] == '\\' {
			j += escapeLen(s, j)
			continue
		}
		if s[j] == quote {
			return j + 1
		}
		j++
	}
	return to
}

// unescapeIdent applies CSS escape rules, so the scanner classifies the name the parsers will
// actually see rather than the name as typed.
func unescapeIdent(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			i++
			continue
		}
		if i+1 >= len(s) {
			// A trailing backslash is U+FFFD per the CSS spec.
			b.WriteRune(utf8.RuneError)
			break
		}
		if !isHexDigit(s[i+1]) {
			r, size := utf8.DecodeRuneInString(s[i+1:])
			b.WriteRune(r)
			i += 1 + size
			continue
		}
		j := i + 1
		var cp rune
		for j < len(s) && j-i <= 6 && isHexDigit(s[j]) {
			cp = cp*16 + rune(hexVal(s[j]))
			j++
		}
		if j < len(s) && isCSSSpace(s[j]) {
			j++
		}
		if cp == 0 || cp > utf8.MaxRune || (cp >= 0xD800 && cp <= 0xDFFF) {
			cp = utf8.RuneError
		}
		b.WriteRune(cp)
		i = j
	}
	return b.String()
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	default:
		return int(c-'A') + 10
	}
}

// lowerASCII folds only A-Z, which is what cascadia's toLowerASCII does. Using strings.ToLower
// would fold Kelvin sign and dotted capital I as well and classify names the parsers never see.
func lowerASCII(s string) string {
	hasUpper := false
	for i := 0; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			hasUpper = true
			break
		}
	}
	if !hasUpper {
		return s
	}
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

// isIdentByte reports whether c can continue an identifier. Non-ASCII bytes always can; escapes are
// handled by the caller.
func isIdentByte(c byte) bool {
	return c == '-' || c == '_' || c >= 0x80 ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// readIdent returns the raw identifier text starting at i and the offset just past it.
func readIdent(s string, i, to int) (string, int) {
	start := i
	for i < to {
		if s[i] == '\\' {
			i += escapeLen(s, i)
			continue
		}
		if !isIdentByte(s[i]) {
			break
		}
		i++
	}
	return s[start:i], i
}

func scanPseudo(s string, i, to, depth int) (pseudoToken, int) {
	tok := pseudoToken{start: i, colons: 1, depth: depth}
	j := i + 1
	if j < to && s[j] == ':' {
		tok.colons = 2
		j++
	}

	raw, next := readIdent(s, j, to)
	tok.rawName = raw
	tok.name = lowerASCII(unescapeIdent(raw))
	j = next

	if j < to && s[j] == '(' {
		close := matchParen(s, j, to)
		tok.hasArg = true
		tok.arg = s[j+1 : min(close, to)]
		if close < to {
			j = close + 1
		} else {
			j = to
		}
	}
	tok.end = j
	return tok, j
}

// matchParen returns the offset of the ')' closing the '(' at i, or `to` if unterminated.
func matchParen(s string, i, to int) int {
	depth := 0
	for j := i; j < to; {
		switch s[j] {
		case '\\':
			j += escapeLen(s, j)
			continue
		case '"', '\'':
			j = skipString(s, j, to)
			continue
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return j
			}
		}
		j++
	}
	return to
}

func scanAttr(s string, i, to, depth int) (attrToken, int) {
	tok := attrToken{start: i, depth: depth}
	j := skipCSSSpace(s, i+1, to)

	raw, next := readIdent(s, j, to)
	j = next
	// [ns|attr] — a '|' not followed by '=' is a namespace separator, so what was just read was
	// the namespace and the real name follows.
	if j < to && s[j] == '|' && (j+1 >= to || s[j+1] != '=') {
		j++
		raw, j = readIdent(s, j, to)
	}
	tok.rawName = raw
	tok.name = lowerASCII(unescapeIdent(raw))

	j = skipCSSSpace(s, j, to)
	if j < to && s[j] != ']' {
		// Operators are one or two bytes; the two-byte forms all end in '='. `!` belongs here even
		// though the gate has no special handling for `!=`: both parsers accept it, so leaving it
		// out did not reject it, it made the clause unreadable and silenced the rest of the group.
		if j+1 < to && s[j+1] == '=' && strings.IndexByte("~|^$*#!", s[j]) >= 0 {
			tok.op = s[j : j+2]
			j += 2
		} else if s[j] == '=' {
			tok.op = "="
			j++
		} else {
			// A byte where an operator belongs that we cannot name. Everything read after it is a
			// guess about a clause we did not understand, so the clause is marked rather than
			// judged: `[a%b]` otherwise reaches validateGroup looking like the presence check `[a]`,
			// because the skip happens to land on ']'. Leaving it for cascadia to reject is the
			// assumption that produced every bug in the phase-4 review, and it is only true until
			// the day cascadia accepts something css-what does not.
			tok.unknownOp = true
			j++
		}

		j = skipCSSSpace(s, j, to)
		if j < to && (s[j] == '"' || s[j] == '\'') {
			end := skipString(s, j, to)
			inner := s[j+1 : max(j+1, end-1)]
			tok.value = unescapeIdent(inner)
			j = end
		} else {
			var v string
			v, j = readIdent(s, j, to)
			tok.value = unescapeIdent(v)
		}

		j = skipCSSSpace(s, j, to)
		if j < to && s[j] != ']' {
			flag, next := readIdent(s, j, to)
			tok.flag = lowerASCII(unescapeIdent(flag))
			j = skipCSSSpace(s, next, to)
		}
	}

	if j < to && s[j] == ']' {
		tok.terminated = true
		tok.closeAt = j
		tok.end = j + 1
	} else {
		// Unterminated. Record the bounds we have; cascadia will reject it.
		tok.closeAt = to
		tok.end = to
	}
	return tok, tok.end
}

func skipCSSSpace(s string, i, to int) int {
	for i < to && isCSSSpace(s[i]) {
		i++
	}
	return i
}

// splitTopLevelCommas returns the bounds of each comma group, ignoring commas inside brackets,
// parentheses and strings — so `:not(a, b)` and `[x="a,b"]` stay whole.
func splitTopLevelCommas(s string) [][2]int {
	var out [][2]int
	depth, start := 0, 0
	for i := 0; i < len(s); {
		switch c := s[i]; c {
		case '\\':
			i += escapeLen(s, i)
			continue
		case '"', '\'':
			i = skipString(s, i, len(s))
			continue
		case '(', '[':
			depth++
		case ')', ']':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				out = append(out, [2]int{start, i})
				start = i + 1
			}
		}
		i++
	}
	return append(out, [2]int{start, len(s)})
}
