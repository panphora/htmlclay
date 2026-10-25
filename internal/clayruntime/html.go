package clayruntime

import (
	"bytes"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"golang.org/x/net/html"
)

const descriptorID = "clay-runtime-map"

var runtimeRel = map[string]bool{
	"stylesheet":    true,
	"preload":       true,
	"modulepreload": true,
}

// Mapping is one runtime URL a projection rewrote.
type Mapping struct {
	Original string `json:"original"`
	Local    string `json:"local"`
}

// Skipped is one candidate the projection left alone, with the reason.
type Skipped struct {
	Value  string `json:"value"`
	Reason string `json:"reason"`
}

// Unavailable explains why a document could not be projected onto a local release.
type Unavailable struct {
	Reason   string   `json:"reason"`
	Versions []string `json:"versions"`
}

// Projection is a served page rewritten onto local URLs, plus what it did.
type Projection struct {
	HTML        string       `json:"html"`
	Mappings    []Mapping    `json:"mappings"`
	Skipped     []Skipped    `json:"skipped"`
	Unavailable *Unavailable `json:"unavailable"`
}

// Canonical is a served page turned back into authoring URLs.
type Canonical struct {
	HTML              string `json:"html"`
	Changed           int    `json:"changed"`
	DescriptorRemoved bool   `json:"descriptorRemoved"`
}

type candidate struct {
	value     string
	raw       string
	valueFrom int
	valueTo   int
	start     int
	quote     byte
}

type descriptorSpan struct {
	start       int
	startTagEnd int
	endTagStart int
	end         int
}

type splice struct {
	start int
	end   int
	text  string
}

func attribute(token html.Token, name string) (string, bool) {
	for _, attr := range token.Attr {
		if attr.Key == name {
			return attr.Val, true
		}
	}
	return "", false
}

func isSpaceByte(char byte) bool {
	switch char {
	case ' ', '\t', '\n', '\f', '\r':
		return true
	default:
		return false
	}
}

func isRuntimeCandidate(token html.Token) bool {
	if token.Data == "script" {
		_, ok := attribute(token, "src")
		return ok
	}
	if token.Data != "link" {
		return false
	}
	if _, ok := attribute(token, "href"); !ok {
		return false
	}
	rel, _ := attribute(token, "rel")
	for _, word := range strings.Fields(strings.ToLower(rel)) {
		if runtimeRel[word] {
			return true
		}
	}
	return false
}

// attributeBounds finds one attribute's name through the end of its value inside a tag's
// raw text, the way parse5's attribute source location does.
func attributeBounds(raw []byte, name string) (int, int, bool) {
	index := 1
	for index < len(raw) && !isSpaceByte(raw[index]) && raw[index] != '>' && raw[index] != '/' {
		index++
	}
	for index < len(raw) {
		for index < len(raw) && isSpaceByte(raw[index]) {
			index++
		}
		if index >= len(raw) || raw[index] == '>' {
			break
		}
		if raw[index] == '/' {
			index++
			continue
		}
		nameStart := index
		for index < len(raw) && !isSpaceByte(raw[index]) && raw[index] != '=' && raw[index] != '>' && raw[index] != '/' {
			index++
		}
		key := strings.ToLower(string(raw[nameStart:index]))
		for index < len(raw) && isSpaceByte(raw[index]) {
			index++
		}
		valueEnd := index
		if index < len(raw) && raw[index] == '=' {
			index++
			for index < len(raw) && isSpaceByte(raw[index]) {
				index++
			}
			if index < len(raw) && (raw[index] == '"' || raw[index] == '\'') {
				quote := raw[index]
				index++
				for index < len(raw) && raw[index] != quote {
					index++
				}
				if index < len(raw) {
					index++
				}
			} else {
				for index < len(raw) && !isSpaceByte(raw[index]) && raw[index] != '>' {
					index++
				}
			}
			valueEnd = index
		}
		if key == name {
			return nameStart, valueEnd, true
		}
	}
	return 0, 0, false
}

// valueBounds finds the value bytes inside one attribute's raw text, and the quote
// that delimited them, 0 when the attribute was unquoted.
func valueBounds(raw string) (int, int, byte, bool) {
	equals := strings.Index(raw, "=")
	if equals < 0 {
		return 0, 0, 0, false
	}
	index := equals + 1
	for index < len(raw) && isSpaceByte(raw[index]) {
		index++
	}
	quote := byte(0)
	if index < len(raw) && (raw[index] == '"' || raw[index] == '\'') {
		quote = raw[index]
	}
	if quote == 0 {
		return index, len(raw), quote, true
	}
	close := strings.LastIndexByte(raw, quote)
	if close > index {
		return index + 1, close, quote, true
	}
	return index + 1, len(raw), quote, true
}

// scan walks the source once, collecting every runtime candidate's attribute bounds and
// every descriptor script's span, both as byte offsets into the untouched source.
func scan(source []byte) ([]candidate, []descriptorSpan) {
	text := string(source)
	tokenizer := html.NewTokenizer(bytes.NewReader(source))
	offset := 0
	found := make([]candidate, 0, 8)
	spans := make([]descriptorSpan, 0, 1)
	pending := -1
	pendingStartTagEnd := 0
	for {
		kind := tokenizer.Next()
		if kind == html.ErrorToken {
			break
		}
		raw := tokenizer.Raw()
		at := offset
		offset += len(raw)
		switch kind {
		case html.StartTagToken, html.SelfClosingTagToken:
			token := tokenizer.Token()
			if token.Data == "script" {
				if id, ok := attribute(token, "id"); ok && id == descriptorID {
					pending = at
					pendingStartTagEnd = offset
				}
			}
			if !isRuntimeCandidate(token) {
				continue
			}
			name := "href"
			if token.Data == "script" {
				name = "src"
			}
			value, _ := attribute(token, name)
			attrStart, attrEnd, ok := attributeBounds(raw, name)
			if !ok {
				continue
			}
			from, to, quote, ok := valueBounds(string(raw[attrStart:attrEnd]))
			if !ok || at+attrStart+to > len(text) {
				continue
			}
			found = append(found, candidate{
				value:     value,
				raw:       text[at+attrStart+from : at+attrStart+to],
				valueFrom: at + attrStart + from,
				valueTo:   at + attrStart + to,
				start:     at,
				quote:     quote,
			})
		case html.EndTagToken:
			if pending >= 0 && tokenizer.Token().Data == "script" {
				spans = append(spans, descriptorSpan{start: pending, startTagEnd: pendingStartTagEnd, endTagStart: at, end: offset})
				pending = -1
			}
		}
	}
	if pending >= 0 {
		spans = append(spans, descriptorSpan{start: pending, startTagEnd: pendingStartTagEnd, endTagStart: len(text), end: len(text)})
	}
	return found, spans
}

func quoteJSON(value string) string {
	var builder strings.Builder
	builder.WriteByte('"')
	for _, char := range value {
		switch char {
		case '"':
			builder.WriteString(`\"`)
		case '\\':
			builder.WriteString(`\\`)
		case '\b':
			builder.WriteString(`\b`)
		case '\f':
			builder.WriteString(`\f`)
		case '\n':
			builder.WriteString(`\n`)
		case '\r':
			builder.WriteString(`\r`)
		case '\t':
			builder.WriteString(`\t`)
		default:
			if char < 0x20 {
				fmt.Fprintf(&builder, `\u%04x`, char)
				continue
			}
			builder.WriteRune(char)
		}
	}
	builder.WriteByte('"')
	return builder.String()
}

func descriptorTag(version string, entries []Mapping) string {
	var json strings.Builder
	json.WriteString(`{"schema":1,"version":`)
	json.WriteString(quoteJSON(version))
	json.WriteString(`,"entries":[`)
	for index, entry := range entries {
		if index > 0 {
			json.WriteByte(',')
		}
		json.WriteString(`{"original":`)
		json.WriteString(quoteJSON(entry.Original))
		json.WriteString(`,"local":`)
		json.WriteString(quoteJSON(entry.Local))
		json.WriteByte('}')
	}
	json.WriteByte(']')
	json.WriteByte('}')
	body := strings.ReplaceAll(json.String(), "<", `\u003c`)
	return `<script type="application/json" id="` + descriptorID + `" clay="no-save no-watch no-snapshot">` + body + `</script>`
}

func applySplices(text string, splices []splice) string {
	ordered := append([]splice(nil), splices...)
	sort.SliceStable(ordered, func(left, right int) bool {
		return ordered[left].start > ordered[right].start
	})
	out := text
	for _, item := range ordered {
		out = out[:item.start] + item.text + out[item.end:]
	}
	return out
}

func attributeText(value string, quote byte) string {
	text := strings.ReplaceAll(value, "&", "&amp;")
	switch quote {
	case '\'':
		return strings.ReplaceAll(text, "'", "&#39;")
	case '"':
		return strings.ReplaceAll(text, `"`, "&quot;")
	}
	if strings.ContainsAny(text, " \t\n\v\f\r\"'<>=`") {
		return `"` + strings.ReplaceAll(text, `"`, "&quot;") + `"`
	}
	return text
}

// respelled is replacement written into the attribute raw came from, keeping the file's own
// spelling of the tail value and replacement share, so a query written with a bare '&' keeps
// it. A shared head holding '&' could split a character reference, so that case escapes all.
func respelled(raw, value, replacement string, quote byte) string {
	shared := 0
	for shared < len(value) && shared < len(replacement) &&
		value[len(value)-1-shared] == replacement[len(replacement)-1-shared] {
		shared++
	}
	head := value[:len(value)-shared]
	newHead := replacement[:len(replacement)-shared]
	if strings.Contains(head, "&") || !strings.HasPrefix(raw, head) {
		return attributeText(replacement, quote)
	}
	if quote == 0 && strings.ContainsAny(newHead, " \t\n\v\f\r\"'<>=`") {
		return attributeText(replacement, quote)
	}
	return attributeText(newHead, quote) + raw[len(head):]
}

// mayProject reports whether source can name a runtime URL: clayjs (perhaps with a character
// reference inside the URL) or the host of one of the selected release's exact aliases.
func mayProject(source []byte, opts Options) bool {
	lower := bytes.ToLower(source)
	if bytes.Contains(lower, []byte("clayjs")) || bytes.Contains(lower, []byte("&#")) {
		return true
	}
	for alias := range opts.Releases[opts.SelectedVersion].Aliases {
		if parsed, err := url.Parse(alias); err == nil && parsed.Host != "" &&
			bytes.Contains(lower, []byte(strings.ToLower(parsed.Host))) {
			return true
		}
	}
	return false
}

// ProjectRuntimeHTML rewrites every runtime URL in a page onto the bundled release and
// writes the descriptor that turns them back, splices only, every other byte untouched.
// mayHold reports whether source can contain needle in any letter case. Parsing a large
// document costs far more than this scan, and most documents name no runtime URL at all.
func mayHold(source []byte, needle string) bool {
	return bytes.Contains(bytes.ToLower(source), []byte(needle))
}

func ProjectRuntimeHTML(source []byte, opts Options) Projection {
	if !mayProject(source, opts) {
		return Projection{HTML: string(source), Mappings: []Mapping{}, Skipped: []Skipped{}}
	}
	text := string(source)
	candidates, _ := scan(source)

	type resolved struct {
		node   candidate
		result *Resolution
	}
	items := make([]resolved, 0, len(candidates))
	versions := make([]string, 0, 2)
	seen := make(map[string]bool, 2)
	missing := false
	absent := false
	for _, node := range candidates {
		result := ResolveRuntimeURL(node.value, opts)
		if result == nil {
			if parsed := ParseRuntimeURL(node.value, opts); parsed != nil &&
				(parsed.Kind == "rolling" || parsed.Kind == "exact") {
				absent = true
			}
		} else {
			if !result.Available {
				missing = true
			}
			if !seen[result.Version] {
				seen[result.Version] = true
				versions = append(versions, result.Version)
			}
		}
		items = append(items, resolved{node: node, result: result})
	}
	if missing || absent || len(versions) > 1 {
		sort.Strings(versions)
		reason := "mixed-versions"
		if missing {
			reason = "release-missing"
		} else if absent {
			reason = "path-missing"
		}
		return Projection{
			HTML:        text,
			Mappings:    []Mapping{},
			Skipped:     []Skipped{},
			Unavailable: &Unavailable{Reason: reason, Versions: versions},
		}
	}

	mappings := make([]Mapping, 0, len(items))
	skipped := []Skipped{}
	splices := make([]splice, 0, len(items)+1)
	version := ""
	if len(versions) == 1 {
		version = versions[0]
	}
	insertAt := -1

	for _, item := range items {
		if item.result == nil {
			continue
		}
		splices = append(splices, splice{
			start: item.node.valueFrom,
			end:   item.node.valueTo,
			text:  respelled(item.node.raw, item.node.value, item.result.LocalURL, item.node.quote),
		})
		mappings = append(mappings, Mapping{Original: item.node.value, Local: item.result.LocalURL})
		if insertAt < 0 {
			insertAt = item.node.start
		}
	}

	if len(mappings) == 0 {
		return Projection{HTML: text, Mappings: mappings, Skipped: skipped, Unavailable: nil}
	}

	if !opts.NoDescriptor {
		splices = append(splices, splice{start: insertAt, end: insertAt, text: descriptorTag(version, mappings)})
	}
	return Projection{HTML: applySplices(text, splices), Mappings: mappings, Skipped: skipped, Unavailable: nil}
}

// CanonicalizeRuntimeHTML turns a served page's local URLs back into authoring URLs and
// removes the descriptor it was projected with.
func CanonicalizeRuntimeHTML(source []byte, releases map[string]Release, documentURL string) Canonical {
	if !mayHold(source, "/_/clayjs/") && !mayHold(source, descriptorID) &&
		!mayHold(source, "&#") && !mayHold(source, "&sol;") && !mayHold(source, "&lowbar;") {
		return Canonical{HTML: string(source)}
	}
	text := string(source)
	candidates, spans := scan(source)
	splices := make([]splice, 0, len(spans)+len(candidates))
	var descriptor *Descriptor
	removed := false
	for _, span := range spans {
		if !removed {
			descriptor = ParseDescriptor([]byte(text[span.startTagEnd:span.endTagStart]))
		}
		removed = true
		splices = append(splices, splice{start: span.start, end: span.end})
	}

	changed := 0
	for _, node := range candidates {
		parsed := ParseRuntimeURL(node.value, Options{DocumentURL: documentURL})
		if parsed == nil || parsed.Kind != "local" {
			continue
		}
		replacement := CanonicalizeRuntimeURL(node.value, CanonOptions{Descriptor: descriptor, Releases: releases, DocumentURL: documentURL})
		if replacement == node.value {
			continue
		}
		splices = append(splices, splice{
			start: node.valueFrom,
			end:   node.valueTo,
			text:  respelled(node.raw, node.value, replacement, node.quote),
		})
		changed++
	}

	return Canonical{HTML: applySplices(text, splices), Changed: changed, DescriptorRemoved: removed}
}
