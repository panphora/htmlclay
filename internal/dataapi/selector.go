package dataapi

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/andybalholm/cascadia"
	"golang.org/x/net/html"
)

// selector.go is the gate. It is the highest-risk file in this change, because it is the only part
// with no JavaScript counterpart to check against: cascadia and cheerio accept overlapping but
// unequal selector languages, and on the overlap they sometimes mean different things without
// either one raising an error.
//
// The gate is an ALLOW-list, not a reject-list. That is a deliberate strengthening of the plan,
// which listed names to reject: a reject-list cannot be complete, because cascadia exposes no
// enumeration of the pseudo names it accepts, so a name added in a future release would pass
// straight through. With an allow-list an unknown name is a 400 by construction, and the
// "gate-completeness test cannot prove future closure" problem stops mattering.
//
// Order of operations, and each step's reason:
//
//	1. scan          classify from text, because escapes and case make raw grep useless
//	2. validate      allow-list every pseudo; reject the measured-divergent constructs by name
//	3. peel          lift terminal :first/:last/:eq/:contains/:checked into post-filters
//	4. fold          append " i" to attribute clauses whose NAME is case-insensitive in HTML
//	5. compile       hand the residue to cascadia; any error is a 400

// allowedPseudos are the names verified to mean the same thing in both engines, each one measured
// against cheerio in testdata/selector-parity.json rather than taken on trust. Anything absent is
// rejected, with a specific message where one is worth giving.
var allowedPseudos = map[string]bool{
	"not": true, "has": true,
	"nth-child": true, "nth-last-child": true, "nth-of-type": true, "nth-last-of-type": true,
	"first-child": true, "last-child": true, "first-of-type": true, "last-of-type": true,
	"only-child": true, "only-of-type": true, "root": true,
	// Handled by a post-filter rather than passed to cascadia. See peelablePseudos.
	"first": true, "last": true, "eq": true, "nth": true, "contains": true, "checked": true,
}

// rejectedPseudos carry a reason, so the 400 tells the author what actually differs instead of
// "unsupported". Every entry was measured during the review.
var rejectedPseudos = map[string]string{
	// Measured: cascadia's :matches(x) matched an <li>x</li> and :matches(^de) matched a div
	// reading "deep", so it is a regex over text. cheerio's :matches(x) matched nothing, because
	// there it is an alias for :is and no element is an <x>. Both returned data; neither errored.
	"matches":     "cascadia reads :matches as a regular-expression match over text while cheerio treats it as :is, and neither errors",
	"matchesown":  "cheerio errors on :matchesOwn; cascadia matches",
	"containsown": "cheerio errors on :containsOwn; cascadia matches",
	"haschild":    "cheerio errors on :haschild; cascadia matches",
	"lang":        "cheerio errors on :lang; cascadia matches and returns text nodes",
	"enabled":     "cheerio reads :enabled as :not(:disabled) on any element; cascadia limits it to form controls plus a/area/link[href]",
	"disabled":    "cascadia inherits disabled from a <fieldset disabled>; cheerio looks only at the element's own attribute",
	"empty":       "cascadia ignores whitespace-only text; cheerio does not",
	"focus":       "cheerio errors on :focus; cascadia returns no matches without erroring",
	"target":      "cheerio errors on :target; cascadia returns no matches without erroring",
	"gt":          "cascadia has no :gt",
	"lt":          "cascadia has no :lt",
	"even":        "cascadia has no :even",
	"odd":         "cascadia has no :odd",
	"is":          "cascadia has no :is",
	"where":       "cascadia has no :where",
	"scope":       "cascadia has no :scope",
	"parent":      "cascadia has no :parent",
	"header":      "cascadia has no :header",
	"selected":    "cascadia has no :selected; use :checked",
	"button":      "cascadia has no :button",
	"submit":      "cascadia has no :submit",
	"text":        "cascadia has no :text",
	"password":    "cascadia has no :password",
	"radio":       "cascadia has no :radio",
	"file":        "cascadia has no :file",
	"image":       "cascadia has no :image",
	"reset":       "cascadia has no :reset",

	// Measured: cheerio's :input matched an <input>; cascadia compiled the same selector without
	// complaint and matched nothing. A silent empty result is the worst failure mode there is.
	"input": "cheerio's :input matches form controls; cascadia accepts the name and matches nothing",

	// These four describe live user state, which no server can observe: both engines always answer
	// nothing. Rejecting turns a rule that could only ever be a silent no-op into a clear error.
	// (:link is rejected for the same reason plus an unverified definition; use [href].)
	"link":    "no server can observe link state; use [href]",
	"visited": "no server can observe visited state, so this could only ever match nothing",
	"hover":   "no server can observe hover state, so this could only ever match nothing",
	"active":  "no server can observe active state, so this could only ever match nothing",
}

// peelablePseudos are implemented here rather than by cascadia, each for its own reason:
//
//	:first :last :eq  cascadia cannot parse them at all
//	:contains         cascadia's is case-INSENSITIVE; css-select's is case-sensitive
//	:checked          cascadia misses the implicit "first option" default (measured)
//
// A post-filter can only describe the LAST compound of a group, so any of these appearing earlier
// in a selector is a 400 rather than a quiet approximation.
var peelablePseudos = map[string]bool{
	"first": true, "last": true, "eq": true, "nth": true, "contains": true, "checked": true,
}

// positionalPseudos are limited to one per comma group; two would be an ordering puzzle with no
// obviously right answer.
var positionalPseudos = map[string]bool{"first": true, "last": true, "eq": true, "nth": true}

type compiledSelector struct {
	source string
	groups []compiledGroup

	// foldedAttrs are the attribute names this selector compares case-insensitively, either by
	// carrying an explicit `i` flag or by being one of the 46 names HTML folds. Collected at compile
	// time so checkFoldedValues has something cheap to test each matched node against.
	foldedAttrs map[string]bool
}

type compiledGroup struct {
	matcher cascadia.Matcher
	filters []nodeFilter
}

type nodeFilter func([]*html.Node) []*html.Node

// compileSelector is the only place a selector string becomes a matcher, and the only caller of
// cascadia.Compile in the package.
func compileSelector(selector string) (*compiledSelector, error) {
	fail := func(reason string) error {
		return &UnsupportedSelector{Selector: selector, Reason: reason}
	}

	// cheerio's .find() short-circuits a falsy selector to an empty set before css-select sees it,
	// so a blank selector is no matches rather than an error. Getting this wrong was a
	// whole-document read: the rule {x:"[]"} strips its own "[]" suffix and arrives here empty.
	if isCSSBlank(selector) {
		return &compiledSelector{}, nil
	}

	// CSS comments cannot be handled, only refused. cascadia's skipWhitespace treats /* */ as
	// whitespace while css-what has no comment syntax at all and parses the text literally, so
	// "div/*x*/p" is "div p" to one engine and the impossible compound "div AND p" to the other.
	// No selector containing one can be a parity match.
	//
	// This runs ahead of the scanner, on its own pass, because a comment can contain the quote or
	// bracket that blinds the scanner, which is a second route to a gate bypass.
	if containsComment(selector) {
		return nil, fail("CSS comments are not supported: cascadia reads /* */ as whitespace and " +
			"cheerio's parser does not accept it at all, so the two engines read a different selector")
	}

	groups := scanSelector(selector)

	// Default-deny on the compound alphabet, for the same reason the pseudo list is an allow-list:
	// a character the scanner does not model is a character the gate would judge without reading.
	// Whitespace is where the two engines disagree most quietly — U+000B, U+0085 and U+00A0 end a
	// selector for css-what and continue an identifier for cascadia — so "\v" answered "no matches"
	// here while cheerio threw. Found by FuzzScanSelector's closure property.
	if i, bad := unmodelledByte(selector, groups); bad {
		return nil, fail(unmodelledReason(selector, i))
	}

	cs := &compiledSelector{source: selector, foldedAttrs: map[string]bool{}}
	for _, g := range groups {
		if err := validateGroup(selector, g, fail); err != nil {
			return nil, err
		}

		for _, a := range g.attrs {
			if a.op != "" && (a.flag == "i" || caseInsensitiveAttrs[a.name]) {
				cs.foldedAttrs[a.name] = true
			}
		}

		residue, filters, err := peelGroup(selector, g, fail)
		if err != nil {
			return nil, err
		}

		matcher, err := cascadia.Compile(residue)
		if err != nil {
			return nil, &SelectorError{Selector: selector, Cause: err}
		}
		cs.groups = append(cs.groups, compiledGroup{matcher: matcher, filters: filters})
	}
	return cs, nil
}

// divergentSpace are the only three characters where the two engines disagree about whether a
// character separates a selector or continues an identifier. Measured one rune at a time against
// cheerio: every other candidate from JS's whitespace set (U+1680, U+180E, U+2000-U+200B, U+2028,
// U+2029, U+202F, U+205F, U+3000, U+FEFF) is an identifier character to BOTH engines and is left
// alone, and the real CSS whitespace set is whitespace to both.
var divergentSpace = map[rune]bool{0x000B: true, 0x0085: true, 0x00A0: true}

// plainCompoundByte is the ASCII alphabet a selector may use OUTSIDE a pseudo or attribute token: the
// combinators, the group comma, and the characters that build a type, class or id selector. It is
// measured rather than assumed — running the residue check over the 111 accepted parity selectors
// yields exactly this set, minus `|`, which is here for the `ns|div` namespace form the corpus has
// no case of. Non-ASCII is handled by the caller, which has to decode runes to test divergentSpace.
//
// Everything else is syntax the scanner does not model.
func plainCompoundByte(c byte) bool {
	switch {
	case isCSSSpace(c), c == ',', c == '>', c == '+', c == '~', c == '*', c == '.', c == '#', c == '|':
		return true
	case c == '-', c == '_':
		return true
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return false
}

// unmodelledByte returns the offset of the first byte of the selector that no token covers and the
// plain-compound alphabet does not admit. It is the gate's closure check: if it finds nothing, every
// byte of the selector was classified, so nothing was judged unread.
func unmodelledByte(selector string, groups []groupScan) (int, bool) {
	covered := make([]bool, len(selector))
	cover := func(start, end int) {
		for i := max(start, 0); i < end && i < len(selector); i++ {
			covered[i] = true
		}
	}
	for _, g := range groups {
		for _, p := range g.pseudos {
			cover(p.start, p.end)
		}
		for _, a := range g.attrs {
			cover(a.start, a.end)
		}
	}

	for i := 0; i < len(selector); {
		if covered[i] {
			i++
			continue
		}
		// An escape makes whatever follows part of an identifier, whatever it is: `\!` is a type
		// selector for an element named "!", which both engines read the same way.
		if selector[i] == '\\' {
			i += escapeLen(selector, i)
			continue
		}
		if selector[i] < utf8.RuneSelf {
			if !plainCompoundByte(selector[i]) {
				return i, true
			}
			i++
			continue
		}
		// Both engines read non-ASCII as identifier text, with three measured exceptions.
		r, size := utf8.DecodeRuneInString(selector[i:])
		if divergentSpace[r] {
			return i, true
		}
		i += size
	}
	return 0, false
}

// unmodelledReason names the three characters worth explaining and describes the rest generically.
func unmodelledReason(selector string, i int) string {
	r, _ := utf8.DecodeRuneInString(selector[i:])
	at := " at offset " + strconv.Itoa(i)
	switch r {
	case 0x000B, 0x0085, 0x00A0:
		return "the whitespace character " + strconv.QuoteRune(r) + at +
			" ends a selector for cheerio's parser and continues an identifier for cascadia, " +
			"so the two engines would read a different selector"
	}
	return "the character " + strconv.QuoteRune(r) + at + " is not part of any selector syntax " +
		"this gate models; it would reach cascadia unclassified"
}

// isCSSBlank reports whether s is empty or holds nothing but CSS whitespace. strings.TrimSpace is
// the wrong tool here and was the original: Go's whitespace set includes U+000B and U+0085, which
// CSS's does not, so the lone vertical tab "\v" took the blank short-circuit above and answered
// "no matches" where cheerio throws Empty sub-selector. FuzzScanSelector's closure property found
// it, which is exactly the class of bug it exists for.
func isCSSBlank(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isCSSSpace(s[i]) {
			return false
		}
	}
	return true
}

func validateGroup(s string, g groupScan, fail func(string) error) error {
	for _, p := range g.pseudos {
		if p.colons == 2 {
			return fail("pseudo-elements like ::" + p.name + " are not supported")
		}
		if reason, bad := rejectedPseudos[p.name]; bad {
			return fail(":" + p.name + " is not supported: " + reason)
		}
		if !allowedPseudos[p.name] {
			return fail(":" + p.name + " is not supported")
		}
		// cascadia cannot parse a :has() argument that begins with a combinator, while cheerio
		// can. The phase-0 survey found zero uses across 5 851 documents.
		if p.name == "has" && p.hasArg {
			arg := strings.TrimLeft(p.arg, " \t\n\r\f")
			if arg != "" && strings.IndexByte(">+~", arg[0]) >= 0 {
				return fail(":has() with a combinator argument is not supported; use a descendant selector")
			}
		}
	}

	for _, a := range g.attrs {
		// An attribute clause the scanner could not read is a REFUSAL, not something to pass along
		// hoping cascadia objects. That hope was false and it was the worst bug in the gate: an
		// unreadable clause sets end = to, which ends the scan for the whole comma group, so every
		// pseudo after it went unclassified — and cascadia accepts constructs the scanner does not,
		// so nothing downstream caught them. `p[a!=b]:matches(x)` reached cascadia's regex :matches.
		//
		// Failing closed here means the NEXT gap in scanAttr is a local refusal rather than a
		// general bypass, which is the only safe default for a component like this.
		if !a.terminated {
			return fail("unreadable attribute clause starting at offset " + strconv.Itoa(a.start-g.start))
		}
		if a.unknownOp {
			return fail("unknown attribute operator in the clause starting at offset " +
				strconv.Itoa(a.start-g.start) + "; the supported operators are =, ~=, |=, ^=, $=, *= and !=")
		}
		switch a.op {
		case "#=":
			return fail("the [attr#=…] regular-expression operator is not supported")
		case "^=", "$=", "*=", "!=", "~=":
			// Measured, each one: an empty value matches nothing in cheerio and nearly everything in
			// cascadia. css-select special-cases it (`not` short-circuits to falseFunc, `element`
			// builds a regex that matches the empty string), cascadia does not.
			if a.value == "" {
				return fail("[" + a.rawName + a.op + `""] means different things in the two engines`)
			}
		}
		if a.flag != "" && a.flag != "i" {
			// `s` is the standard CSS case-sensitivity flag and cheerio honours it, but cascadia's
			// parser takes only i/I. Rejecting it by name gives the author a reason instead of the
			// syntax error cascadia would raise three lines later for syntax that is actually fine.
			if a.flag == "s" {
				return fail("the [attr=value s] case-sensitivity flag is not supported: cascadia " +
					"accepts only the i flag; omit it, since matching is case-sensitive by default")
			}
			return fail("unknown attribute flag " + strconv.Quote(a.flag))
		}
		// Case-insensitive comparison is not the same relation in both languages: cheerio lowercases
		// while cascadia uses EqualFold, so <div type="ς"> matches [type=Σ] in one and not the other.
		// Only ASCII values are safe to compare case-insensitively.
		if a.op != "" && (a.flag == "i" || (a.flag == "" && caseInsensitiveAttrs[a.name])) && !isASCII(a.value) {
			return fail("case-insensitive matching on the non-ASCII value " + strconv.Quote(a.value) +
				" is not supported; the two engines fold case differently")
		}
	}
	return nil
}

// peelGroup lifts the trailing peelable pseudos off a group and returns the residue selector plus
// the filters to apply, in source order.
func peelGroup(s string, g groupScan, fail func(string) error) (string, []nodeFilter, error) {
	// cut is where the residue ends, and it is deliberately NOT trimmed: the whitespace between the
	// residue and the first peeled token is the evidence that the pseudo formed its own compound.
	// Trimming it here is what made "#solo :first" collapse to "#solo" and return the wrong node.
	cut := g.end
	var peeled []pseudoToken

	// Peel only WITHIN one compound. The end of the group may be preceded by whitespace, but once
	// the first pseudo is peeled every further peel must be immediately adjacent, because a
	// post-filter can only ever describe the last compound.
	//
	// Skipping whitespace between peels meant "li:first :contains(x)" lifted BOTH pseudos off two
	// different compounds and applied them to the residue "li", returning the li instead of its
	// descendant. Stopping here lets the non-terminal check below refuse it, which is the locked
	// behavior for a positional that is not last.
	for first := true; ; first = false {
		ce := cut
		if first {
			ce = trimEndCSSSpace(s, g.start, cut)
		}
		var found *pseudoToken
		for i := range g.pseudos {
			p := &g.pseudos[i]
			if p.depth == 0 && p.end == ce && peelablePseudos[p.name] {
				found = p
				break
			}
		}
		if found == nil {
			break
		}
		peeled = append(peeled, *found)
		cut = found.start
	}

	// Anything peelable left in the residue was not in the terminal position.
	positionals := 0
	for _, p := range peeled {
		if positionalPseudos[p.name] {
			positionals++
		}
	}
	if positionals > 1 {
		return "", nil, fail("only one positional pseudo (:first, :last, :eq, :nth) is allowed per comma group")
	}
	for _, p := range g.pseudos {
		if p.end <= cut && peelablePseudos[p.name] {
			return "", nil, fail(":" + p.name + " is only supported at the end of a selector")
		}
	}

	residue := s[g.start:cut]
	trimmed := strings.TrimRight(residue, " \t\n\r\f")

	// A peeled pseudo that formed its own compound has to leave a universal selector behind, or
	// "li :first" would silently become "li" and match the wrong elements.
	needStar := len(peeled) > 0 &&
		(trimmed == "" || strings.IndexByte(">+~", trimmed[len(trimmed)-1]) >= 0 || trimmed != residue)

	// Attribute case folding, applied right to left so earlier offsets stay valid.
	var b strings.Builder
	b.WriteString(residue)
	out := b.String()
	for i := len(g.attrs) - 1; i >= 0; i-- {
		a := g.attrs[i]
		if a.end > cut || a.op == "" || a.flag != "" || !caseInsensitiveAttrs[a.name] {
			continue
		}
		at := a.closeAt - g.start
		if at < 0 || at > len(out) {
			continue
		}
		out = out[:at] + " i" + out[at:]
	}

	if needStar {
		out = strings.TrimRight(out, " \t\n\r\f") + " *"
	}

	// An empty group is a syntax error, never a universal selector. This used to fall back to "*",
	// which was reachable ONLY in the case it was wrong for — a real peel already emits " *" via
	// needStar — so ".row," and ",p" matched every element in the document where cheerio throws
	// Empty sub-selector. Blank whole selectors were short-circuited by the caller before this ran.
	if isCSSBlank(out) {
		return "", nil, fail("empty selector group")
	}

	filters := make([]nodeFilter, 0, len(peeled))
	// peeled is right-to-left; filters apply left-to-right, the order jQuery would.
	for i := len(peeled) - 1; i >= 0; i-- {
		f, err := filterFor(peeled[i], fail)
		if err != nil {
			return "", nil, err
		}
		filters = append(filters, f)
	}
	return out, filters, nil
}

func filterFor(p pseudoToken, fail func(string) error) (nodeFilter, error) {
	switch p.name {
	case "first":
		return func(n []*html.Node) []*html.Node {
			if len(n) == 0 {
				return nil
			}
			return n[:1]
		}, nil

	case "last":
		return func(n []*html.Node) []*html.Node {
			if len(n) == 0 {
				return nil
			}
			return n[len(n)-1:]
		}, nil

	case "eq", "nth":
		// cheerio-select parses the index with parseInt, which takes a leading integer and ignores
		// the rest, so :eq(1.9) and :eq(1abc) are index 1 and :eq() is NaN. strconv.Atoi rejected
		// all three, turning a working selector into a 400.
		idx, ok := parseIntPrefix(p.arg)
		return func(n []*html.Node) []*html.Node {
			if !ok {
				return nil // NaN selects nothing rather than erroring, matching cheerio
			}
			// cheerio-select's rule is abs(idx) < len, NOT jQuery's idx += len. The difference is
			// exactly one index: it makes position 0 unreachable from the negative side, so
			// :eq(-1) on a single match is EMPTY there and was the first element here.
			if idx <= -len(n) || idx >= len(n) {
				return nil
			}
			i := idx
			if i < 0 {
				i += len(n)
			}
			return n[i : i+1]
		}, nil

	case "contains":
		want := unquoteArg(p.arg)
		return func(n []*html.Node) []*html.Node {
			out := n[:0:0]
			for _, node := range n {
				// css-select compares against the untrimmed text, case-SENSITIVELY.
				if strings.Contains(textContent(node), want) {
					out = append(out, node)
				}
			}
			return out
		}, nil

	case "checked":
		return func(n []*html.Node) []*html.Node {
			out := n[:0:0]
			for _, node := range n {
				if isChecked(node) {
					out = append(out, node)
				}
			}
			return out
		}, nil
	}
	return nil, fail(":" + p.name + " has no filter")
}

// isChecked is css-select's alias, translated literally:
//
//	:checked  = :is(:is(input[type=radio], input[type=checkbox])[checked], option:selected)
//	:selected = option:is([selected],
//	              select:not([multiple]):not(:has(> option[selected])) > :first-of-type)
//
// cascadia implements only the explicit halves, so a <select> whose options carry no `selected`
// attribute yields nothing there and its first option here. Measured, both ways.
func isChecked(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}

	switch n.Data {
	case "input":
		// [type=radio] folds case, because "type" is one of the 46 case-insensitive names.
		t, _ := attrValue(n, "type")
		if t := lowerASCII(t); t != "radio" && t != "checkbox" {
			return false
		}
		_, checked := attrValue(n, "checked")
		return checked

	case "option":
		if _, selected := attrValue(n, "selected"); selected {
			return true
		}
		parent := n.Parent
		if parent == nil || parent.Type != html.ElementNode || parent.Data != "select" {
			return false
		}
		if _, multiple := attrValue(parent, "multiple"); multiple {
			return false
		}
		// :not(:has(> option[selected])) — any DIRECT option child being selected cancels the
		// default, which is why an <optgroup> wrapper produces no default at all.
		for c := parent.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode && c.Data == "option" {
				if _, selected := attrValue(c, "selected"); selected {
					return false
				}
			}
		}
		// > :first-of-type
		for c := parent.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode && c.Data == n.Data {
				return c == n
			}
		}
	}
	return false
}

// match runs every comma group, applies that group's filters, and merges the results in document
// order without duplicates. Groups are filtered independently because that is what cheerio does:
// "a:first, b:first" is two firsts, not the first of the union.
func (cs *compiledSelector) match(ctx *html.Node) ([]*html.Node, error) {
	if len(cs.groups) == 1 {
		g := cs.groups[0]
		nodes := elementsOnly(cascadia.QueryAll(ctx, g.matcher))
		for _, f := range g.filters {
			nodes = f(nodes)
		}
		if err := cs.checkFoldedValues(nodes); err != nil {
			return nil, err
		}
		return nodes, nil
	}

	selected := map[*html.Node]bool{}
	for _, g := range cs.groups {
		nodes := elementsOnly(cascadia.QueryAll(ctx, g.matcher))
		for _, f := range g.filters {
			nodes = f(nodes)
		}
		if err := cs.checkFoldedValues(nodes); err != nil {
			return nil, err
		}
		for _, n := range nodes {
			selected[n] = true
		}
	}
	if len(selected) == 0 {
		return nil, nil
	}

	var out []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if selected[c] {
				out = append(out, c)
			}
			walk(c)
		}
	}
	walk(ctx)
	return out, nil
}

// checkFoldedValues refuses a match whose document-side attribute value is non-ASCII on an
// attribute this selector compares case-insensitively.
//
// The compile-time guard only ever saw the SELECTOR's value, and that is the wrong half. cascadia
// folds with EqualFold (Unicode simple folding) while css-select folds with JavaScript's
// toLowerCase, and the two disagree on exactly the characters neither side writes: `[type^=i]`
// matches <div type="İ"> here and nothing in cheerio, and `[type=s]` matches <div type="ſ">.
// cascadia's relation is the broader one, so the divergence always shows up as an EXTRA node,
// which is why inspecting the nodes that matched is enough to catch it.
//
// This is why match returns an error at all. cascadia's Matcher is a bool, so a per-clause check
// cannot live inside matching; the honest alternative to this post-check was reimplementing
// attribute matching, which is far more code for an input nobody sends.
func (cs *compiledSelector) checkFoldedValues(nodes []*html.Node) error {
	if len(cs.foldedAttrs) == 0 {
		return nil
	}
	for _, n := range nodes {
		for _, a := range n.Attr {
			if !cs.foldedAttrs[a.Key] || isASCII(a.Val) {
				continue
			}
			return &UnsupportedSelector{
				Selector: cs.source,
				Reason: "the document's " + a.Key + "=" + strconv.Quote(a.Val) +
					" is non-ASCII and this selector compares " + a.Key + " case-insensitively; " +
					"the two engines fold case differently, so the result would be unreliable",
			}
		}
	}
	return nil
}

// elementsOnly drops the non-element nodes cascadia can return — :lang reaches text nodes — since
// cheerio's result set is elements only.
func elementsOnly(nodes []*html.Node) []*html.Node {
	out := nodes[:0:0]
	for _, n := range nodes {
		if n.Type == html.ElementNode {
			out = append(out, n)
		}
	}
	return out
}

// unquoteArg strips the quotes from a pseudo argument if it has them, and otherwise hands the raw
// text through UNTRIMMED. The whitespace is significant: css-what does not trim, so
// `p:contains( foo )` searches for " foo " with the spaces, and trimming here made it match every
// element containing "foo".
func unquoteArg(arg string) string {
	a := strings.Trim(arg, " \t\n\r\f")
	if len(a) >= 2 && (a[0] == '"' || a[0] == '\'') && a[len(a)-1] == a[0] {
		return unescapeIdent(a[1 : len(a)-1])
	}
	return unescapeIdent(arg)
}

// containsComment reports whether the selector has a real CSS comment, as opposed to the two
// characters "/*" appearing inside a string or behind an escape.
//
// It has to be its own pass rather than a case in scanInto, because a comment can hide the quote
// that would blind the scanner. It also cannot be a plain strings.Contains: "/*" is legal inside a
// quoted attribute value, and refusing [id="/*"] would be a false alarm on a selector cheerio runs
// happily.
//
// Both directions come out right because each position is tested for a comment BEFORE it is tested
// for a quote, exactly as CSS tokenization does it. In div/*'*/span the comment opens first, so the
// stray quote inside it is never treated as a string; in [id="/*"] the quote opens first, so the
// "/*" inside it is skipped as content.
func containsComment(s string) bool {
	for i := 0; i < len(s); {
		switch {
		case s[i] == '\\':
			i += escapeLen(s, i)
		case s[i] == '/' && i+1 < len(s) && s[i+1] == '*':
			return true
		case s[i] == '"' || s[i] == '\'':
			i = skipString(s, i, len(s))
		default:
			i++
		}
	}
	return false
}

// parseIntPrefix is JavaScript's parseInt, restricted to the base-10 forms a selector index can
// take: optional sign, then digits, then anything, which is ignored. Returns ok=false for NaN.
func parseIntPrefix(s string) (int, bool) {
	i := 0
	for i < len(s) && isCSSSpace(s[i]) {
		i++
	}
	start := i
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	digits := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == digits {
		return 0, false
	}
	n, err := strconv.Atoi(s[start:i])
	if err != nil {
		return 0, false
	}
	return n, true
}

func trimEndCSSSpace(s string, from, to int) int {
	for to > from && isCSSSpace(s[to-1]) {
		to--
	}
	return to
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// caseInsensitiveAttrs is css-select's caseInsensitiveAttributes, copied exactly
// (css-select/lib/attributes.js). It is consulted by EVERY attribute matcher, not just equality,
// so all six operators fold when the NAME is in this set.
//
// Note href is NOT here — hreflang is. So [href=X] is case-sensitive in both engines and needs no
// rewrite, which is the opposite of what the plan's phase-0 note claimed.
var caseInsensitiveAttrs = map[string]bool{
	"accept": true, "accept-charset": true, "align": true, "alink": true, "axis": true,
	"bgcolor": true, "charset": true, "checked": true, "clear": true, "codetype": true,
	"color": true, "compact": true, "declare": true, "defer": true, "dir": true,
	"direction": true, "disabled": true, "enctype": true, "face": true, "frame": true,
	"hreflang": true, "http-equiv": true, "lang": true, "language": true, "link": true,
	"media": true, "method": true, "multiple": true, "nohref": true, "noresize": true,
	"noshade": true, "nowrap": true, "readonly": true, "rel": true, "rev": true,
	"rules": true, "scope": true, "scrolling": true, "selected": true, "shape": true,
	"target": true, "text": true, "type": true, "valign": true, "valuetype": true,
	"vlink": true,
}
