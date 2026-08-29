package htmlutil

import (
	"bytes"
	"crypto/rand"
	"fmt"
	htmlpkg "html"
	"regexp"
)

// The two attributes a host may inject (spec §9), each in the spec's spelling and
// in the pre-spec one htmlclay shipped with first.
//
// Both fallbacks are PERMANENT, not a migration step. A saved document is a frozen
// client: it carries whatever library version wrote it, hardcoded, and goes on
// running for years. A file saved before this rename holds `htmlclayid` on disk
// forever, and a page saved by an older build can still arrive carrying
// `htmlclaytoken`. Dropping either spelling silently orphans that document's
// version history, or leaves a live credential in a file we promised to strip.
//
// Order is significant: the spec spelling is read first, so a document carrying
// both answers with the current one.
func attrPattern(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)\s+` + name + `=("[^"]*"|'[^']*'|\S+)`)
}

// Ephemeral, per-file and per-tab, stripped before anything is written.
var tokenAttrs = []*regexp.Regexp{attrPattern("savetoken"), attrPattern("htmlclaytoken")}

// Durable identity, so version history follows a document through a rename or a
// move rather than following its path.
var documentIDAttrs = []*regexp.Regexp{attrPattern("documentid"), attrPattern("htmlclayid")}

func isHTMLSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f'
}

func equalFoldASCII(b []byte, name string) bool {
	if len(b) != len(name) {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := b[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != name[i] {
			return false
		}
	}
	return true
}

func equalFoldHTML(b []byte) bool {
	return equalFoldASCII(b, "html")
}

// indexFoldASCII returns the offset of the first case-insensitive occurrence of
// needle in data, or -1.
func indexFoldASCII(data []byte, needle string) int {
	if len(needle) == 0 || len(data) < len(needle) {
		return -1
	}
	for i := 0; i+len(needle) <= len(data); i++ {
		if equalFoldASCII(data[i:i+len(needle)], needle) {
			return i
		}
	}
	return -1
}

// findHTMLTagStart returns the byte offset of the real top-level <html> start
// tag, skipping any <html occurrence inside an HTML comment. Returns -1 if none.
func findHTMLTagStart(data []byte) int {
	n := len(data)
	for i := 0; i < n; i++ {
		if data[i] != '<' {
			continue
		}
		if i+3 < n && data[i+1] == '!' && data[i+2] == '-' && data[i+3] == '-' {
			end := bytes.Index(data[i+4:], []byte("-->"))
			if end < 0 {
				return -1
			}
			i += 4 + end + 2
			continue
		}
		if i+5 < n && equalFoldHTML(data[i+1:i+5]) && (isHTMLSpace(data[i+5]) || data[i+5] == '>') {
			return i
		}
	}
	return -1
}

func findHTMLTagRange(data []byte) (tagStart, closeAngle int, ok bool) {
	tagStart = findHTMLTagStart(data)
	if tagStart < 0 {
		return 0, 0, false
	}

	inDouble, inSingle := false, false
	for i := tagStart + 5; i < len(data); i++ {
		switch data[i] {
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '>':
			if !inSingle && !inDouble {
				return tagStart, i, true
			}
		}
	}
	return 0, 0, false
}

// HasHTMLTag reports whether data contains a real top-level <html> start tag,
// ignoring any <html occurrence inside a comment. Used to reject a save body
// that is not a full HTML document before it overwrites a file.
func HasHTMLTag(data []byte) bool {
	_, _, ok := findHTMLTagRange(data)
	return ok
}

// rawTextNames are the elements whose content is not parsed as markup, so a
// </html> sequence inside one is text and not an end tag.
var rawTextNames = []string{"script", "style", "textarea", "title"}

// rawTextNameAt reports the raw-text element whose start tag begins at i.
func rawTextNameAt(data []byte, i int) (string, bool) {
	for _, name := range rawTextNames {
		end := i + 1 + len(name)
		if end < len(data) && equalFoldASCII(data[i+1:end], name) &&
			(isHTMLSpace(data[end]) || data[end] == '>' || data[end] == '/') {
			return name, true
		}
	}
	return "", false
}

// findHTMLCloseTag returns the offset of a real top-level </html> end tag at or
// after from, or -1.
//
// It skips comments and raw-text elements the same way findHTMLTagStart skips
// comments. A raw regex over the trailing bytes accepted a fake closing tag, so
// `<html><body><!-- </html> -->` passed the restore completeness check and a
// truncated version replaced a good file.
func findHTMLCloseTag(data []byte, from int) int {
	n := len(data)
	for i := from; i < n; i++ {
		if data[i] != '<' {
			continue
		}
		if i+3 < n && data[i+1] == '!' && data[i+2] == '-' && data[i+3] == '-' {
			end := bytes.Index(data[i+4:], []byte("-->"))
			if end < 0 {
				return -1
			}
			i += 4 + end + 2
			continue
		}
		if name, ok := rawTextNameAt(data, i); ok {
			gt := bytes.IndexByte(data[i:], '>')
			if gt < 0 {
				return -1
			}
			body := i + gt + 1
			closeTag := "</" + name
			// The scan for </name must require the HTML tag-name delimiter after
			// the name, matching what rawTextNameAt does on the start-tag side.
			// A bare </script prefix inside </scripture is a longer name, not a
			// close, and treating it as one exited raw-text mode too early and
			// exposed a later textual </html>.
			pos := body
			for {
				rel := indexFoldASCII(data[pos:], closeTag)
				if rel < 0 {
					// The raw-text element never closes.
					return -1
				}
				hit := pos + rel
				after := hit + len(closeTag)
				if after >= len(data) {
					// </name with nothing after it: the element never closes.
					return -1
				}
				if isHTMLSpace(data[after]) || data[after] == '/' || data[after] == '>' {
					// A real end tag for this raw-text element. Resume the outer
					// scan from here.
					i = hit
					break
				}
				// A longer name such as </scripture: keep searching the raw text.
				pos = hit + 2
			}
			continue
		}
		if i+6 <= n && data[i+1] == '/' && equalFoldHTML(data[i+2:i+6]) {
			j := i + 6
			for j < n && isHTMLSpace(data[j]) {
				j++
			}
			if j < n && data[j] == '>' {
				return i
			}
		}
	}
	return -1
}

// IsCompleteHTMLDocument reports whether data is a whole document: a real
// top-level <html> start tag followed by a matching </html> end tag. HasHTMLTag
// alone is not sufficient for a restore, because it accepts `<html><body>partial`
// and would let a truncated version overwrite a good file.
func IsCompleteHTMLDocument(data []byte) bool {
	_, closeAngle, ok := findHTMLTagRange(data)
	if !ok {
		return false
	}
	return findHTMLCloseTag(data, closeAngle+1) >= 0
}

// removeAll strips every spelling of an attribute from a run of tag attributes.
// Injecting clears them all before writing, so serving the same document twice
// cannot leave a stale value behind under one of the names.
func removeAll(attrs []byte, patterns []*regexp.Regexp) []byte {
	for _, p := range patterns {
		attrs = p.ReplaceAll(attrs, nil)
	}
	return attrs
}

// injectAttr writes value under every name in names, having first removed every
// spelling in patterns. More than one name is how a serve-time attribute survives
// a rename: see InjectToken.
func injectAttr(data []byte, patterns []*regexp.Regexp, value string, names ...string) []byte {
	tagStart, closeAngle, ok := findHTMLTagRange(data)
	if !ok {
		return data
	}

	nameEnd := tagStart + 5
	attrs := data[nameEnd:closeAngle]
	stripped := removeAll(attrs, patterns)

	escaped := htmlpkg.EscapeString(value)
	attr := make([]byte, 0, len(names)*(len(escaped)+16))
	for _, name := range names {
		attr = append(attr, ` `+name+`="`+escaped+`"`...)
	}

	out := make([]byte, 0, len(data)+len(attr))
	out = append(out, data[:nameEnd]...)
	out = append(out, attr...)
	out = append(out, stripped...)
	out = append(out, '>')
	out = append(out, data[closeAngle+1:]...)
	return out
}

func stripAttr(data []byte, patterns []*regexp.Regexp) []byte {
	tagStart, closeAngle, ok := findHTMLTagRange(data)
	if !ok {
		return data
	}

	nameEnd := tagStart + 5
	attrs := data[nameEnd:closeAngle]
	stripped := removeAll(attrs, patterns)

	out := make([]byte, 0, len(data))
	out = append(out, data[:nameEnd]...)
	out = append(out, stripped...)
	out = append(out, '>')
	out = append(out, data[closeAngle+1:]...)
	return out
}

// readAttr returns the first spelling that matches, so callers get the current
// name when a document carries it and the legacy one otherwise.
func readAttr(data []byte, patterns []*regexp.Regexp) string {
	tagStart, closeAngle, ok := findHTMLTagRange(data)
	if !ok {
		return ""
	}

	for _, p := range patterns {
		loc := p.FindSubmatch(data[tagStart : closeAngle+1])
		if loc == nil {
			continue
		}
		val := string(loc[1])
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') {
			val = val[1 : len(val)-1]
		}
		return val
	}
	return ""
}

// InjectToken injects the save token on <html> under BOTH spellings, carrying the
// same value. Ephemeral auth token — stripped on save under either name, so the
// pair never reaches disk.
//
// Serving both is what makes the rename safe, and it is not temporary. A document
// reads this attribute by name in its own inline script, and a saved document is a
// frozen client: it goes on reading whatever name it was written against, for
// years, with no library update able to reach it. Serving only `savetoken` breaks
// every such document at once, silently — the page loads, the save button does
// nothing, and the only clue is a status line telling the person to open the file
// through the app, which is what they did. That set is not hypothetical: it
// includes the welcome example, which is written to disk once and never
// overwritten on upgrade.
//
// Two attributes with one value is safe where two credentials would not be. A
// reader takes the first name it recognises and gets the same token either way.
func InjectToken(data []byte, value string) []byte {
	return injectAttr(data, tokenAttrs, value, "savetoken", "htmlclaytoken")
}

// StripToken removes the save token from <html> under either spelling. This one
// has to stay permissive forever: a document saved by a pre-rename build arrives
// carrying htmlclaytoken, and a strip that only knew the new name would write a
// live credential to disk.
func StripToken(data []byte) []byte {
	return stripAttr(data, tokenAttrs)
}

// ReadHTMLClayID extracts the document id from the <html> tag, preferring the
// spec's documentid and falling back to the legacy htmlclayid. Returns empty
// string if neither is present.
func ReadHTMLClayID(data []byte) string {
	return readAttr(data, documentIDAttrs)
}

// InjectHTMLClayID adds documentid to <html> if the document has no id under
// either spelling. This is persistent — never stripped on save. A file already
// carrying htmlclayid keeps it untouched: its version history is filed under
// that value, and rewriting it here would strand every version taken before now.
//
// One name only, unlike the token above, and the asymmetry is the point. No
// document reads its own id: it exists for this host's version history, and the
// clients that touch it (to keep a peer's morph from stripping it) already know
// both spellings. So nothing is frozen against the old name, and writing both
// would put two ids into every saved file, permanently, for no reader.
func InjectHTMLClayID(data []byte, id string) []byte {
	if ReadHTMLClayID(data) != "" {
		return data
	}
	return injectAttr(data, documentIDAttrs, id, "documentid")
}

// SetHTMLClayID forces documentid on <html>, replacing any existing value under
// either spelling. Restore uses it to keep the target file's canonical identity
// rather than adopting the id stored inside the restored version.
func SetHTMLClayID(data []byte, id string) []byte {
	return injectAttr(data, documentIDAttrs, id, "documentid")
}

// StripHTMLClayID removes the document id from <html> under either spelling.
// Used when restoring into a file that carries no identity of its own, so a
// version taken from a different file cannot donate its id.
func StripHTMLClayID(data []byte) []byte {
	return stripAttr(data, documentIDAttrs)
}

// bannerStart and bannerEnd delimit the server-injected read-only banner. The
// markers exist so StripBanner can remove the banner byte-exactly without
// parsing: everything between them is server-authored and never user content.
var (
	bannerStart = []byte("<!--htmlclay-banner-->")
	bannerEnd   = []byte("<!--/htmlclay-banner-->")
)

// WrapBanner surrounds server-authored banner markup with the strip markers.
func WrapBanner(banner []byte) []byte {
	out := make([]byte, 0, len(bannerStart)+len(banner)+len(bannerEnd))
	out = append(out, bannerStart...)
	out = append(out, banner...)
	out = append(out, bannerEnd...)
	return out
}

// InjectBanner inserts an already-wrapped banner immediately before the real
// top-level </html> end tag, using the same comment- and raw-text-aware scan as
// IsCompleteHTMLDocument, so a fake close tag inside a script or comment cannot
// misplace it. Position in the DOM is irrelevant to a fixed-position banner,
// and inserting at the end sidesteps every head/body reparenting subtlety an
// injection near <html> would raise. A document with no close tag gets the
// banner appended; one with no <html> tag at all is returned unchanged (served
// plain rather than guessed at).
func InjectBanner(data, wrappedBanner []byte) []byte {
	_, closeAngle, ok := findHTMLTagRange(data)
	if !ok {
		return data
	}
	insert := len(data)
	if end := findHTMLCloseTag(data, closeAngle+1); end >= 0 {
		insert = end
	}
	out := make([]byte, 0, len(data)+len(wrappedBanner))
	out = append(out, data[:insert]...)
	out = append(out, wrappedBanner...)
	out = append(out, data[insert:]...)
	return out
}

// StripBanner removes every marker-delimited banner from data. It runs on every
// save and restore: a banner that reached a token-holding tab (for instance
// pushed through live-sync into an edit-mode page) must never autosave itself
// into the file on disk. An unterminated start marker strips nothing rather
// than guessing at an end.
func StripBanner(data []byte) []byte {
	for {
		i := bytes.Index(data, bannerStart)
		if i < 0 {
			return data
		}
		rest := data[i+len(bannerStart):]
		j := bytes.Index(rest, bannerEnd)
		if j < 0 {
			return data
		}
		next := make([]byte, 0, i+len(rest)-j-len(bannerEnd))
		next = append(next, data[:i]...)
		next = append(next, rest[j+len(bannerEnd):]...)
		data = next
	}
}

// GenerateHTMLClayID generates a UUID v4 using crypto/rand.
func GenerateHTMLClayID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("cannot generate htmlclayid: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
