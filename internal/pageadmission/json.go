package pageadmission

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/panphora/htmlclay/internal/dataapi"
)

var (
	jsonNumber = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)
	identifier = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)
)

func parsePageJSON(source string) (dataapi.Value, error) {
	if value, err := dataapi.ParseStrict(source); err == nil {
		return value, nil
	}
	strict, err := relaxedPageJSON(source)
	if err != nil {
		return nil, err
	}
	return dataapi.ParseStrict(strict)
}

func pageSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', '\u00a0', '\u1680', '\u2028', '\u2029', '\u202f', '\u205f', '\u3000', '\ufeff':
		return true
	}
	return r >= '\u2000' && r <= '\u200a'
}

func nextPageToken(source string, at int) int {
	for at < len(source) {
		r, size := utf8.DecodeRuneInString(source[at:])
		if pageSpace(r) {
			at += size
			continue
		}
		if strings.HasPrefix(source[at:], "//") {
			for at < len(source) && source[at] != '\n' {
				at++
			}
			continue
		}
		if strings.HasPrefix(source[at:], "/*") {
			end := strings.Index(source[at+2:], "*/")
			if end < 0 {
				return len(source)
			}
			at += end + 4
			continue
		}
		return at
	}
	return at
}

func pageWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '_' || c == '$' || c == '.' || c == '+' || c == '-'
}

func relaxedPageJSON(source string) (string, error) {
	var out strings.Builder
	for at := 0; at < len(source); {
		at = nextPageToken(source, at)
		if at >= len(source) {
			break
		}
		char := source[at]
		if strings.ContainsRune("{}[]:", rune(char)) {
			out.WriteByte(char)
			at++
			continue
		}
		if char == ',' {
			next := nextPageToken(source, at+1)
			if next < len(source) && (source[next] == '}' || source[next] == ']') {
				at++
				continue
			}
			out.WriteByte(',')
			at++
			continue
		}
		if char == '"' || char == '\'' {
			end := at + 1
			for end < len(source) && source[end] != char {
				if source[end] == '\\' {
					end++
				}
				end++
			}
			if end >= len(source) {
				return "", fmt.Errorf("unterminated page JSON string at %d", at)
			}
			if char == '"' {
				out.WriteString(source[at : end+1])
			} else {
				out.WriteByte('"')
				for i := at + 1; i < end; i++ {
					if source[i] == '\\' && i+1 < end {
						if source[i+1] == '\'' {
							out.WriteByte('\'')
						} else {
							out.WriteByte('\\')
							out.WriteByte(source[i+1])
						}
						i++
					} else if source[i] == '"' {
						out.WriteString(`\"`)
					} else {
						out.WriteByte(source[i])
					}
				}
				out.WriteByte('"')
			}
			at = end + 1
			continue
		}
		end := at
		for end < len(source) && pageWordByte(source[end]) {
			end++
		}
		if end == at {
			return "", fmt.Errorf("unexpected page JSON character at %d", at)
		}
		word := source[at:end]
		switch {
		case word == "true" || word == "false" || word == "null" || jsonNumber.MatchString(word):
			out.WriteString(word)
		case identifier.MatchString(word) && nextPageToken(source, end) < len(source) && source[nextPageToken(source, end)] == ':':
			out.WriteByte('"')
			out.WriteString(word)
			out.WriteByte('"')
		default:
			return "", fmt.Errorf("invalid page JSON token at %d", at)
		}
		at = end
	}
	return out.String(), nil
}
