package htmlutil

import (
	"bytes"
	"crypto/rand"
	"fmt"
	htmlpkg "html"
	"regexp"
)

var tokenAttr = regexp.MustCompile(`(?i)\s+htmlclaytoken=("[^"]*"|'[^']*'|\S+)`)
var htmlclayidAttr = regexp.MustCompile(`(?i)\s+htmlclayid=("[^"]*"|'[^']*'|\S+)`)

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
			end := indexFoldASCII(data[body:], "</"+name)
			if end < 0 {
				return -1
			}
			i = body + end
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

func injectAttr(data []byte, attrRegex *regexp.Regexp, attrName, value string) []byte {
	tagStart, closeAngle, ok := findHTMLTagRange(data)
	if !ok {
		return data
	}

	nameEnd := tagStart + 5
	attrs := data[nameEnd:closeAngle]
	stripped := attrRegex.ReplaceAll(attrs, nil)

	attr := ` ` + attrName + `="` + htmlpkg.EscapeString(value) + `"`

	out := make([]byte, 0, len(data)+len(attr))
	out = append(out, data[:nameEnd]...)
	out = append(out, []byte(attr)...)
	out = append(out, stripped...)
	out = append(out, '>')
	out = append(out, data[closeAngle+1:]...)
	return out
}

func stripAttr(data []byte, attrRegex *regexp.Regexp) []byte {
	tagStart, closeAngle, ok := findHTMLTagRange(data)
	if !ok {
		return data
	}

	nameEnd := tagStart + 5
	attrs := data[nameEnd:closeAngle]
	stripped := attrRegex.ReplaceAll(attrs, nil)

	out := make([]byte, 0, len(data))
	out = append(out, data[:nameEnd]...)
	out = append(out, stripped...)
	out = append(out, '>')
	out = append(out, data[closeAngle+1:]...)
	return out
}

func readAttr(data []byte, attrRegex *regexp.Regexp) string {
	tagStart, closeAngle, ok := findHTMLTagRange(data)
	if !ok {
		return ""
	}

	loc := attrRegex.FindSubmatch(data[tagStart : closeAngle+1])
	if loc == nil {
		return ""
	}

	val := string(loc[1])
	if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') {
		val = val[1 : len(val)-1]
	}
	return val
}

// InjectToken injects the htmlclaytoken attribute on <html>. Ephemeral auth
// token — stripped on save.
func InjectToken(data []byte, value string) []byte {
	return injectAttr(data, tokenAttr, "htmlclaytoken", value)
}

// StripToken removes the htmlclaytoken attribute from <html>.
func StripToken(data []byte) []byte {
	return stripAttr(data, tokenAttr)
}

// ReadHTMLClayID extracts the htmlclayid UUID from the <html> tag.
// Returns empty string if not present.
func ReadHTMLClayID(data []byte) string {
	return readAttr(data, htmlclayidAttr)
}

// InjectHTMLClayID adds htmlclayid to <html> if not already present.
// This is persistent — never stripped on save.
func InjectHTMLClayID(data []byte, id string) []byte {
	if ReadHTMLClayID(data) != "" {
		return data
	}
	return injectAttr(data, htmlclayidAttr, "htmlclayid", id)
}

// SetHTMLClayID forces htmlclayid on <html>, replacing any existing value.
// Restore uses it to keep the target file's canonical identity rather than
// adopting the id stored inside the restored version.
func SetHTMLClayID(data []byte, id string) []byte {
	return injectAttr(data, htmlclayidAttr, "htmlclayid", id)
}

// StripHTMLClayID removes the htmlclayid attribute from <html>. Used when
// restoring into a file that carries no identity of its own, so a version taken
// from a different file cannot donate its id.
func StripHTMLClayID(data []byte) []byte {
	return stripAttr(data, htmlclayidAttr)
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
