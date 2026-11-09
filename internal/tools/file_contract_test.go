package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func editArgs(path, old, next string) map[string]any {
	return map[string]any{"file_path": path, "edits": []any{map[string]any{"old_text": old, "new_text": next}}}
}
func TestLocalEditAfterPartialReadPreservesOtherChanges(t *testing.T) {
	ctx := freshCtx(t)
	p := filepath.Join(ctx.WorkingDir, "a")
	os.WriteFile(p, []byte("head\ntarget\n"+strings.Repeat("other\n", 3000)), 0640)
	if _, e := runTool(t, NewReadTool(), ctx, map[string]any{"file_path": p, "offset": 2, "limit": 1}); e != nil {
		t.Fatal(e)
	}
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("external\n")
	f.Close()
	out, e := runTool(t, NewEditTool(), ctx, editArgs(p, "target", "changed"))
	if e != nil || !strings.Contains(out, ":2") {
		t.Fatal(out, e)
	}
	b, _ := os.ReadFile(p)
	info, _ := os.Stat(p)
	if !strings.Contains(string(b), "changed") || !strings.HasSuffix(string(b), "external\n") || info.Mode().Perm() != 0640 {
		t.Fatal("lost content or permissions")
	}
}
func TestEditValidatesWholeBatch(t *testing.T) {
	ctx := freshCtx(t)
	p := filepath.Join(ctx.WorkingDir, "a")
	os.WriteFile(p, []byte("abcdef"), 0600)
	cases := [][]any{{map[string]any{"old_text": "abc", "new_text": "X"}, map[string]any{"old_text": "bcd", "new_text": "Y"}}, {map[string]any{"old_text": "abc", "new_text": "X"}, map[string]any{"old_text": "def"}}, {map[string]any{"old_text": "absent", "new_text": "X"}}}
	for _, edits := range cases {
		if _, e := runTool(t, NewEditTool(), ctx, map[string]any{"file_path": p, "edits": edits}); e == nil {
			t.Fatal("invalid edits accepted")
		}
		b, _ := os.ReadFile(p)
		if string(b) != "abcdef" {
			t.Fatal("partial mutation")
		}
	}
}
func TestWriteRequiresCurrentVersion(t *testing.T) {
	ctx := freshCtx(t)
	p := filepath.Join(ctx.WorkingDir, "a")
	if err := os.WriteFile(p, []byte("old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"file_path": p, "content": "new"}
	if _, e := runTool(t, NewWriteTool(), ctx, args); e == nil || !strings.Contains(e.Error(), "has not been read") {
		t.Fatalf("unread replacement: %v", e)
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != "old\n" {
		t.Fatalf("unread file changed: %q %v", b, err)
	}
	runTool(t, NewReadTool(), ctx, map[string]any{"file_path": p})
	info, _ := os.Stat(p)
	os.WriteFile(p, []byte("bad\n"), 0600)
	os.Chtimes(p, info.ModTime(), info.ModTime())
	if _, e := runTool(t, NewWriteTool(), ctx, args); e == nil || !strings.Contains(e.Error(), "modified since observation") {
		t.Fatalf("stale replacement: %v", e)
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != "bad\n" {
		t.Fatalf("external change overwritten: %q %v", b, err)
	}
	runTool(t, NewReadTool(), ctx, map[string]any{"file_path": p})
	if _, e := runTool(t, NewWriteTool(), ctx, args); e != nil {
		t.Fatal(e)
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != "new" {
		t.Fatalf("replacement lost: %q %v", b, err)
	}
	if info, err := os.Stat(p); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("replacement lost permissions: %v %v", info, err)
	}
}
func TestReadRangeContinuationAndCompleteVisibility(t *testing.T) {
	ctx := freshCtx(t)
	p := filepath.Join(ctx.WorkingDir, "a")
	var b strings.Builder
	for i := 1; i <= 1600; i++ {
		fmt.Fprintf(&b, "中文 line %d\n", i)
	}
	os.WriteFile(p, []byte(b.String()), 0600)
	out, e := runTool(t, NewReadTool(), ctx, map[string]any{"file_path": p, "offset": 1500, "limit": 40})
	if e != nil || !strings.Contains(out, "1500\t中文 line 1500") || !strings.Contains(out, "next_offset=1540") || !utf8.ValidString(out) {
		t.Fatal(out, e)
	}
	if _, e = ctx.Resources.Files.ObservedVersion(p); e != nil {
		t.Fatal("partial read lost version observation", e)
	}
	for _, args := range []map[string]any{{"file_path": p, "offset": 0}, {"file_path": p, "offset": 1700}, {"file_path": p, "unit": "bytes"}} {
		if _, e := runTool(t, NewReadTool(), ctx, args); e == nil {
			t.Fatal("invalid range accepted", args)
		}
	}
}
func TestReadLongLineCanSkipAndNeverClaimsComplete(t *testing.T) {
	ctx := freshCtx(t)
	p := filepath.Join(ctx.WorkingDir, "a")
	os.WriteFile(p, []byte(strings.Repeat("x", 100000)+"\nlast\n"), 0600)
	out, e := runTool(t, NewReadTool(), ctx, map[string]any{"file_path": p})
	if e != nil || !strings.Contains(out, "exceeds output budget") {
		t.Fatal(out, e)
	}
	out, e = runTool(t, NewReadTool(), ctx, map[string]any{"file_path": p, "offset": 2, "limit": 1})
	if e != nil || !strings.Contains(out, "2\tlast") || !strings.Contains(out, "EOF") {
		t.Fatal(out, e)
	}
}
func TestReadOutputBudgetDoesNotDetermineOverwrite(t *testing.T) {
	ctx := freshCtx(t)
	ctx.OutputLimit = 300
	p := filepath.Join(ctx.WorkingDir, "a")
	os.WriteFile(p, []byte(strings.Repeat("short\n", 50)), 0600)
	out, e := runTool(t, NewReadTool(), ctx, map[string]any{"file_path": p})
	if e != nil || len(out) > 300 {
		t.Fatal(out, e)
	}
	if _, e = ctx.Resources.Files.ObservedVersion(p); e != nil {
		t.Fatal("display limit invalidated observation", e)
	}
}
