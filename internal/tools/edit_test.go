package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEditReportsBatchFailuresWithoutPartialWrite(t *testing.T) {
	ctx := freshCtx(t)
	path := filepath.Join(ctx.WorkingDir, "raft.go")
	original := "// leader heartbeat interval\ninterval = 120\nfunc first() {\n\treturn\n}\nfunc second() {\n\treturn\n}\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	writes := 0
	ctx.BeforeWrite = func(string, []byte) (func() error, error) {
		writes++
		return func() error { return nil }, nil
	}
	result, err := NewEditTool().Execute(ctx, map[string]any{"file_path": path, "edits": []any{
		map[string]any{"old_text": "interval = 120", "new_text": "interval = 150"},
		map[string]any{"old_text": "\treturn", "new_text": "\treturn true"},
		map[string]any{"old_text": "// leader heartbeat interval\ninterval = 150", "new_text": "new"},
	}})
	if err == nil || result.Failure == nil || result.Failure.SideEffect != "none" || result.Failure.RetryClass != "after_correction" {
		t.Fatalf("lost non-publication: %+v %v", result, err)
	}
	for _, want := range []string{"0 edits applied; file unchanged", "entry 2", "lines 4, 7", "entry 3", "2\tinterval = 120"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %q: %v", want, err)
		}
	}
	if data, e := os.ReadFile(path); e != nil || string(data) != original || writes != 0 {
		t.Fatalf("partial mutation: %q %d %v", data, writes, e)
	}
	_, err = NewEditTool().Execute(ctx, map[string]any{"file_path": path, "edits": []any{
		map[string]any{"old_text": original, "new_text": "replacement"},
		map[string]any{"old_text": "120", "new_text": "150"},
		map[string]any{"old_text": "second", "new_text": "last"},
	}})
	if err == nil || !strings.Contains(err.Error(), "entries 1 and 2 overlap") || !strings.Contains(err.Error(), "entries 1 and 3 overlap") || writes != 0 {
		t.Fatalf("nested overlaps not diagnosed together: %v", err)
	}
	// The diagnostic supplies the current text; a corrected call needs no Read.
	result, err = NewEditTool().Execute(ctx, map[string]any{"file_path": path, "edits": []any{
		map[string]any{"old_text": "interval = 120", "new_text": "interval = 150"},
		map[string]any{"old_text": "func first() {\n\treturn", "new_text": "func first() {\n\treturn true"},
	}})
	if err != nil || writes != 1 || !strings.Contains(result.Text, "Applied 2 edits") {
		t.Fatalf("correction failed: %s %v", result.Text, err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != strings.Replace(strings.Replace(original, "interval = 120", "interval = 150", 1), "\treturn", "\treturn true", 1) {
		t.Fatal(string(data))
	}
}

func TestEditNormalizesOnlyBOMAndLineEndings(t *testing.T) {
	for _, tc := range []struct {
		name, content, old, next, want string
		reject                         bool
	}{
		{"crlf", "\uFEFFkeep\r\nold\r\nend\r\n", "keep\nold\n", "keep\nnew\nextra\n", "\uFEFFkeep\r\nnew\r\nextra\r\nend\r\n", false},
		{"lf", "keep\nold\nend", "old\r\n", "new\r\n", "keep\nnew\nend", false},
		{"mixed", "first\r\nold\nlast\r\n", "old\r\n", "new\n", "first\r\nnew\nlast\r\n", false},
		{"newline boundary", "a\r\nb", "\n", "\nextra\n", "a\r\nextra\r\nb", false},
		{"overlapping", "aaaa", "aa", "b", "", true},
		{"normalized ambiguity", "x\r\nx\n", "x\n", "new", "", true},
		{"whitespace", "  old\n", "old \n", "new", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := freshCtx(t)
			path := filepath.Join(ctx.WorkingDir, "file")
			if err := os.WriteFile(path, []byte(tc.content), 0640); err != nil {
				t.Fatal(err)
			}
			_, err := NewEditTool().Execute(ctx, editArgs(path, tc.old, tc.next))
			if (err != nil) != tc.reject {
				t.Fatalf("unexpected outcome: %v", err)
			}
			want := tc.want
			if tc.reject {
				want = tc.content
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != want {
				t.Fatalf("got %q, want %q: %v", got, want, err)
			}
		})
	}
}

func TestEditSummaryOmitsLargeUnchangedAnchors(t *testing.T) {
	ctx := freshCtx(t)
	path := filepath.Join(ctx.WorkingDir, "file")
	context := strings.Repeat("unchanged context\n", 1000)
	before, after := context+"old value\n"+context, context+"new value\n"+context
	if err := os.WriteFile(path, []byte(before), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := NewEditTool().Execute(ctx, editArgs(path, before, after))
	if err != nil || len(result.Text) > 3*len(path)+200 || strings.Contains(result.Text, "unchanged context") || !strings.Contains(result.Text, "@@ -1001,1 +1001,1 @@\n-old value\n+new value") {
		t.Fatalf("noisy/incorrect diff: %s %v", result.Text, err)
	}
}

func TestEditSummaryUsesFinalLinesAndNewlineChanges(t *testing.T) {
	for _, tc := range []struct {
		before, after string
		edits         []editReplacement
		want          string
	}{
		{"a=1 b=2\n", "a=10 b=20\n", []editReplacement{{start: 2, end: 3, next: "10"}, {start: 6, end: 7, next: "20"}}, "-a=1 b=2\n+a=10 b=20\n"},
		{"a\nb\n", "ab\n", []editReplacement{{start: 1, end: 2, next: ""}}, "@@ -1,2 +1,1 @@\n-a\n-b\n+ab\n"},
		{"old\n", "", []editReplacement{{start: 0, end: 4, next: ""}}, "@@ -1,1 +0,0 @@\n-old\n"},
		{"old\n", "old", []editReplacement{{start: 3, end: 4, next: ""}}, "@@ -1,1 +1,1 @@\n-old\n+old\n\\ No newline at end of file\n"},
	} {
		got := editSummary("file", tc.before, tc.after, tc.edits)
		if !strings.Contains(got, tc.want) {
			t.Fatalf("got %q, want %q", got, tc.want)
		}
	}
}
