package tui

import (
	"strings"
	"testing"
)

func TestDiffWordLevelHighlight(t *testing.T) {
	diff := "diff --git a/foo.go b/foo.go\n--- a/foo.go\n+++ b/foo.go\n@@ -1,1 +1,1 @@\n-var totalCount = 10\n+var totalCount = 20\n"
	rendered := renderDiff(diff, 80)
	clean := sanitizeANSI(rendered)

	if !strings.Contains(clean, "var totalCount = 10") || !strings.Contains(clean, "var totalCount = 20") {
		t.Fatalf("clean text missing modified lines:\n%s", clean)
	}

	// Verify wordDiff function directly
	delSegs, addSegs := wordDiff("var totalCount = 10", "var totalCount = 20")
	if len(delSegs) == 0 || len(addSegs) == 0 {
		t.Fatalf("wordDiff returned empty segments")
	}

	foundChangedDel := false
	for _, s := range delSegs {
		if s.text == "10" && s.changed {
			foundChangedDel = true
		}
		if s.text == "totalCount" && s.changed {
			t.Errorf("totalCount should not be marked changed in deletion")
		}
	}
	if !foundChangedDel {
		t.Errorf("did not mark '10' as changed in deletion segments: %+v", delSegs)
	}

	foundChangedAdd := false
	for _, s := range addSegs {
		if s.text == "20" && s.changed {
			foundChangedAdd = true
		}
		if s.text == "totalCount" && s.changed {
			t.Errorf("totalCount should not be marked changed in addition")
		}
	}
	if !foundChangedAdd {
		t.Errorf("did not mark '20' as changed in addition segments: %+v", addSegs)
	}
}

func TestDiffTokenizeWords(t *testing.T) {
	tokens := tokenizeWords("foo.Bar_123 += (a + b)")
	expected := []string{"foo", ".", "Bar_123", " += (", "a", " + ", "b", ")"}
	if len(tokens) != len(expected) {
		t.Fatalf("token count mismatch: got %v, want %v", tokens, expected)
	}
	for i := range tokens {
		if tokens[i] != expected[i] {
			t.Errorf("token %d mismatch: got %q, want %q", i, tokens[i], expected[i])
		}
	}
}
