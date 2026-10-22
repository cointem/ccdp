package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"ccdp/internal/protocol"
)

func TestReadOutputTextAndDataContinuation(t *testing.T) {
	for _, content := range []string{"", strings.Repeat("a中文\n", 300)} {
		ctx := &Context{OutputLimit: 300}
		ctx.ReadOutput = func(_ context.Context, id string, offset int64, limit int) (protocol.OutputPage, error) {
			if id != "saved-result" {
				t.Fatal(id)
			}
			end := min(len(content), int(offset)+limit)
			for end < len(content) && !utf8.RuneStart(content[end]) {
				end--
			}
			return protocol.OutputPage{Text: content[offset:end], Next: int64(end), Total: int64(len(content)), More: end < len(content)}, nil
		}
		var recovered strings.Builder
		for offset, calls := int64(0), 0; ; calls++ {
			if calls > 100 {
				t.Fatal("pagination stalled")
			}
			r, err := NewReadOutputTool().Execute(ctx, map[string]any{"output_id": "saved-result", "offset": offset, "limit": 65536})
			if err != nil || len(r.Text) > ctx.OutputLimit || !utf8.ValidString(r.Text) {
				t.Fatalf("invalid text page: %q %v", r.Text, err)
			}
			var page protocol.OutputPage
			if err := json.Unmarshal(r.Data, &page); err != nil {
				t.Fatal(err)
			}
			if page.Next != offset+int64(len(page.Text)) || r.DataTruncated != page.More {
				t.Fatalf("cursor includes footer: %+v", page)
			}
			recovered.WriteString(page.Text)
			if !page.More {
				if !strings.Contains(r.Text, "EOF]") {
					t.Fatal("missing EOF", r.Text)
				}
				break
			}
			if page.Next <= offset || !strings.Contains(r.Text, fmt.Sprintf("ReadOutput output_id=\"saved-result\" offset=%d", page.Next)) {
				t.Fatal("missing usable continuation", r.Text)
			}
			offset = page.Next
		}
		if recovered.String() != content {
			t.Fatal("pagination lost or duplicated content")
		}
	}
}

func TestReadLongLinePreviewAndSafeExtraction(t *testing.T) {
	ctx := freshCtx(t)
	path := filepath.Join(ctx.WorkingDir, "long ' $(echo nope).json")
	content := strings.Repeat("中文abcdef", 10000) + "\nlast\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	for _, budget := range []int{256, 32000} {
		ctx.OutputLimit = budget
		r, err := NewReadTool().Execute(ctx, map[string]any{"file_path": path})
		if err != nil || len(r.Text) > budget || !utf8.ValidString(r.Text) {
			t.Fatalf("bad preview: %q %v", r.Text, err)
		}
		var data struct {
			Preview string
			Lines   []any
			Next    int `json:"next_offset"`
			EOF     bool
		}
		if err := json.Unmarshal(r.Data, &data); err != nil {
			t.Fatal(err)
		}
		if data.Preview == "" || !strings.HasPrefix(content, data.Preview) || len(data.Lines) != 0 || data.Next != 1 || data.EOF || !r.DataTruncated {
			t.Fatalf("preview advertised as a complete line: %s", r.Data)
		}
		if budget == 32000 {
			_, command, ok := strings.Cut(r.Text, "bounded extraction: ")
			if !ok {
				t.Fatal("missing extraction command", r.Text)
			}
			command, _, _ = strings.Cut(command, ". Read offset=")
			out, err := exec.Command("/bin/sh", "-c", command).Output()
			if err != nil || string(out) != content[:4096] {
				t.Fatalf("unsafe/incorrect extraction: %q %v", command, err)
			}
		}
	}
	if _, err := ctx.Resources.Files.ObservedVersion(path); err == nil {
		t.Fatal("preview granted edit observation")
	}
}

func TestReadLongLinePreviewRejectsBinary(t *testing.T) {
	for _, prefix := range []string{"\x00", "\xff"} {
		ctx := freshCtx(t)
		path := filepath.Join(ctx.WorkingDir, "binary")
		if err := os.WriteFile(path, []byte(prefix+strings.Repeat("x", 100000)), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewReadTool().Execute(ctx, map[string]any{"file_path": path}); err == nil {
			t.Fatal("binary preview accepted")
		}
	}
}
