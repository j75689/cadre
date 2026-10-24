// Package runes cuts UTF-8 text to a byte budget without splitting a
// character.
package runes

import "unicode/utf8"

// Cut returns the longest prefix of s that is at most n bytes and ends on
// a rune boundary.
func Cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 0 {
		return ""
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
