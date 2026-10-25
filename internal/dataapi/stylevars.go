package dataapi

import (
	"regexp"
	"strings"
)

// This file ports hyper-html-api/src/engine/style-vars.js: one CSS custom property inside an
// inline style attribute, read and written as text. Every delimiter it looks for is ASCII, so
// walking bytes finds the same boundaries the reference finds walking UTF-16 units.

var customPropertyName = regexp.MustCompile(`^--[A-Za-z0-9_-]+$`)

func isCustomPropertyName(name string) bool { return customPropertyName.MatchString(name) }

// jsSpaceClass is JS's \s, which Go's ASCII-only \s is not. Same set as isJSSpace.
const jsSpaceClass = `[\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]`

var importantSuffix = regexp.MustCompile(`(?i)!` + jsSpaceClass + `*important` + jsSpaceClass + `*$`)

// splitDeclarations splits on top-level semicolons, ignoring any inside quotes, brackets or
// comments. Brackets close only their own kind, as in CSS.
func splitDeclarations(text string) []string {
	var parts []string
	var stack []byte
	var quote byte
	start := 0
	for i := 0; i < len(text); i++ {
		c := text[i]
		if quote != 0 {
			if c == '\\' {
				i++
			} else if c == quote {
				quote = 0
			}
			continue
		}
		if c == '/' && i+1 < len(text) && text[i+1] == '*' {
			if end := strings.Index(text[i+2:], "*/"); end == -1 {
				i = len(text)
			} else {
				i += 2 + end + 1
			}
			continue
		}
		switch {
		case c == '"' || c == '\'':
			quote = c
		case c == '(':
			stack = append(stack, ')')
		case c == '[':
			stack = append(stack, ']')
		case c == '{':
			stack = append(stack, '}')
		case (c == ')' || c == ']' || c == '}') && len(stack) > 0 && stack[len(stack)-1] == c:
			stack = stack[:len(stack)-1]
		case c == ';' && len(stack) == 0:
			parts = append(parts, text[start:i])
			start = i + 1
		}
	}
	return append(parts, text[start:])
}

// stripComments drops comments outside strings, each replaced by one space.
func stripComments(text string) string {
	var out strings.Builder
	var quote byte
	for i := 0; i < len(text); i++ {
		c := text[i]
		if quote != 0 {
			out.WriteByte(c)
			if c == '\\' && i+1 < len(text) {
				i++
				out.WriteByte(text[i])
			} else if c == quote {
				quote = 0
			}
			continue
		}
		if c == '/' && i+1 < len(text) && text[i+1] == '*' {
			if end := strings.Index(text[i+2:], "*/"); end == -1 {
				i = len(text)
			} else {
				i += 2 + end + 1
			}
			out.WriteByte(' ')
			continue
		}
		if c == '"' || c == '\'' {
			quote = c
		}
		out.WriteByte(c)
	}
	return out.String()
}

type declaration struct {
	name, value string
	important   bool
}

func parseDeclaration(raw string) (declaration, bool) {
	raw = stripComments(raw)
	colon := strings.IndexByte(raw, ':')
	if colon == -1 {
		return declaration{}, false
	}
	d := declaration{name: jsTrim(raw[:colon]), value: jsTrim(raw[colon+1:])}
	if loc := importantSuffix.FindStringIndex(d.value); loc != nil {
		d.value = jsTrim(d.value[:loc[0]])
		d.important = true
	}
	return d, true
}

// readStyleVar returns the declaration that wins: the last one, unless an earlier one is
// !important and the last one is not. ok is false when the property is absent.
func readStyleVar(style, name string) (string, bool) {
	var winner *declaration
	for _, raw := range splitDeclarations(style) {
		d, ok := parseDeclaration(raw)
		if !ok || d.name != name {
			continue
		}
		if winner == nil || d.important || !winner.important {
			winner = &d
		}
	}
	if winner == nil {
		return "", false
	}
	return winner.value, true
}

// writeStyleVar returns the new attribute text, and false when nothing is left in it. An empty
// value removes every declaration of the property. Otherwise the first declaration is rewritten in
// place (keeping !important if any copy had it), later duplicates are dropped, and a property not
// yet present is appended.
func writeStyleVar(style, name, value string) (string, bool) {
	remove := value == ""
	parts := splitDeclarations(style)
	decls := make([]declaration, len(parts))
	parsed := make([]bool, len(parts))
	important := false
	for i, raw := range parts {
		decls[i], parsed[i] = parseDeclaration(raw)
		if parsed[i] && decls[i].name == name && decls[i].important {
			important = true
		}
	}
	out := make([]string, 0, len(parts))
	placed := false
	for i, raw := range parts {
		if !parsed[i] || decls[i].name != name {
			out = append(out, raw)
			continue
		}
		if remove || placed {
			continue
		}
		lead := raw[:len(raw)-len(strings.TrimLeftFunc(raw, isJSSpace))]
		next := lead + name + ": " + value
		if important {
			next += " !important"
		}
		out = append(out, next)
		placed = true
	}
	next := strings.Join(out, ";")
	if !remove && !placed {
		body := strings.TrimRightFunc(next, isJSSpace)
		switch {
		case body == "":
			next = name + ": " + value
		case strings.HasSuffix(body, ";"):
			next = body + " " + name + ": " + value
		default:
			next = body + "; " + name + ": " + value
		}
	}
	if jsTrim(next) == "" {
		return "", false
	}
	return next, true
}
