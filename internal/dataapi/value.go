// Package dataapi is the Go port of hyper-html-api's read engine: it turns a document plus a
// set of extraction rules into JSON. It is a pure library — no net/http, no session, no
// filesystem — so the server can wrap it without the engine knowing anything about requests.
//
// The contract it implements is not "reasonable JSON extraction", it is "byte-for-byte what
// the JavaScript engine already returns", quirks included. Three JS hosts (hyperclay,
// hyperclay-local, makerclay) answer this API today and their answers are what pages depend
// on. Where this package deliberately differs, the difference is recorded in the conformance
// corpus as a tier-3 case, never left to be discovered.
//
// See plans/htmlclay/data-api-plan.md for the full parity ledger.
package dataapi

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
)

// Value is anything extraction can produce: nil, string, []Value, or *Object. There is no
// number or boolean case — the engine reads text and attributes, so every scalar is a string,
// and a non-string rule extracts to null (see extract.go).
type Value any

// Object is a JSON object that remembers its key order, because the JS engine's output order
// is observable and pages rely on it. encoding/json marshals a Go map in sorted order, which
// would silently reorder every response.
type Object struct {
	keys []string
	vals map[string]Value
}

func NewObject() *Object {
	return &Object{vals: map[string]Value{}}
}

// Set stores a key, preserving first-insertion position on overwrite — JS assignment semantics,
// where re-assigning an existing property updates the value and leaves the key where it was.
//
// __proto__ is dropped rather than stored. In JS, extract.js builds results with `const result
// = {}` and assigns `result[key] = …`, so an own "__proto__" key hits the legacy prototype
// setter instead of creating a property, and the key never appears in the output. A Go map
// would happily emit it. Matching JS here is the whole point; the upstream fix is to build
// with Object.create(null), and when that lands this drops out with a corpus change.
func (o *Object) Set(key string, v Value) {
	if key == "__proto__" {
		return
	}
	if _, seen := o.vals[key]; !seen {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = v
}

// Define stores a key the way JSON.parse does, keeping "__proto__" as a real own property.
//
// The difference from Set is not a nicety, it is the whole reason both exist. JSON.parse builds
// objects with CreateDataProperty, which bypasses the prototype setter; assignment does not. So a
// "__proto__" rule key survives parsing, gets iterated, and has its selector resolved — errors and
// all — and then vanishes when the extractor assigns the result. Parse with Define, build output
// with Set, and both halves of that behavior fall out. Measured against node, both directions.
func (o *Object) Define(key string, v Value) {
	if _, seen := o.vals[key]; !seen {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = v
}

func (o *Object) Get(key string) (Value, bool) {
	v, ok := o.vals[key]
	return v, ok
}

func (o *Object) Len() int { return len(o.keys) }

// Keys returns the JS property order: canonical array indices first in ascending numeric
// order, then every other key in insertion order. This is the ordinary-object [[OwnPropertyKeys]]
// rule, and it is why {b:…, "2":…, "0":…, a:…} serializes as 0, 2, b, a.
func (o *Object) Keys() []string {
	var idx, rest []string
	for _, k := range o.keys {
		if isArrayIndex(k) {
			idx = append(idx, k)
		} else {
			rest = append(rest, k)
		}
	}
	sort.Slice(idx, func(i, j int) bool {
		a, _ := strconv.ParseUint(idx[i], 10, 64)
		b, _ := strconv.ParseUint(idx[j], 10, 64)
		return a < b
	})
	return append(idx, rest...)
}

// isArrayIndex reports whether a key is a *canonical* array index: the plain decimal form of an
// integer in [0, 2^32-2]. The canonical part is what the edge cases turn on — "00" and "01" are
// not indices because they do not round-trip through ToString, and neither is "4294967295",
// which is the maximum array *length* and therefore one past the maximum index.
func isArrayIndex(k string) bool {
	if k == "" || len(k) > 10 {
		return false
	}
	if k == "0" {
		return true
	}
	if k[0] == '0' {
		return false // leading zero never round-trips
	}
	for i := 0; i < len(k); i++ {
		if k[i] < '0' || k[i] > '9' {
			return false
		}
	}
	n, err := strconv.ParseUint(k, 10, 64)
	return err == nil && n < 4294967295
}

func (o *Object) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range o.Keys() {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, err := encode(k)
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		val, err := Marshal(o.vals[k])
		if err != nil {
			return nil, err
		}
		buf.Write(val)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// Marshal renders a Value the way the JS hosts do: HTML characters are NOT escaped, because
// JSON.stringify does not escape them and extracted values routinely carry < and &. Go's
// default encoder escapes all three of < > & into \u00XX, which would make every response
// differ from the reference.
//
// Two byte-level exceptions we cannot close and therefore document: Go escapes U+2028 and
// U+2029 even with escaping off, and Go replaces invalid UTF-8 with U+FFFD where JS preserves
// the lone surrogate. Lone surrogates are rejected upstream in rules.go rather than silently
// mangled here.
func Marshal(v Value) ([]byte, error) {
	switch t := v.(type) {
	case nil:
		return []byte("null"), nil
	case *Object:
		return t.MarshalJSON()
	case []Value:
		var buf bytes.Buffer
		buf.WriteByte('[')
		for i, item := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			b, err := Marshal(item)
			if err != nil {
				return nil, err
			}
			buf.Write(b)
		}
		buf.WriteByte(']')
		return buf.Bytes(), nil
	default:
		return encode(t)
	}
}

// encode is the single place SetEscapeHTML(false) is applied. json.Encoder appends a newline
// to every value it writes, which is never wanted mid-document.
func encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
