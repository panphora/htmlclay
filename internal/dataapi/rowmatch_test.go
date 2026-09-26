package dataapi

import "testing"

// TestMatchRows pins the pairing answers against the reference, which is what a list write does to
// the rows the page already has: it pairs min(new, old) rows and reports -1 for growth.
func TestMatchRows(t *testing.T) {
	shape := obj("name", "b", "n", "i")
	cases := []struct {
		name      string
		newItems  []Value
		oldValues []Value
		shape     Value
		locked    []int
		want      []int
	}{
		{
			name:      "identical",
			newItems:  []Value{"a", "b", "c"},
			oldValues: []Value{"a", "b", "c"},
			want:      []int{0, 1, 2},
		},
		{
			name:      "reorder",
			newItems:  []Value{"c", "a", "b"},
			oldValues: []Value{"a", "b", "c"},
			want:      []int{2, 0, 1},
		},
		{
			name:      "one edit keeps its place",
			newItems:  []Value{"a", "x", "c"},
			oldValues: []Value{"a", "b", "c"},
			want:      []int{0, 1, 2},
		},
		{
			name:      "growth",
			newItems:  []Value{"a", "b"},
			oldValues: []Value{"a", "b", "c"},
			want:      []int{0, 1},
		},
		{
			name:      "shrink",
			newItems:  []Value{"a", "b", "c", "d"},
			oldValues: []Value{"a", "b"},
			want:      []int{0, 1, -1, -1},
		},
		{
			name:      "duplicates on both sides align in order",
			newItems:  []Value{"a", "a"},
			oldValues: []Value{"a", "a", "a"},
			want:      []int{0, 1},
		},
		{
			name:      "swap still crosses",
			newItems:  []Value{"b", "a"},
			oldValues: []Value{"a", "b"},
			want:      []int{1, 0},
		},
		{
			name:     "object field edit",
			newItems: []Value{obj("name", "A", "n", "9"), obj("name", "B", "n", "2")},
			oldValues: []Value{
				obj("name", "A", "n", "1"),
				obj("name", "B", "n", "2"),
			},
			shape: shape,
			want:  []int{0, 1},
		},
		{
			name:     "object reorder",
			newItems: []Value{obj("name", "B", "n", "2"), obj("name", "A", "n", "1")},
			oldValues: []Value{
				obj("name", "A", "n", "1"),
				obj("name", "B", "n", "2"),
			},
			shape: shape,
			want:  []int{1, 0},
		},
		{
			name:     "missing field is the undefined sentinel",
			newItems: []Value{obj("name", "A"), obj("name", "B", "n", "2")},
			oldValues: []Value{
				obj("name", "A", "n", "1"),
				obj("name", "B", "n", "2"),
			},
			shape: shape,
			want:  []int{0, 1},
		},
		{
			name:      "null item is not the string null",
			newItems:  []Value{nil, "b"},
			oldValues: []Value{"a", "b"},
			want:      []int{0, 1},
		},
		{
			name:      "locked pairing survives",
			newItems:  []Value{"b", "a"},
			oldValues: []Value{"a", "b", "c"},
			locked:    []int{1, -1},
			want:      []int{1, 0},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := matchRows(c.newItems, c.oldValues, c.shape, c.locked)
			if len(got) != len(c.want) {
				t.Fatalf("matchRows = %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("matchRows = %v, want %v", got, c.want)
				}
			}
		})
	}
}
