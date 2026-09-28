package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGrepSingleFileDoesNotSearchSiblings(t *testing.T) {
	ctx := freshCtx(t)
	for _, name := range []string{"target.go", "sibling.go"} {
		if err := os.WriteFile(filepath.Join(ctx.WorkingDir, name), []byte("before\nneedle\nafter\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	out, err := runTool(t, NewGrepTool(), ctx, map[string]any{"path": filepath.Join(ctx.WorkingDir, "target.go"), "pattern": "needle", "-A": 1})
	if err != nil || !strings.Contains(out, "needle") || !strings.Contains(out, "after") || strings.Contains(out, "sibling.go") {
		t.Fatalf("single file search: %s %v", out, err)
	}
	lines, _, err := inProcessSearch(ctx, &grepRequest{base: ctx.WorkingDir, singleFile: "target.go", pattern: "needle", outputMode: grepModeContent})
	if err != nil || len(lines) != 1 || lines[0].file != "target.go" {
		t.Fatalf("fallback single-file search: %+v %v", lines, err)
	}
}
