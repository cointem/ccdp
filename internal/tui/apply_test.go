package tui

import "testing"

func TestLooksLikePatch(t *testing.T) {
	if !looksLikePatch("diff --git a/x.go b/x.go\nindex 000..111\n--- a/x.go\n+++ b/x.go\n") {
		t.Error("expected unified diff detection")
	}
	if !looksLikePatch("--- a/a.txt\n+++ b/a.txt\n@@ -1 +1 @@\n-old\n+new\n") {
		t.Error("expected patch detection")
	}
	if looksLikePatch("# My Plan\n\n1. read files\n2. fix bugs") {
		t.Error("markdown plan should not be treated as a patch")
	}
}
