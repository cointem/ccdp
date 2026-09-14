package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// ANSI sanitization: tool output and user/assistant text can contain terminal
// escape sequences (a file catted by the Bash tool may embed them, or a
// compromised server may return them). Rendered verbatim they can corrupt or
// spoof the TUI, so every log entry is stripped before display. The model
// still sees the raw bytes; this only cleans what the terminal shows.

// sanitizeANSI strips terminal escape sequences and control characters from
// display text. Tabs and newlines are kept for code and paragraph layout.
// Use the terminal parser rather than a regex subset: cursor save/restore,
// DCS strings and unfinished sequences must not reach the terminal either.
func sanitizeANSI(s string) string {
	if stringsContainsTerminalSequence(s) {
		s = ansi.Strip(s)
	}
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return -1
		}
		return r
	}, s)
}

func stringsContainsTerminalSequence(s string) bool {
	for i := 0; i < len(s); i++ {
		// Some UTF-8 continuation bytes also match this range. The parser
		// preserves complete UTF-8 runes, so parsing those strings is safe.
		if s[i] == 0x1b || (s[i] >= 0x80 && s[i] <= 0x9f) {
			return true
		}
	}
	return false
}
