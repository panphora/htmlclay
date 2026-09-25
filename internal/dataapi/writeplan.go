package dataapi

import (
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

// planWrite walks rules against data without touching the document. It reports the body keys the
// rules do not define, and the scalar rules whose selector matches nothing. Shape problems are left
// to validateShape, which reports them with the paths the caller can act on, and inside list items
// only unknown keys are checked, because the rows a grow would create do not exist yet.
func planWrite(root *html.Node, rules, data Value) ([]string, []Unmatched, error) {
	p := &writePlan{unknownKeys: []string{}, unmatched: []Unmatched{}}
	p.checkKeys(rules, data, nil)
	if err := p.checkTargets(root, rules, data, nil); err != nil {
		return nil, nil, err
	}
	return p.unknownKeys, p.unmatched, nil
}

type writePlan struct {
	unknownKeys []string
	unmatched   []Unmatched
}

func (p *writePlan) checkKeys(rule, value Value, path []any) {
	if list, ok := rule.([]Value); ok {
		var shape Value
		if len(list) > 1 {
			shape = list[1]
		}
		if _, ok := shape.(*Object); !ok {
			return
		}
		items, ok := value.([]Value)
		if !ok {
			return
		}
		for i, item := range items {
			p.checkKeys(shape, item, appendPath(path, i))
		}
		return
	}

	ruleObj, ok := rule.(*Object)
	if !ok {
		return
	}
	valueObj, ok := value.(*Object)
	if !ok {
		return
	}
	for _, key := range valueObj.Keys() {
		sub, defined := ruleObj.Get(key)
		if !defined {
			p.unknownKeys = append(p.unknownKeys, label(appendPath(path, key)))
			continue
		}
		v, _ := valueObj.Get(key)
		p.checkKeys(sub, v, appendPath(path, key))
	}
}

func (p *writePlan) checkTargets(ctx *html.Node, rule, value Value, path []any) error {
	if isUndefined(value) {
		return nil
	}

	switch r := rule.(type) {
	case string:
		if strings.HasSuffix(r, "[]") || r == "." || strings.HasPrefix(r, "@") {
			return nil
		}
		selector := r
		if at := ruleAttrIndex(r); at != -1 {
			selector = r[:at]
		}
		if selector == "" {
			return nil
		}
		matches, err := Find(ctx, selector, FindOpts{})
		if err != nil {
			return err
		}
		if len(matches) == 0 {
			p.unmatched = append(p.unmatched, Unmatched{Path: label(path), Selector: selector})
		}
		return nil

	case []Value:
		return nil

	case *Object:
		valueObj, ok := value.(*Object)
		if !ok {
			return nil
		}
		for _, key := range r.Keys() {
			sub, _ := r.Get(key)
			v, has := valueObj.Get(key)
			if !has {
				v = missing
			}
			if err := p.checkTargets(ctx, sub, v, appendPath(path, key)); err != nil {
				return err
			}
		}
	}
	return nil
}

// label is the write path format a rejected key is reported with: `items[1].nmae`, `meta.titel`.
func label(path []any) string {
	out := ""
	for _, part := range path {
		switch t := part.(type) {
		case int:
			out += "[" + strconv.Itoa(t) + "]"
		case string:
			if out == "" {
				out = t
			} else {
				out += "." + t
			}
		}
	}
	return out
}

func appendPath(path []any, part any) []any {
	out := make([]any, len(path)+1)
	copy(out, path)
	out[len(path)] = part
	return out
}

func pathString(path []any) string {
	parts := make([]string, len(path))
	for i, part := range path {
		parts[i] = jsString(part)
	}
	return strings.Join(parts, ".")
}
