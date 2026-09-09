package tui

import (
	"strings"
	"testing"
)

func TestSanitizeANSI(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain text", "plain text"},
		{"colored \x1b[31mred\x1b[0m text", "colored red text"},
		{"title \x1b]0;evil\x07here", "title here"},
		{"cursor \x1b[2J\x1b[Hclear", "cursor clear"},
		{"two-byte \x1bMesc", "two-byte esc"},
		{"", ""},
	}
	for _, c := range cases {
		if got := sanitizeANSI(c.in); got != c.want {
			t.Errorf("sanitizeANSI(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRenderItemSanitizesText(t *testing.T) {
	it := logItem{kind: "tool", status: "success", text: "output \x1b]0;spoofed title\x07here"}
	out := renderItem(&it)
	if containsESC(out) {
		t.Errorf("renderItem left escape sequences in text: %q", out)
	}
	if !contains(out, "here") {
		t.Errorf("sanitized text lost content: %q", out)
	}
	// Cache: a second render with unchanged text reuses the sanitized copy.
	it2 := logItem{kind: "error", text: "\x1b[31mbad\x1b[0m"}
	_ = renderItem(&it2)
	if it2.sanitizedLen != len(it2.text) || strings.ContainsRune(it2.sanitized, 0x1b) {
		t.Errorf("sanitize cache not populated: %+v", it2)
	}
}

func containsESC(s string) bool {
	return strings.ContainsRune(s, 0x1b)
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}
