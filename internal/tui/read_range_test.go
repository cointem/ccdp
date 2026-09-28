package tui

import (
	"strings"
	"testing"
)

func TestReadSummaryIncludesRangeUnits(t *testing.T) {
	for _, unit := range []string{"lines", "bytes"} {
		out := toolArgumentSummary("Read", map[string]any{"file_path": "runtime.go", "offset": float64(1500), "limit": float64(40), "unit": unit}, "")
		if !strings.Contains(out, unit+" offset=1500 limit=40") {
			t.Fatalf("range missing: %s", out)
		}
	}
}
