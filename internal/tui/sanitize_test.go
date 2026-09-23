package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

func TestSanitizeANSI(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain text", "plain text"},
		{"colored \x1b[31mred\x1b[0m text", "colored red text"},
		{"title \x1b]0;evil\x07here", "title here"},
		{"cursor \x1b[2J\x1b[Hclear", "cursor clear"},
		{"two-byte \x1bMesc", "two-byte esc"},
		{"save \x1b7cursor\x1b8 restore", "save cursor restore"},
		{"dcs \x1bPpayload\x1b\\safe", "dcs safe"},
		{"unfinished \x1b]0;title", "unfinished "},
		{"trailing escape\x1b", "trailing escape"},
		{"c1 \x9b2Jsafe", "c1 safe"},
		{"other\x01\x0b\x0c\x7fcontrols", "othercontrols"},
		{"\t中文 e\u0301 👨‍👩‍👧‍👦\n", "\t中文 e\u0301 👨‍👩‍👧‍👦\n"},
		{"", ""},
		// C0 control characters that can spoof the display.
		{"line1\rline2", "line1line2"},
		{"carriage \r\nreturn", "carriage \nreturn"},
		{"bell\x07ring", "bellring"},
		{"back\x08space", "backspace"},
		{"nul\x00byte", "nulbyte"},
		{"keep\nnewlines\n", "keep\nnewlines\n"},
		{"mixed \x1b[31mred\x1b[0m\r\n\x07end", "mixed red\nend"},
	}
	for _, c := range cases {
		if got := sanitizeANSI(c.in); got != c.want {
			t.Errorf("sanitizeANSI(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRenderItemSanitizesText(t *testing.T) {
	it := historyCell{kind: "tool", status: "success",
		text: "output \x1b]0;spoofed title\x07here \x1b[2J\x1b[Hwipe\r"}
	out := renderItemWidth(&it, 80)
	// The rendered entry legitimately carries lipgloss styling (applied after
	// sanitization), so instead of asserting "no ESC at all" assert that the
	// injected sequences and control characters are gone while the content
	// survives.
	if contains(out, "spoofed title") {
		t.Errorf("renderItem kept the embedded OSC sequence: %q", out)
	}
	if contains(out, "\x1b[2J") || contains(out, "\x1b[H") {
		t.Errorf("renderItem kept the injected CSI sequence: %q", out)
	}
	if containsRune(out, '\r') {
		t.Errorf("renderItem kept a carriage return: %q", out)
	}
	if !contains(out, "here") {
		t.Errorf("sanitized text lost content: %q", out)
	}
	// Cache: a second render with unchanged text reuses the sanitized copy.
	it2 := historyCell{kind: "error", text: "\x1b[31mbad\x1b[0m"}
	_ = renderItemWidth(&it2, 80)
	if it2.sanitizedSource != it2.text || strings.ContainsRune(it2.sanitized, 0x1b) {
		t.Errorf("sanitize cache not populated: %+v", it2)
	}
}

func TestRenderItemStylesToolAfterSanitize(t *testing.T) {
	// Force a color profile: under `go test` stdout is not a TTY, so lipgloss
	// defaults to the Ascii profile and emits no ANSI at all. The point of
	// this test is that styling happens on the sanitized text, so make the
	// styles observable first.
	origProfile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	t.Cleanup(func() { lipgloss.SetColorProfile(origProfile) })

	it := historyCell{kind: "tool", toolID: "t1", status: "success", text: "clean\n", toolName: "Bash", toolArgs: map[string]any{"command": "git status"}}
	out := renderItemWidth(&it, 80)
	if !containsESC(out) {
		t.Error("expected styling applied after sanitization")
	}
	for _, want := range []string{"Ran", "git status", "clean"} {
		if !contains(out, want) {
			t.Errorf("rendered tool entry lost %q: %q", want, out)
		}
	}
	if containsRune(out, 0x00) || containsRune(out, '\r') {
		t.Errorf("rendered tool entry kept control characters: %q", out)
	}
}

func containsESC(s string) bool {
	return strings.ContainsRune(s, 0x1b)
}

func containsRune(s string, r rune) bool {
	return strings.ContainsRune(s, r)
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}
