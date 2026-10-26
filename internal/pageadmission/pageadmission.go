// Package pageadmission checks page store changes before a document is written.
package pageadmission

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/panphora/htmlclay/internal/dataapi"
	"golang.org/x/net/html"
)

var (
	pageName = regexp.MustCompile(`^page(\.[0-9]+)?$`)
	liveName = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)
	partName = regexp.MustCompile(`^(.+)\.(\d+)\.(\d+)$`)
	baseName = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Tag is one eligible page store in document order.
type Tag struct {
	Name  string
	Value dataapi.Value
}

// Result describes the page stores found in an HTML document.
type Result struct {
	Kind   string
	Reason string
	Tags   []Tag
}

// Decision identifies the admitted kind of write.
type Decision string

const (
	Ordinary   Decision = "ordinary"
	Conversion Decision = "conversion"
	Initial    Decision = "initial"
)

// Refusal carries a hosted page conversion reason.
type Refusal struct{ Reason string }

func (e *Refusal) Error() string { return "Page conversion refused: " + e.Reason }

// FormatError means an existing converted page would lose its complete Yjs value.
type FormatError struct{}

func (*FormatError) Error() string { return "A converted page must retain one complete Yjs page value" }

type rawTag struct {
	name      string
	text      string
	ephemeral bool
}

func attribute(n *html.Node, name string) (string, bool) {
	for _, attr := range n.Attr {
		if attr.Key == name {
			return attr.Val, true
		}
	}
	return "", false
}

func eligibleTags(source string) []rawTag {
	root, err := html.Parse(strings.NewReader(source))
	if err != nil {
		return nil
	}
	var tags []rawTag
	var walk func(*html.Node, bool)
	walk = func(node *html.Node, inert bool) {
		if node.Type == html.ElementNode {
			inert = inert || node.Data == "template" || node.Data == "noscript"
			if !inert && node.Data == "script" {
				if _, external := attribute(node, "src"); !external {
					name, hasMerge := attribute(node, "merge")
					clay, _ := attribute(node, "clay")
					kind, _ := attribute(node, "type")
					kind = strings.ToLower(strings.TrimSpace(strings.SplitN(kind, ";", 2)[0]))
					if hasMerge && liveName.MatchString(name) && pageName.MatchString(name) &&
						containsToken(clay, "live") && (kind == "application/json" || strings.HasSuffix(kind, "+json")) {
						var body strings.Builder
						for child := node.FirstChild; child != nil; child = child.NextSibling {
							if child.Type == html.TextNode {
								body.WriteString(child.Data)
							}
						}
						tags = append(tags, rawTag{name, body.String(), containsToken(clay, "ephemeral")})
					}
				}
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child, inert)
		}
	}
	walk(root, false)
	return tags
}

func containsToken(source, token string) bool {
	for _, part := range strings.Fields(source) {
		if part == token {
			return true
		}
	}
	return false
}

func hasMerge(source string) bool {
	for i := 0; i+5 <= len(source); i++ {
		if source[i]|0x20 == 'm' && source[i+1]|0x20 == 'e' && source[i+2]|0x20 == 'r' &&
			source[i+3]|0x20 == 'g' && source[i+4]|0x20 == 'e' {
			return true
		}
	}
	return false
}

func record(v dataapi.Value) (*dataapi.Object, bool) {
	o, ok := v.(*dataapi.Object)
	return o, ok
}

func hybridValue(v dataapi.Value) bool {
	o, ok := record(v)
	if !ok {
		return false
	}
	blocks, exists := o.Get("blocks")
	if !exists {
		return false
	}
	if _, ok := record(blocks); !ok {
		return false
	}
	if deleted, exists := o.Get("deleted"); exists && deleted != nil {
		if _, ok := record(deleted); !ok {
			return false
		}
	}
	for _, key := range o.Keys() {
		if key != "blocks" && key != "deleted" {
			return false
		}
	}
	return true
}

func yPageValue(v dataapi.Value, complete bool) bool {
	o, ok := record(v)
	if !ok {
		return false
	}
	version, _ := o.Get("v")
	if version != float64(1) {
		return false
	}
	parts, ok := recordValue(o, "y")
	if !ok {
		return false
	}
	for _, key := range o.Keys() {
		if key != "v" && key != "g" && key != "y" {
			return false
		}
	}
	type group struct {
		count int64
		seen  map[int64]bool
	}
	whole := map[string]bool{}
	groups := map[string]*group{}
	for _, key := range parts.Keys() {
		value, _ := parts.Get(key)
		if _, ok := value.(string); !ok {
			return false
		}
		match := partName.FindStringSubmatch(key)
		if match == nil {
			whole[key] = true
			continue
		}
		index, iErr := strconv.ParseInt(match[2], 10, 64)
		count, cErr := strconv.ParseInt(match[3], 10, 64)
		if iErr != nil || cErr != nil || index > 9007199254740991 || count > 9007199254740991 || count < 1 || index >= count {
			return false
		}
		g := groups[match[1]]
		if g == nil {
			g = &group{count: count, seen: map[int64]bool{}}
			groups[match[1]] = g
		}
		if g.count != count || g.seen[index] {
			return false
		}
		g.seen[index] = true
	}
	for name, group := range groups {
		if whole[name] || (complete && int64(len(group.seen)) != group.count) {
			return false
		}
	}
	return true
}

func recordValue(o *dataapi.Object, name string) (*dataapi.Object, bool) {
	v, exists := o.Get(name)
	if !exists {
		return nil, false
	}
	return record(v)
}

// Inspect classifies the eligible page stores in source.
func Inspect(source string) Result {
	if !hasMerge(source) {
		return Result{Kind: "none"}
	}
	raw := eligibleTags(source)
	if len(raw) == 0 {
		return Result{Kind: "none"}
	}
	seen := map[string]bool{}
	tags := make([]Tag, 0, len(raw))
	for _, tag := range raw {
		if seen[tag.name] {
			return Result{Kind: "invalid", Reason: "duplicate-tag"}
		}
		seen[tag.name] = true
		if tag.ephemeral {
			return Result{Kind: "invalid", Reason: "ephemeral-tag"}
		}
		value, err := parsePageJSON(tag.text)
		if err != nil {
			return Result{Kind: "invalid", Reason: "invalid-json"}
		}
		tags = append(tags, Tag{tag.name, value})
	}
	claimed := false
	for _, tag := range tags {
		if o, ok := record(tag.Value); ok {
			_, hasY := o.Get("y")
			_, hasV := o.Get("v")
			claimed = claimed || hasY || hasV
		}
	}
	if claimed {
		if len(tags) != 1 || tags[0].Name != "page" || !yPageValue(tags[0].Value, true) {
			return Result{Kind: "invalid", Reason: "invalid-yjs", Tags: tags}
		}
		return Result{Kind: "yjs", Tags: tags}
	}
	for _, tag := range tags {
		if !hybridValue(tag.Value) {
			return Result{Kind: "other", Tags: tags}
		}
	}
	return Result{Kind: "hybrid", Tags: tags}
}

func claimsYPage(source string) bool {
	if !hasMerge(source) {
		return false
	}
	for _, tag := range eligibleTags(source) {
		value, err := parsePageJSON(tag.text)
		if err != nil {
			continue
		}
		o, ok := record(value)
		if !ok {
			continue
		}
		version, _ := o.Get("v")
		if _, hasY := recordValue(o, "y"); version == float64(1) && hasY {
			return true
		}
	}
	return false
}

// StripPageTag removes the page editor's merge state, the one Yjs page tag,
// from a page document, and the whole line when the tag stands alone on it.
// Any other input comes back unchanged.
func StripPageTag(source string) string {
	if Inspect(source).Kind != "yjs" {
		return source
	}
	start, end, articles := locatePage(source)
	if start < 0 || articles != 1 {
		return source
	}
	from := strings.LastIndexByte(source[:start], '\n') + 1
	to := len(source)
	if nl := strings.IndexByte(source[end:], '\n'); nl >= 0 {
		to = end + nl + 1
	}
	if strings.TrimSpace(source[from:start]) == "" && strings.TrimSpace(source[end:to]) == "" {
		start, end = from, to
	}
	return source[:start] + source[end:]
}

// pageDocument reports whether source is a page document: exactly one
// article[data-page] outside template and noscript.
func pageDocument(source string) bool {
	_, _, articles := locatePage(source)
	return articles == 1
}

func tokenAttributes(z *html.Tokenizer) (string, map[string]string) {
	name, _ := z.TagName()
	attrs := map[string]string{}
	for {
		key, value, more := z.TagAttr()
		attrs[string(key)] = string(value)
		if !more {
			break
		}
	}
	return string(name), attrs
}

func pageScript(attrs map[string]string) bool {
	merge, ok := attrs["merge"]
	if !ok || merge != "page" {
		return false
	}
	if _, external := attrs["src"]; external {
		return false
	}
	kind := strings.ToLower(strings.TrimSpace(strings.SplitN(attrs["type"], ";", 2)[0]))
	return containsToken(attrs["clay"], "live") && (kind == "application/json" || strings.HasSuffix(kind, "+json"))
}

// locatePage finds the byte span of the one Yjs page tag and counts the
// article[data-page] elements outside template and noscript.
func locatePage(source string) (start, end, articles int) {
	start = -1
	z := html.NewTokenizer(strings.NewReader(source))
	offset, article := 0, 0
	var inertTags []string
	matched := 0
	inside, pending := false, false
	for {
		token := z.Next()
		at := offset
		offset += len(z.Raw())
		switch token {
		case html.ErrorToken:
			if matched != 1 || inside || end == 0 {
				return -1, 0, articles
			}
			return start, end, articles
		case html.StartTagToken, html.SelfClosingTagToken:
			name, attrs := tokenAttributes(z)
			switch name {
			case "template", "noscript":
				if token == html.StartTagToken {
					inertTags = append(inertTags, name)
				}
			case "article":
				if token == html.StartTagToken {
					if len(inertTags) == 0 {
						if _, ok := attrs["data-page"]; ok {
							articles++
						}
					}
					article++
				}
			case "script":
				if token == html.StartTagToken && len(inertTags) == 0 && !pending && pageScript(attrs) {
					matched++
					start = at
					inside = article > 0
					pending = true
				}
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			switch string(name) {
			case "template", "noscript":
				for i := len(inertTags) - 1; i >= 0; i-- {
					if inertTags[i] == string(name) {
						inertTags = inertTags[:i]
						break
					}
				}
			case "article":
				if article > 0 {
					article--
				}
			case "script":
				if pending {
					end = offset
					pending = false
				}
			}
		}
	}
}

func utf16Less(a, b string) bool {
	left, right := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(left) && i < len(right); i++ {
		if left[i] != right[i] {
			return left[i] < right[i]
		}
	}
	return len(left) < len(right)
}

func canonicalString(source string) []byte {
	var out bytes.Buffer
	out.WriteByte('"')
	for _, r := range source {
		switch r {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			if r < 0x20 {
				out.WriteString("\\u00")
				out.WriteString(hex.EncodeToString([]byte{byte(r)}))
			} else {
				out.WriteRune(r)
			}
		}
	}
	out.WriteByte('"')
	return out.Bytes()
}

func canonical(v dataapi.Value) ([]byte, error) {
	switch item := v.(type) {
	case *dataapi.Object:
		keys := item.Keys()
		sort.Slice(keys, func(i, j int) bool { return utf16Less(keys[i], keys[j]) })
		var out bytes.Buffer
		out.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				out.WriteByte(',')
			}
			out.Write(canonicalString(key))
			out.WriteByte(':')
			value, _ := item.Get(key)
			encoded, err := canonical(value)
			if err != nil {
				return nil, err
			}
			out.Write(encoded)
		}
		out.WriteByte('}')
		return out.Bytes(), nil
	case []dataapi.Value:
		var out bytes.Buffer
		out.WriteByte('[')
		for i, value := range item {
			if i > 0 {
				out.WriteByte(',')
			}
			encoded, err := canonical(value)
			if err != nil {
				return nil, err
			}
			out.Write(encoded)
		}
		out.WriteByte(']')
		return out.Bytes(), nil
	case string:
		return canonicalString(item), nil
	case float64:
		if item == 0 {
			return []byte("0"), nil
		}
		if math.IsInf(item, 0) || math.IsNaN(item) {
			return []byte("null"), nil
		}
		return dataapi.Marshal(item)
	default:
		return dataapi.Marshal(v)
	}
}

// Baseline hashes named hybrid stores using the hosted canonical JSON order.
func Baseline(tags []Tag) (string, error) {
	if len(tags) == 0 {
		return "", errors.New("expected named hybrid page tags")
	}
	seen := map[string]bool{}
	var out bytes.Buffer
	out.WriteByte('[')
	for i, tag := range tags {
		if !pageName.MatchString(tag.Name) || seen[tag.Name] || !hybridValue(tag.Value) {
			return "", errors.New("expected unique hybrid page tags")
		}
		seen[tag.Name] = true
		if i > 0 {
			out.WriteByte(',')
		}
		name, err := dataapi.Marshal(tag.Name)
		if err != nil {
			return "", err
		}
		value, err := canonical(tag.Value)
		if err != nil {
			return "", err
		}
		out.WriteByte('[')
		out.Write(name)
		out.WriteByte(',')
		out.Write(value)
		out.WriteByte(']')
	}
	out.WriteByte(']')
	sum := sha256.Sum256(out.Bytes())
	return hex.EncodeToString(sum[:]), nil
}

func conversionBase(header string) (string, error) {
	parts := strings.Split(header, ";")
	if strings.TrimSpace(parts[0]) != "v1" {
		return "", &Refusal{Reason: "invalid-header"}
	}
	params := map[string]string{}
	for _, field := range parts[1:] {
		pair := strings.Split(field, "=")
		if len(pair) != 2 {
			return "", &Refusal{Reason: "invalid-header"}
		}
		name, value := strings.TrimSpace(pair[0]), strings.TrimSpace(pair[1])
		if value == "" || (name != "source" && name != "base") || params[name] != "" {
			return "", &Refusal{Reason: "invalid-header"}
		}
		params[name] = value
	}
	if params["source"] != "file" || !baseName.MatchString(params["base"]) {
		return "", &Refusal{Reason: "invalid-header"}
	}
	return params["base"], nil
}

// CheckWrite applies the file-side page format and conversion rules.
// Initial and conversion decisions also require an exact If-Match check by the caller.
func CheckWrite(current, next, header string) (Decision, error) {
	before, after := Inspect(current), Inspect(next)
	if before.Kind == "yjs" && after.Kind == "none" && header == "" {
		return Ordinary, nil
	}
	if before.Kind == "yjs" && after.Kind != "yjs" {
		return "", &FormatError{}
	}
	if header != "" {
		base, err := conversionBase(header)
		if err != nil {
			return "", err
		}
		if before.Kind != "hybrid" {
			return "", &Refusal{Reason: "invalid-source"}
		}
		if after.Kind != "yjs" {
			return "", &Refusal{Reason: "invalid-capture"}
		}
		actual, err := Baseline(before.Tags)
		if err != nil {
			return "", &Refusal{Reason: "invalid-source"}
		}
		if actual != base {
			return "", &Refusal{Reason: "source-changed"}
		}
		return Conversion, nil
	}
	if before.Kind == "yjs" {
		return Ordinary, nil
	}
	if after.Kind != "yjs" {
		if after.Kind == "invalid" && claimsYPage(next) && !claimsYPage(current) {
			return "", &Refusal{Reason: "invalid-capture"}
		}
		return Ordinary, nil
	}
	if before.Kind == "hybrid" {
		return "", &Refusal{Reason: "implicit-conversion"}
	}
	if before.Kind != "none" {
		return "", &Refusal{Reason: "invalid-source"}
	}
	if pageDocument(current) {
		return Ordinary, nil
	}
	return Initial, nil
}

// NothingToWrite reports a Page body that would store exactly the bytes already on
// disk: a page document whose body differs from current only by the page tag the
// host strips. A clean enter and exit sends such a body; writing it would only
// churn the file's stamp and its backups.
func NothingToWrite(current, next string) bool {
	return Inspect(next).Kind == "yjs" && pageDocument(current) && StripPageTag(next) == current
}
