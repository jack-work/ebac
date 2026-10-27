package model

import (
	"strings"
	"unicode/utf8"
)

func Short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func FirstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	const max = 110
	if len(s) > max {
		// Trim on a rune boundary so a multi-byte character is never cut
		// in half into a replacement char.
		r := []rune(s)
		if len(r) > max {
			r = r[:max]
		}
		s = string(r) + "…"
	}
	return s
}

func FirstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// cutBody keeps at most n bytes of s, on a rune boundary.
func CutBody(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…", true
}
