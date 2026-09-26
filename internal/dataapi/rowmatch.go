package dataapi

import "strconv"

// This file is a line-by-line port of hyper-html-api/src/engine/row-match.js. A list arrives as
// plain JSON with no ids, so identity has to be inferred: content decides WHICH old row a new item
// is, never WHETHER it is one. Two passes — anchors (content appearing exactly once on each side,
// matched wherever it moved to) and then in-order alignment at minimum cost for everything left
// over.

// undefKey is the reference's ' undef' stand-in for JSON.stringify(undefined), which is not a
// string and so cannot be compared with `!==` without one.
const undefKey = " undef"

// isObjectShape is the reference's `typeof shape === 'object' && shape !== null`. An array shape
// counts, because typeof [] is 'object': Object.keys then yields the indices.
func isObjectShape(shape Value) bool {
	switch shape.(type) {
	case *Object, []Value:
		return true
	}
	return false
}

// shapeFields is Object.keys(shape) for the two object shapes. JSON arrays are dense, so every
// index is present.
func shapeFields(shape Value) []string {
	switch t := shape.(type) {
	case *Object:
		return t.Keys()
	case []Value:
		fields := make([]string, len(t))
		for i := range t {
			fields[i] = strconv.Itoa(i)
		}
		return fields
	}
	return nil
}

// itemField is `item == null ? undefined : item[f]`. A missing field is the undefined sentinel,
// which is not the same as the JSON null a present field can hold.
func itemField(item Value, f string) Value {
	if isNullish(item) {
		return missing
	}
	switch t := item.(type) {
	case *Object:
		if v, ok := t.Get(f); ok {
			return v
		}
	case []Value:
		if idx, ok := fieldIndex(f); ok && idx < len(t) {
			return t[idx]
		}
	case string:
		// A string primitive has its characters as own index properties, which is why the
		// reference can read a "field" off one at all.
		units := []rune(t)
		if idx, ok := fieldIndex(f); ok && idx < len(units) {
			return string(units[idx])
		}
	}
	return missing
}

// fieldIndex reports whether f is a canonical array index, so "00" and "1.0" read as absent
// exactly as they do in JavaScript.
func fieldIndex(f string) (int, bool) {
	if !isArrayIndex(f) {
		return 0, false
	}
	n := 0
	for i := 0; i < len(f); i++ {
		n = n*10 + int(f[i]-'0')
	}
	return n, true
}

// encodedField is JSON.stringify(value), with undefined kept distinguishable from the string
// "undefined" or "null". JSON.stringify of a value Go cannot encode (an Infinity that overflowed
// its JSON literal) is "null" there, so that is what an error here becomes.
func encodedField(v Value) string {
	if isUndefined(v) {
		return undefKey
	}
	b, err := Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

// itemKey is the identity content matching compares on: the whole item for a scalar list, every
// shape field for an object list, in the object's own key order.
func itemKey(item, shape Value) string {
	if !isObjectShape(shape) {
		if isNullish(item) {
			return " null"
		}
		return jsString(item)
	}
	fields := shapeFields(shape)
	parts := make([]Value, len(fields))
	for i, f := range fields {
		parts[i] = encodedField(itemField(item, f))
	}
	b, err := Marshal(parts)
	if err != nil {
		return ""
	}
	return string(b)
}

// differingFields counts how many shape fields disagree. It is the primary cost of a pairing, so a
// changed field outranks any displacement.
func differingFields(a, b, shape Value) int {
	if !isObjectShape(shape) {
		if jsStrictEqual(a, b) {
			return 0
		}
		return 1
	}
	fields := shapeFields(shape)
	if len(fields) == 0 {
		return 0
	}
	differing := 0
	for _, f := range fields {
		if encodedField(itemField(a, f)) != encodedField(itemField(b, f)) {
			differing++
		}
	}
	return differing
}

// jsStrictEqual is ===, for the value kinds a scalar list holds. Two objects are only equal when
// they are the same object, which two rows never are.
func jsStrictEqual(a, b Value) bool {
	if isNullish(a) || isNullish(b) {
		return isNullish(a) && isNullish(b)
	}
	switch x := a.(type) {
	case string:
		y, ok := b.(string)
		return ok && x == y
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case float64:
		y, ok := b.(float64)
		return ok && x == y
	case int:
		y, ok := b.(int)
		return ok && x == y
	}
	return false
}

func isNullish(v Value) bool {
	return v == nil || isUndefined(v)
}

// matchRows maps each incoming item onto an old row index, or -1 when it grows the list. It always
// pairs min(len(newItems), len(oldValues)) rows when unlocked: it creates only on growth and
// destroys only on shrink.
func matchRows(newItems, oldValues []Value, shape Value, locked []int) []int {
	n := len(newItems)
	m := len(oldValues)
	matches := make([]int, n)
	for i := range matches {
		matches[i] = -1
	}

	// A caller that knows which old row an item is keeps that pairing; the passes below only
	// fill in what it did not answer for. Range-checked and deduped so the min(n, m) guarantee
	// holds for any locked, not only a sanitised one.
	if locked != nil {
		seen := map[int]bool{}
		for i := 0; i < n; i++ {
			j := -1
			if i < len(locked) {
				j = locked[i]
			}
			if j < 0 || j >= m || seen[j] {
				continue
			}
			matches[i] = j
			seen[j] = true
		}
	}
	if n == 0 || m == 0 {
		return matches
	}

	newKeys := make([]string, n)
	for i, item := range newItems {
		newKeys[i] = itemKey(item, shape)
	}
	oldKeys := make([]string, m)
	for j, item := range oldValues {
		oldKeys[j] = itemKey(item, shape)
	}

	takenOld := make([]bool, m)
	for _, j := range matches {
		if j >= 0 {
			takenOld[j] = true
		}
	}

	// Uniqueness is counted over the rows still in play, never over the locked ones: a value
	// can be duplicated across the whole list and still be the only unlocked row of its kind.
	oldByKey := map[string]int{}
	for j, key := range oldKeys {
		if takenOld[j] {
			continue
		}
		if _, seen := oldByKey[key]; seen {
			oldByKey[key] = -1
			continue
		}
		oldByKey[key] = j
	}
	newCounts := map[string]int{}
	for i, key := range newKeys {
		if matches[i] >= 0 {
			continue
		}
		newCounts[key]++
	}

	for i, key := range newKeys {
		if matches[i] >= 0 {
			continue
		}
		if newCounts[key] != 1 {
			continue
		}
		j, ok := oldByKey[key]
		if !ok || j == -1 || takenOld[j] {
			continue
		}
		matches[i] = j
		takenOld[j] = true
	}

	var leftNew, leftOld []int
	for i := 0; i < n; i++ {
		if matches[i] < 0 {
			leftNew = append(leftNew, i)
		}
	}
	for j := 0; j < m; j++ {
		if !takenOld[j] {
			leftOld = append(leftOld, j)
		}
	}
	if len(leftNew) == 0 || len(leftOld) == 0 {
		return matches
	}

	// Changed content outranks displacement: the scale is larger than any total displacement
	// this list can produce, so position only ever breaks a tie.
	scale := n*m + 1
	cost := func(i, j int) int {
		return differingFields(newItems[i], oldValues[j], shape)*scale + abs(i-j)
	}
	for _, pair := range alignInOrder(leftNew, leftOld, cost) {
		matches[pair[0]] = pair[1]
	}
	return matches
}

// alignInOrder pairs every entry of the shorter list with one of the longer, keeping both in order
// and minimising total cost. Each returned pair is [newIndex, oldIndex].
func alignInOrder(newIdx, oldIdx []int, cost func(i, j int) int) [][2]int {
	if len(newIdx) <= len(oldIdx) {
		return align(newIdx, oldIdx, cost, false)
	}
	return align(oldIdx, newIdx, func(a, b int) int { return cost(b, a) }, true)
}

// align is the minimum-cost in-order alignment, with the reference's tie rule: an equal-cost pair
// is TAKEN (take <= skip), so rows line up rather than drift.
func align(short, long []int, cost func(a, b int) int, flipped bool) [][2]int {
	s := len(short)
	l := len(long)
	const inf = int(1) << 62

	table := make([][]int, s+1)
	takeHere := make([][]bool, s+1)
	for i := 0; i <= s; i++ {
		table[i] = make([]int, l+1)
		takeHere[i] = make([]bool, l+1)
		for j := 0; j <= l; j++ {
			table[i][j] = inf
		}
	}
	for j := 0; j <= l; j++ {
		table[s][j] = 0
	}

	for i := s - 1; i >= 0; i-- {
		for j := l - 1; j >= 0; j-- {
			take := cost(short[i], long[j]) + table[i+1][j+1]
			skip := table[i][j+1]
			if take <= skip {
				table[i][j] = take
				takeHere[i][j] = true
			} else {
				table[i][j] = skip
			}
		}
	}

	var pairs [][2]int
	i, j := 0, 0
	for i < s && j < l {
		if takeHere[i][j] {
			if flipped {
				pairs = append(pairs, [2]int{long[j], short[i]})
			} else {
				pairs = append(pairs, [2]int{short[i], long[j]})
			}
			i++
			j++
			continue
		}
		j++
	}
	return pairs
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
