package web

import "testing"

func TestTextDiff(t *testing.T) {
	cases := []struct {
		old, now string
		from, to int
		text     string
	}{
		{"", "hello", 0, 0, "hello"},
		{"hello", "help", 3, 5, "p"},
		{"teh cat", "the cat", 1, 3, "he"},             // autocorrect
		{"abc", "ab", 2, 3, ""},                        // backspace
		{"hi", "hi pasted text", 2, 2, " pasted text"}, // paste
		{"你", "你好", 1, 1, "好"},                         // composition committed
		{"a😀b", "a😁b", 1, 3, "😁"},                      // a surrogate pair is never split
		{"😀", "😀😀", 2, 2, "😀"},
	}
	for _, c := range cases {
		from, to, text, ok := textDiff(c.old, c.now)
		if !ok || from != c.from || to != c.to || text != c.text {
			t.Errorf("textDiff(%q, %q) = %d, %d, %q; want %d, %d, %q", c.old, c.now, from, to, text, c.from, c.to, c.text)
		}
	}
	if _, _, _, ok := textDiff("same", "same"); ok {
		t.Error("no change reported a change")
	}
}
