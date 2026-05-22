package clarify_test

import (
	"reflect"
	"testing"

	"marketpclce/internal/clarify"
)

func TestDedupNonEmpty(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{name: "nil", in: nil, want: []string{}},
		{name: "empty", in: []string{}, want: []string{}},
		{name: "all empty strings", in: []string{"", "  ", ""}, want: []string{}},
		{name: "trims and dedups", in: []string{"a", " a ", "b", "a", ""}, want: []string{"a", "b"}},
		{name: "preserves first-seen order", in: []string{"b", "a", "c", "a"}, want: []string{"b", "a", "c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := clarify.DedupNonEmpty(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
