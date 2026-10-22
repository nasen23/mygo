package web

import (
	"unicode/utf16"
)

// textDiff returns the replacement that turns old into now: the range of
// old (UTF-16 offsets, as browsers and text clients count) that changed, and
// what is there now. A surrogate pair is never split.
func textDiff(old, now string) (from, to int, text string, ok bool) {
	if old == now {
		return 0, 0, "", false
	}
	a, b := utf16.Encode([]rune(old)), utf16.Encode([]rune(now))
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	if p > 0 && utf16.IsSurrogate(rune(a[p-1])) && a[p-1] < 0xDC00 { // stopped after a high surrogate
		p--
	}
	q := 0
	for q < len(a)-p && q < len(b)-p && a[len(a)-1-q] == b[len(b)-1-q] {
		q++
	}
	if q > 0 && a[len(a)-q] >= 0xDC00 && a[len(a)-q] <= 0xDFFF { // stopped before a low surrogate
		q--
	}
	return p, len(a) - q, string(utf16.Decode(b[p : len(b)-q])), true
}

func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }
