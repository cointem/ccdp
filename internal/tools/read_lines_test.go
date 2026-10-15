package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRgSingleFileDoesNotSearchSiblings(t *testing.T) {
	ctx := freshCtx(t)
	for _, name := range []string{"target.go", "sibling.go"} {
		if err := os.WriteFile(filepath.Join(ctx.WorkingDir, name), []byte("before\nneedle\nafter\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	out, err := runTool(t, NewRgTool(), ctx, map[string]any{"path": filepath.Join(ctx.WorkingDir, "target.go"), "pattern": "needle", "after": 1})
	if err != nil || !strings.Contains(out, "needle") || !strings.Contains(out, "after") || strings.Contains(out, "sibling.go") {
		t.Fatalf("single file search: %s %v", out, err)
	}
	lines, _, err := ripgrepSearch(ctx, &rgRequest{base: ctx.WorkingDir, singleFile: "target.go", pattern: "needle", outputMode: rgModeContent})
	if err != nil || len(lines) != 1 || lines[0].file != "target.go" {
		t.Fatalf("native single-file search: %+v %v", lines, err)
	}
}
