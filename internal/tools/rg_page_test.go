package tools

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

type rgProgramPage struct {
	Entries []struct {
		Path  string
		Line  int
		Text  string
		Match bool
		Count int
	}
	NextOffset int `json:"next_offset"`
	Truncated  bool
}

func runRgProgramPage(t *testing.T, ctx *Context) (Result, rgProgramPage) {
	t.Helper()
	result, err := executeToolResult(NewRgTool(), ctx)
	if err != nil {
		t.Fatal(err)
	}
	var page rgProgramPage
	if err := json.Unmarshal(result.Data, &page); err != nil {
		t.Fatal(err)
	}
	return result, page
}

func TestRgProgramPaginationDoesNotCountContext(t *testing.T) {
	root := t.TempDir()
	reviewRgWrite(t, root, "text.txt", "before\nhit first\nbetween\nhit second\nafter\n")
	ctx := rgCtx(t, root)
	offset := 0
	for _, matchLine := range []int{2, 4} {
		ctx.Args = map[string]any{"pattern": "hit", "context": 1, "limit": 1, "offset": offset}
		result, page := runRgProgramPage(t, ctx)
		if page.NextOffset != offset+1 || len(page.Entries) != 3 {
			t.Fatalf("context advanced the match cursor: %s", result.Data)
		}
		for i, entry := range page.Entries {
			if entry.Line != matchLine-1+i || entry.Match != (i == 1) {
				t.Fatalf("wrong context page: %s", result.Data)
			}
			sep := "-"
			if entry.Match {
				sep = ":"
			}
			if !strings.Contains(result.Text, fmt.Sprintf("%s%s%d%s%s\n", entry.Path, sep, entry.Line, sep, entry.Text)) {
				t.Fatalf("text and program page differ: %s\n%s", result.Text, result.Data)
			}
		}
		if page.Truncated != (offset == 0) || result.DataTruncated != page.Truncated {
			t.Fatalf("wrong completion state: %s", result.Data)
		}
		offset = page.NextOffset
	}
}

func TestRgOnlyMatchingKeepsAllSubmatchesOnSourceLine(t *testing.T) {
	root := t.TempDir()
	reviewRgWrite(t, root, "text.txt", "hit hit\nhit hit hit\n")
	ctx := rgCtx(t, root)
	for offset, count := range []int{2, 3} {
		ctx.Args = map[string]any{"pattern": "hit", "only_matching": true, "limit": 1, "offset": offset}
		result, page := runRgProgramPage(t, ctx)
		if len(page.Entries) != count || page.NextOffset != offset+1 {
			t.Fatalf("same-line submatches were split or lost: %s", result.Data)
		}
		for _, entry := range page.Entries {
			if entry.Line != offset+1 || entry.Text != "hit" || !entry.Match {
				t.Fatalf("wrong submatch source: %s", result.Data)
			}
		}
		if strings.Count(result.Text, fmt.Sprintf("text.txt:%d:hit\n", offset+1)) != count {
			t.Fatalf("text lost a submatch: %s", result.Text)
		}
	}
}

func TestRgOnlyMatchingMultilinePreservesSourceNumbers(t *testing.T) {
	root := t.TempDir()
	reviewRgWrite(t, root, "text.txt", "alpha B\nC alpha\nalpha alpha\n")
	ctx := rgCtx(t, root)
	for offset, want := range [][]string{{"alpha", "B"}, {"C", "alpha"}, {"alpha", "alpha"}} {
		ctx.Args = map[string]any{"pattern": "alpha|B\\nC", "multiline": true, "only_matching": true, "limit": 1, "offset": offset}
		result, page := runRgProgramPage(t, ctx)
		if len(page.Entries) != len(want) || page.NextOffset != offset+1 {
			t.Fatalf("wrong multiline page: %s", result.Data)
		}
		for i, entry := range page.Entries {
			if entry.Line != offset+1 || entry.Text != want[i] {
				t.Fatalf("wrong multiline submatch source: %s", result.Data)
			}
		}
	}
}

func TestRgProgramFilePagination(t *testing.T) {
	root := t.TempDir()
	reviewRgWrite(t, root, "a.txt", "hit\n")
	reviewRgWrite(t, root, "b.txt", "hit\nhit\n")
	reviewRgWrite(t, root, "c.txt", "hit\n")
	ctx := rgCtx(t, root)
	for _, mode := range []string{rgModeFiles, rgModeCount} {
		ctx.Args = map[string]any{"pattern": "hit", "mode": mode, "limit": 1, "offset": 1}
		result, page := runRgProgramPage(t, ctx)
		if len(page.Entries) != 1 || page.Entries[0].Path != "b.txt" || page.NextOffset != 2 || !page.Truncated {
			t.Fatalf("wrong file page: %s", result.Data)
		}
		want := "b.txt\n"
		if mode == rgModeCount {
			want = "b.txt:2\n"
			if page.Entries[0].Count != 2 {
				t.Fatalf("wrong match count: %s", result.Data)
			}
		}
		if !strings.Contains(result.Text, want) {
			t.Fatalf("text and program page differ: %s\n%s", result.Text, result.Data)
		}
	}
}

func TestRgProgramReportsTextClipping(t *testing.T) {
	root := t.TempDir()
	reviewRgWrite(t, root, "text.txt", "hit "+strings.Repeat("x", 100)+"\n")
	ctx := rgCtx(t, root)
	ctx.OutputLimit = 64
	ctx.Args = map[string]any{"pattern": "hit"}
	result, page := runRgProgramPage(t, ctx)
	if !result.DataTruncated || !page.Truncated || len(result.Text) > ctx.OutputLimit || len(page.Entries) != 1 {
		t.Fatalf("text clipping not reported: %+v", result)
	}
}
