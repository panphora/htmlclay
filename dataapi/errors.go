package dataapi

import (
	"fmt"
	"strconv"
	"strings"
)

// RulesParseError is returned when rules cannot be parsed, from either face. It mirrors the JS
// engine's error of the same name.
//
// The message text does NOT match the JS engine's, and cannot: it wraps Go's encoding/json
// diagnostics rather than V8's. That is deliberate and safe, because htmlclay maps errors to HTTP
// status by TYPE. The JS hosts map by sniffing message text, which is exactly why two sibling
// selector failures answer 400 and 500 there. See the error-parity table in the plan.
type RulesParseError struct {
	Message string
	Cause   error
}

func (e *RulesParseError) Error() string { return e.Message }

func (e *RulesParseError) Unwrap() error { return e.Cause }

// SelectorFailure is implemented by both selector errors, so a caller mapping HTTP status can
// answer 400 for either without knowing which. The two stay distinct types because they mean
// different things to a human reading the log: one selector is broken, the other is fine but
// ambiguous across engines.
type SelectorFailure interface {
	error
	selectorFailure()
}

func (e *SelectorError) selectorFailure()       {}
func (e *UnsupportedSelector) selectorFailure() {}

// SelectorError is a selector cascadia would not compile.
//
// This is the clearest place htmlclay departs from the JS hosts on STATUS rather than on data. Of
// the sixteen message classes css-what can produce, twelve carry no word the JS hosts' sniffing
// recognises, so those answer 500 while their siblings answer 400. Two failures of the same kind
// getting different statuses is not a contract worth reproducing; every selector failure is 400
// here. Recorded as a divergence, not parity.
type SelectorError struct {
	Selector string
	Cause    error
}

func (e *SelectorError) Error() string {
	return "invalid selector " + strconv.Quote(e.Selector) + ": " + e.Cause.Error()
}

func (e *SelectorError) Unwrap() error { return e.Cause }

// UnsupportedSelector is the gate's refusal: a selector cheerio would accept and answer, which
// htmlclay declines because the two engines do not agree on what it means.
//
// It is deliberately NOT the same type as SelectorError, even though both answer 400. A
// SelectorError is a selector nobody can run. An UnsupportedSelector is a divergence made loud on
// purpose: the reference produces data and htmlclay produces an error, which is the safe direction
// to differ but is still a difference, and it should be countable rather than blended into the
// generic parse failures. Reason says what actually differs, since "unsupported" alone leaves the
// author with nothing to act on.
type UnsupportedSelector struct {
	Selector string
	Reason   string
}

func (e *UnsupportedSelector) Error() string {
	return "unsupported selector " + strconv.Quote(e.Selector) + ": " + e.Reason
}

// MaxRuleDepthExceeded mirrors the JS error of the same name, message included, because the path
// it names is genuinely useful for finding the offending branch of a large rule tree.
type MaxRuleDepthExceeded struct {
	Path []string
}

func (e *MaxRuleDepthExceeded) Error() string {
	return fmt.Sprintf("rule depth exceeded %d at path: %s", maxRuleDepth, strings.Join(e.Path, "."))
}

// UnknownRulesVersion is a rules tag whose data-rules-version is not "1". A missing attribute
// reports as empty, matching the JS, where undefined !== "1" fails the same way.
type UnknownRulesVersion struct {
	Version string
}

func (e *UnknownRulesVersion) Error() string {
	return fmt.Sprintf("unknown rules version: %s. Library supports %q.", e.Version, SupportedRulesVersion)
}

// InvalidRulesToken is a rules-tag token that failed validation before reaching the selector.
type InvalidRulesToken struct {
	Token string
}

func (e *InvalidRulesToken) Error() string {
	return "invalid rules token " + strconv.Quote(e.Token) + " (must match " + rulesToken.String() + ")"
}
