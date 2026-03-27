package htmlutil

import (
	"crypto/rand"
	"fmt"
	htmlpkg "html"
	"regexp"
)

var htmlTagPrefix = regexp.MustCompile(`(?i)<html[\s>]`)
var tokenAttr = regexp.MustCompile(`(?i)\s+htmlclaytoken=("[^"]*"|'[^']*'|\S+)`)
var htmlclayidAttr = regexp.MustCompile(`(?i)\s+htmlclayid=("[^"]*"|'[^']*'|\S+)`)

func findHTMLTagRange(data []byte) (tagStart, closeAngle int, ok bool) {
	loc := htmlTagPrefix.FindIndex(data)
	if loc == nil {
		return 0, 0, false
	}
	tagStart = loc[0]

	if data[loc[1]-1] == '>' {
		return tagStart, loc[1] - 1, true
	}

	inDouble, inSingle := false, false
	for i := loc[1]; i < len(data); i++ {
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
	_, closeAngle, ok := findHTMLTagRange(data)
	if !ok {
		return ""
	}

	loc := attrRegex.FindSubmatch(data[:closeAngle+1])
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
