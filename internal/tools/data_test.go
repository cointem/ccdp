package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestReadProgramDataUsesActualPage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte("first\nsecond\nthird"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := scopedTestContext(t, dir)
	ctx.Args = map[string]any{"file_path": "a.txt", "offset": 2, "limit": 1}
	r, err := executeToolResult(NewReadTool(), ctx)
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		Lines []struct {
			Number int
			Text   string
		}
		Next int `json:"next_offset"`
		EOF  bool
	}
	if err = json.Unmarshal(r.Data, &data); err != nil {
		t.Fatal(err)
	}
	if !r.DataTruncated || data.EOF || data.Next != 3 || len(data.Lines) != 1 || data.Lines[0].Number != 2 || data.Lines[0].Text != "second" {
		t.Fatalf("%s truncated %v", r.Data, r.DataTruncated)
	}
	ctx.Args = map[string]any{"file_path": "a.txt", "offset": 3}
	r, err = executeToolResult(NewReadTool(), ctx)
	if err != nil || r.DataTruncated {
		t.Fatalf("%s %v", r.Data, err)
	}
}
func TestGlobAndLSProgramData(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0600)
	_ = os.WriteFile(filepath.Join(dir, "b.txt"), []byte("y"), 0600)
	ctx := scopedTestContext(t, dir)
	ctx.Args = map[string]any{"pattern": "*.txt", "limit": 1}
	r, err := executeToolResult(NewGlobTool(), ctx)
	if err != nil || !r.DataTruncated {
		t.Fatalf("%s %v", r.Data, err)
	}
	var glob struct {
		Paths []string
		Total int
	}
	_ = json.Unmarshal(r.Data, &glob)
	if len(glob.Paths) != 1 || glob.Total != 2 {
		t.Fatal(string(r.Data))
	}
	ctx.Args = map[string]any{}
	r, err = executeToolResult(NewLSTool(), ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ls struct {
		Entries []struct {
			Name string
			Size int
		}
	}
	_ = json.Unmarshal(r.Data, &ls)
	found := false
	for _, e := range ls.Entries {
		if e.Name == "a.txt" && e.Size == 1 {
			found = true
		}
	}
	if !found {
		t.Fatal(string(r.Data))
	}
}
