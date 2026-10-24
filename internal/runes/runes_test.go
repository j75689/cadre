package runes

import (
	"testing"
	"unicode/utf8"
)

func TestCut(t *testing.T) {
	for _, tc := range []struct {
		s    string
		n    int
		want string
	}{
		{"abc", 5, "abc"},
		{"abc", 2, "ab"},
		{"aé", 2, "a"}, // é is two bytes
		{"aé", 3, "aé"},
		{"日本", 4, "日"},
		{"x", 0, ""},
	} {
		if got := Cut(tc.s, tc.n); got != tc.want || !utf8.ValidString(got) {
			t.Errorf("Cut(%q, %d) = %q, want %q", tc.s, tc.n, got, tc.want)
		}
	}
}
