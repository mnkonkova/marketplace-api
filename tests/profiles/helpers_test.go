package profiles_test

import (
	"reflect"
	"testing"

	"marketpclce/internal/profiles"
)

func TestIsHTTPURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{name: "https", in: "https://example.com/v.mp4", want: true},
		{name: "http", in: "http://example.com/v.mp4", want: true},
		{name: "empty", in: "", want: false},
		{name: "no scheme", in: "example.com/v.mp4", want: false},
		{name: "ftp scheme", in: "ftp://example.com/v.mp4", want: false},
		{name: "javascript scheme", in: "javascript:alert(1)", want: false},
		{name: "data scheme", in: "data:text/html,<script>x</script>", want: false},
		{name: "file scheme", in: "file:///etc/passwd", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := profiles.IsHTTPURL(tc.in); got != tc.want {
				t.Errorf("IsHTTPURL(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestDedupStrings(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{name: "nil", in: nil, want: []string{}},
		{name: "empty", in: []string{}, want: []string{}},
		{name: "dups", in: []string{"a", "b", "a", "c", "b"}, want: []string{"a", "b", "c"}},
		{name: "no dups", in: []string{"a", "b", "c"}, want: []string{"a", "b", "c"}},
		{name: "preserves order", in: []string{"c", "a", "b"}, want: []string{"c", "a", "b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := profiles.DedupStrings(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
