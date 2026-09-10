package tui

import (
	"regexp"
	"strings"
)

// ANSI sanitization: tool output and user/assistant text can contain terminal
// escape sequences (a file catted by the Bash tool may embed them, or a
// compromised server may return them). Rendered verbatim they can corrupt or
// spoof the TUI, so every log entry is stripped before display. The model
// still sees the raw bytes; this only cleans what the terminal shows.

var (
	// csiRe matches CSI sequences: ESC [ parameters intermediate final-byte.
	csiRe = regexp.MustCompile(`\x1b\[[0-9;:?]*[ -/]*[@-~]`)
	// oscRe matches OSC sequences: ESC ] ... terminated by BEL or ST.
	oscRe = regexp.MustCompile(`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)
	// escRe matches any other two-byte ESC sequence (ESC + 0x30-0x7E).
	escRe = regexp.MustCompile(`\x1b[@-Z\\-_]`)
)

// sanitizeANSI strips terminal escape sequences and disruptive C0 control
// characters (\r, \x00, \x07, \x08) from display text. Newlines are kept;
// the stripped controls could otherwise fake line edits, bells or visual
// carriage returns inside the TUI.
func sanitizeANSI(s string) string {
	if stringsContainsESC(s) {
		s = oscRe.ReplaceAllString(s, "")
		s = csiRe.ReplaceAllString(s, "")
		s = escRe.ReplaceAllString(s, "")
	}
	return strings.Map(func(r rune) rune {
		switch r {
		case '\r', 0x00, 0x07, 0x08:
			return -1
		}
		return r
	}, s)
}

func stringsContainsESC(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			return true
		}
	}
	return false
}
