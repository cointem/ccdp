package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCountLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lines.txt")
	content := strings.Repeat("line\n", 3000) // 15000 bytes, 3000 lines
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	// Offsets at line boundaries report the line that starts there (1-based).
	for _, tc := range []struct {
		offset int
		want   int
	}{
		{0, 1},
		{5, 2},        // start of line 2 ("line\n" is 5 bytes)
		{5000, 1001},  // start of line 1001
		{14995, 3000}, // start of the last line
	} {
		if got := countLines(path, tc.offset); got != tc.want {
			t.Errorf("countLines(offset=%d) = %d, want %d", tc.offset, got, tc.want)
		}
	}

	// A huge offset does not blow up (fixed 64KB chunks) and stops at EOF.
	if got := countLines(path, 1<<30); got < 1 {
		t.Errorf("countLines beyond EOF = %d, want a positive line number", got)
	}
}
