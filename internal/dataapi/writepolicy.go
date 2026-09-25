package dataapi

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// writePolicyJSON is the policy table itself, vendored from the reference's conformance corpus by
// `make sync-conformance` rather than restated here. The reasons it produces are pinned in the
// corpus's .error.json files, so a table that drifts from the reference is a test failure rather
// than a slow divergence in what a page is allowed to write.
//
//go:embed write-policy.json
var writePolicyJSON []byte

// WritePolicy is the content-only policy: the rule forms, targets, attributes and URL schemes a
// write may not touch.
type WritePolicy struct {
	RefusedRuleForms         []string `json:"refusedRuleForms"`
	RefusedTargets           []string `json:"refusedTargets"`
	RefusedAttributes        []string `json:"refusedAttributes"`
	RefusedAttributePrefixes []string `json:"refusedAttributePrefixes"`
	URLAttributes            []string `json:"urlAttributes"`
	RefusedSchemes           []string `json:"refusedSchemes"`
}

var writePolicy = loadWritePolicy()

func loadWritePolicy() *WritePolicy {
	var p WritePolicy
	if err := json.Unmarshal(writePolicyJSON, &p); err != nil {
		panic("dataapi: write-policy.json is not valid JSON: " + err.Error())
	}
	return &p
}

var (
	// urlSchemeEdges is /^[\u0000- ]+|[\u0000- ]+$/g: control characters and spaces around the
	// value are ignored, which is how "  JaVaScRiPt:" is still refused.
	urlSchemeEdges = regexp.MustCompile(`^[\x00-\x20]+|[\x00-\x20]+$`)
	// urlSchemeInner drops the tab/newline/carriage-return a browser also strips.
	urlSchemeInner = regexp.MustCompile(`[\t\n\r]`)
	urlSchemeShape = regexp.MustCompile(`^([a-zA-Z][a-zA-Z0-9+.-]*):`)
)

// urlScheme is the scheme a URL attribute value names, lowercased, or "" when it names none.
func urlScheme(value string) string {
	cleaned := urlSchemeInner.ReplaceAllString(urlSchemeEdges.ReplaceAllString(value, ""), "")
	m := urlSchemeShape.FindStringSubmatch(cleaned)
	if m == nil {
		return ""
	}
	return strings.ToLower(m[1])
}

// checkAttribute returns the reason a write of name (with a non-null value) is refused, or "" when
// it is allowed.
func checkAttribute(name string, value Value, policy *WritePolicy) string {
	lower := strings.ToLower(name)
	for _, p := range policy.RefusedAttributePrefixes {
		if strings.HasPrefix(lower, p) {
			return fmt.Sprintf("attribute %q can run script", lower)
		}
	}
	if containsString(policy.RefusedAttributes, lower) {
		return fmt.Sprintf("attribute %q is not content", lower)
	}
	if containsString(policy.URLAttributes, lower) && value != nil {
		urls := []string{jsString(value)}
		if lower == "srcset" {
			urls = urls[:0]
			for _, candidate := range strings.Split(jsString(value), ",") {
				if part := firstJSToken(jsTrim(candidate)); part != "" {
					urls = append(urls, part)
				}
			}
		}
		for _, url := range urls {
			if scheme := urlScheme(url); scheme != "" && containsString(policy.RefusedSchemes, scheme) {
				return fmt.Sprintf("%q URLs are not allowed in %q", scheme+":", lower)
			}
		}
	}
	return ""
}

// firstJSToken is String.split(/\s+/)[0], which the srcset parser needs rather than
// strings.Fields: \s and unicode.IsSpace disagree at U+0085 and U+FEFF, the same two ends
// isJSSpace exists for.
func firstJSToken(s string) string {
	start := -1
	for i, r := range s {
		if isJSSpace(r) {
			if start >= 0 {
				return s[start:i]
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		return s[start:]
	}
	return ""
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
