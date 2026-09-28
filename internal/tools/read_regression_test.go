package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestReadByteLimitDoesNotHideFinalLine(t *testing.T) {
	for _, ending := range []string{"", "\n"} {
		t.Run(fmt.Sprintf("newline=%t", ending != ""), func(t *testing.T) {
			ctx := freshCtx(t)
			p := filepath.Join(ctx.WorkingDir, "file")
			first := strings.Repeat("中", 10000)
			last := strings.Repeat("尾", 8000)
			if err := os.WriteFile(p, []byte(first+"\n"+last+ending), 0600); err != nil {
				t.Fatal(err)
			}
			out, err := runTool(t, NewReadTool(), ctx, map[string]any{"file_path": p})
			if err != nil || !strings.Contains(out, "next_offset=2") || strings.Contains(out, "EOF") || strings.Contains(out, "尾") {
				t.Fatalf("incorrect truncation: suffix=%q err=%v", out[max(0, len(out)-200):], err)
			}
			out, err = runTool(t, NewReadTool(), ctx, map[string]any{"file_path": p, "offset": 2})
			if err != nil || !strings.Contains(out, "2\t"+last+"\n") || !strings.Contains(out, "EOF") {
				t.Fatal("final line lost", err)
			}
		})
	}
}

func TestReadBeyondEOFPreservesCompleteSnapshot(t *testing.T) {
	ctx := freshCtx(t)
	p := filepath.Join(ctx.WorkingDir, "file")
	if err := os.WriteFile(p, []byte("complete\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runTool(t, NewReadTool(), ctx, map[string]any{"file_path": p}); err != nil {
		t.Fatal(err)
	}
	if _, err := runTool(t, NewReadTool(), ctx, map[string]any{"file_path": p, "offset": 2}); err == nil {
		t.Fatal("accepted offset past EOF")
	}
	if _, err := runTool(t, NewWriteTool(), ctx, map[string]any{"file_path": p, "mode": "replace", "content": "replacement"}); err != nil {
		t.Fatal(err)
	}
}

func TestReadRejectsFIFO(t *testing.T) {
	ctx := freshCtx(t)
	p := filepath.Join(ctx.WorkingDir, "fifo")
	if err := syscall.Mkfifo(p, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runTool(t, NewReadTool(), ctx, map[string]any{"file_path": p}); err == nil {
		t.Fatal("accepted FIFO")
	}
}
