package tui

import (
	"strings"
	"testing"
)

func TestHighlightCodeBasic(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	lines := []string{
		"package main",
		"",
		"func main() {",
		"\tprintln(\"hello\")",
		"}",
	}

	// Dark theme
	darkRes := highlightCode(lines, "go", true)
	if len(darkRes) != len(lines) {
		t.Fatalf("expected %d lines, got %d", len(lines), len(darkRes))
	}
	// Verify that ANSI escape codes are present in non-empty lines
	if !strings.ContainsRune(darkRes[0], '\x1b') {
		t.Errorf("expected ANSI escape sequences in highlighted code: %q", darkRes[0])
	}

	// Sanitizing ANSI should yield the original text
	for i, l := range darkRes {
		if clean := sanitizeANSI(l); clean != lines[i] {
			t.Errorf("line %d stripped text mismatch: got %q, want %q", i, clean, lines[i])
		}
	}

	// Light theme
	lightRes := highlightCode(lines, "go", false)
	if len(lightRes) != len(lines) {
		t.Fatalf("expected %d lines for light theme, got %d", len(lines), len(lightRes))
	}

	// Unknown language should return nil
	unknownRes := highlightCode(lines, "unknown_lang_xyz", true)
	if unknownRes != nil {
		t.Errorf("expected nil for unknown language, got %v", unknownRes)
	}

	// Plaintext / text should return nil
	for _, plain := range []string{"text", "txt", "plain", "plaintext", "output", "none"} {
		if res := highlightCode(lines, plain, true); res != nil {
			t.Errorf("expected nil for %s, got %v", plain, res)
		}
	}

	// Empty lines should return nil
	if res := highlightCode(nil, "go", true); res != nil {
		t.Errorf("expected nil for empty lines, got %v", res)
	}
}

func TestHighlightCodeCache(t *testing.T) {
	lines := []string{"x := 1", "y := 2"}
	res1 := highlightCode(lines, "go", true)
	res2 := highlightCode(lines, "go", true)
	if len(res1) != len(res2) {
		t.Fatalf("cache result line count mismatch")
	}
	for i := range res1 {
		if res1[i] != res2[i] {
			t.Errorf("cache result mismatch at line %d: %q vs %q", i, res1[i], res2[i])
		}
	}
}

func TestRenderMarkdownWithSyntaxHighlighting(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	md := "```go\nvar a int = 42\n```"
	rendered := renderMarkdown(md, 80)
	// Check that gutter is present and keyword is present
	clean := sanitizeANSI(rendered)
	if !strings.Contains(clean, "var a int = 42") {
		t.Fatalf("rendered markdown missing code text: %q", clean)
	}
	if !strings.Contains(rendered, "\x1b[") {
		t.Fatalf("rendered markdown missing ANSI styling: %q", rendered)
	}
}
