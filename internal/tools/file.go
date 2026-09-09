package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ---------- Read ----------

// ReadTool reads files with optional offset/limit and line numbers.
type ReadTool struct{}

// NewReadTool creates the read tool.
func NewReadTool() *ReadTool { return &ReadTool{} }

func (t *ReadTool) Name() string { return "Read" }

func (t *ReadTool) Description() string {
	return `Read a file from the filesystem. Supports optional byte offset and limit
for reading large files in chunks. Output is prefixed with line numbers so the
agent can reference exact locations. Never read entire huge files at once.`
}

func (t *ReadTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"file_path": map[string]any{
				"type":        "string",
				"description": "Absolute path to the file to read.",
			},
			"offset": map[string]any{
				"type":        "integer",
				"description": "Optional byte offset to start reading from (default 0).",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Optional number of bytes to read (default 2000 lines worth, capped at 64KB).",
			},
		},
		"required": []string{"file_path"},
	}
}

func (t *ReadTool) Run(ctx *Context) (string, error) {
	path, err := ctx.ResolveRead(StringArg(ctx.Args, "file_path", ""))
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", fmt.Errorf("Read: missing file_path")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("Read: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("Read: %s is a directory, use LS", path)
	}

	offset := IntArg(ctx.Args, "offset", 0)
	limit := IntArg(ctx.Args, "limit", 0)
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 64 * 1024
	}
	if limit > 512*1024 {
		limit = 512 * 1024
	}

	data, err := readRange(path, offset, limit)
	if err != nil {
		return "", fmt.Errorf("Read: %w", err)
	}
	// Record the observed mtime so Edit/Write can detect external changes.
	MarkFileRead(path)
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	var sb strings.Builder
	startLine := countLines(path, offset)
	for i, line := range lines {
		fmt.Fprintf(&sb, "%d\t%s\n", startLine+i, line)
	}
	return sb.String(), nil
}

func readRange(path string, offset, limit int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, limit)
	if _, err := f.Seek(int64(offset), 0); err != nil {
		return nil, err
	}
	n, err := f.Read(buf)
	if err != nil && err.Error() != "EOF" && n == 0 {
		return nil, err
	}
	return buf[:n], nil
}

// countLines estimates the 1-based line number at a byte offset.
func countLines(path string, offset int) int {
	if offset <= 0 {
		return 1
	}
	f, err := os.Open(path)
	if err != nil {
		return 1
	}
	defer f.Close()
	buf := make([]byte, offset)
	n, _ := f.Read(buf)
	return strings.Count(string(buf[:n]), "\n") + 1
}

// ---------- Write ----------

// WriteTool creates or overwrites a file with the given content.
type WriteTool struct{}

// NewWriteTool creates the write tool.
func NewWriteTool() *WriteTool { return &WriteTool{} }

func (t *WriteTool) Name() string { return "Write" }

func (t *WriteTool) Description() string {
	return `Write content to a file, creating it (and parent directories) if needed.
Overwrites the entire file. Use Edit instead when you only need to change a
small part of an existing file. Overwriting an existing file requires reading
it first in this session; new files can be written directly.`
}

func (t *WriteTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"file_path": map[string]any{
				"type":        "string",
				"description": "Absolute path of the file to write.",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "The full content to write to the file.",
			},
		},
		"required": []string{"file_path", "content"},
	}
}

func (t *WriteTool) Run(ctx *Context) (string, error) {
	path, err := ctx.ResolveWrite(StringArg(ctx.Args, "file_path", ""))
	if err != nil {
		return "", err
	}
	content := StringArg(ctx.Args, "content", "")
	if path == "" {
		return "", fmt.Errorf("Write: missing file_path")
	}
	// Overwriting an existing file requires a fresh Read (file safety); new
	// files are exempt.
	if _, err := os.Stat(path); err == nil {
		if err := CheckFileFresh(path); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("Write: %w", err)
	}
	oldLen := 0
	if info, err := os.Stat(path); err == nil {
		oldLen = int(info.Size())
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("Write: %w", err)
	}
	// The agent just wrote it: refresh the record so a follow-up Edit works.
	MarkFileRead(path)
	verb := "wrote"
	if oldLen > 0 {
		verb = "overwrote"
	}
	return fmt.Sprintf("%s %d bytes to %s (previous size %d bytes)", verb, len(content), path, oldLen), nil
}

// ---------- Edit ----------

// EditTool performs exact string replacement in an existing file.
type EditTool struct{}

// NewEditTool creates the edit tool.
func NewEditTool() *EditTool { return &EditTool{} }

func (t *EditTool) Name() string { return "Edit" }

func (t *EditTool) Description() string {
	return `Edit an existing file by replacing an exact string with a new string.
The old_string must match exactly, including whitespace, and must appear
exactly once in the file. Prefer this over Write for surgical changes.

You MUST Read the file in this session before editing it; if it changed on
disk after your Read (by the user or a linter), the edit is rejected — Read
it again and retry.`
}

func (t *EditTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"file_path": map[string]any{
				"type":        "string",
				"description": "Absolute path of the file to edit.",
			},
			"old_string": map[string]any{
				"type":        "string",
				"description": "The exact text to find and replace.",
			},
			"new_string": map[string]any{
				"type":        "string",
				"description": "The replacement text.",
			},
		},
		"required": []string{"file_path", "old_string", "new_string"},
	}
}

func (t *EditTool) Run(ctx *Context) (string, error) {
	path, err := ctx.ResolveWrite(StringArg(ctx.Args, "file_path", ""))
	if err != nil {
		return "", err
	}
	oldStr := StringArg(ctx.Args, "old_string", "")
	newStr := StringArg(ctx.Args, "new_string", "")
	if path == "" || oldStr == "" {
		return "", fmt.Errorf("Edit: file_path and old_string are required")
	}
	// File safety (Claude Code's freshness gate): the file must have been read
	// this session and must not have changed on disk since.
	if err := CheckFileFresh(path); err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("Edit: %w", err)
	}
	content := string(data)
	count := strings.Count(content, oldStr)
	if count == 0 {
		return "", fmt.Errorf("Edit: old_string not found in %s", path)
	}
	if count > 1 {
		return "", fmt.Errorf("Edit: old_string appears %d times in %s; it must be unique. Include more surrounding context", count, path)
	}
	updated := strings.Replace(content, oldStr, newStr, 1)
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return "", fmt.Errorf("Edit: %w", err)
	}
	// The agent just wrote it: refresh the record so a follow-up Edit works.
	MarkFileRead(path)
	// A short diff summary helps the model verify the change.
	return diffSummary(oldStr, newStr), nil
}

func diffSummary(oldStr, newStr string) string {
	lines := func(s string) []string {
		return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	}
	oldLines, newLines := lines(oldStr), lines(newStr)
	n := len(oldLines)
	if len(newLines) > n {
		n = len(newLines)
	}
	var sb strings.Builder
	sb.WriteString("Applied edit. Diff summary:\n")
	for i := 0; i < n; i++ {
		switch {
		case i < len(oldLines) && i < len(newLines):
			if oldLines[i] != newLines[i] {
				fmt.Fprintf(&sb, "  - %s\n  + %s\n", oldLines[i], newLines[i])
			}
		case i < len(oldLines):
			fmt.Fprintf(&sb, "  - %s\n", oldLines[i])
		case i < len(newLines):
			fmt.Fprintf(&sb, "  + %s\n", newLines[i])
		}
	}
	return sb.String()
}
