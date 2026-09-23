package tools

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"ccdp/internal/sandbox"
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
	if err := ctx.checkResources(); err != nil {
		return "", err
	}
	path, err := ctx.ResolveRead(StringArg(ctx.Args, "file_path", ""))
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", fmt.Errorf("Read: missing file_path")
	}
	state, err := ctx.fileStateForUse()
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("Read: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("Read: %s is a directory, use LS", path)
	}

	offset, err := IntArgChecked(ctx.Args, "offset", 0)
	if err != nil {
		return "", fmt.Errorf("Read: %w", err)
	}
	limit, err := IntArgChecked(ctx.Args, "limit", 0)
	if err != nil {
		return "", fmt.Errorf("Read: %w", err)
	}
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 64 * 1024
	}
	if limit > ctx.readLimit() {
		limit = ctx.readLimit()
	}

	readPath := path
	noFollow := false
	if ctx.Sandbox != nil && ctx.Sandbox.CurrentMode() == sandbox.ModeStrict {
		// ResolveRead checks containment using the resolved form but deliberately
		// returns the caller-visible alias. Canonicalize once more at the open
		// boundary so O_NOFOLLOW can reject a final-link swap while preserving
		// legitimate links whose target is inside the workspace.
		readPath, err = filepath.EvalSymlinks(path)
		if err != nil {
			return "", fmt.Errorf("Read: strict path resolution: %w", err)
		}
		if readPath, err = ctx.Sandbox.ResolveRead(readPath); err != nil {
			return "", fmt.Errorf("Read: strict path resolution: %w", err)
		}
		noFollow = true
	}
	data, snapshot, err := readRangeSnapshot(readPath, offset, limit, noFollow)
	if err != nil {
		return "", fmt.Errorf("Read: %w", err)
	}
	// Record the observed mtime so Edit/Write can detect external changes.
	state.MarkFileReadSnapshot(path, snapshot)
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	var sb strings.Builder
	if !snapshot.Complete {
		warning := fmt.Sprintf("[partial read: %d of %d bytes; read from offset 0 with limit=%d before Edit/Write]", len(data), snapshot.Size, snapshot.Size)
		if snapshot.Size > int64(ctx.readLimit()) {
			warning = fmt.Sprintf("[partial read: %d of %d bytes; full-file Edit/Write requires raising the read limit to at least %d and then reading with offset 0 and limit=%d]", len(data), snapshot.Size, snapshot.Size, snapshot.Size)
		}
		appendBounded(&sb, warning+"\n", ctx.outputLimit())
	}
	startLine := countLinesChecked(readPath, offset, noFollow)
	for i, line := range lines {
		appendBounded(&sb, fmt.Sprintf("%d\t%s\n", startLine+i, line), ctx.outputLimit())
	}
	return sb.String(), nil
}

func readRangeChecked(path string, offset, limit int, noFollow bool) ([]byte, error) {
	data, _, err := readRangeSnapshot(path, offset, limit, noFollow)
	return data, err
}

// readRangeSnapshot returns bytes and the exact stat/digest snapshot observed
// from the same descriptor. The shared advisory lock pairs with the exclusive
// lock in writeFileNoFollow for all built-in file mutations.
func readRangeSnapshot(path string, offset, limit int, noFollow bool) ([]byte, FileReadSnapshot, error) {
	flags := os.O_RDONLY
	if noFollow {
		flags |= syscall.O_NOFOLLOW
	}
	f, err := os.OpenFile(path, flags, 0)
	if err != nil {
		return nil, FileReadSnapshot{}, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		return nil, FileReadSnapshot{}, err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	for attempt := 0; attempt < 2; attempt++ {
		before, statErr := f.Stat()
		if statErr != nil {
			return nil, FileReadSnapshot{}, statErr
		}
		buf := make([]byte, limit)
		if _, err := f.Seek(int64(offset), 0); err != nil {
			return nil, FileReadSnapshot{}, err
		}
		n, readErr := f.Read(buf)
		if readErr != nil && readErr != io.EOF && n == 0 {
			return nil, FileReadSnapshot{}, readErr
		}
		after, statErr := f.Stat()
		if statErr != nil {
			return nil, FileReadSnapshot{}, statErr
		}
		beforeDev, beforeInode := fileIdentity(before)
		afterDev, afterInode := fileIdentity(after)
		if before.ModTime().Equal(after.ModTime()) && before.Size() == after.Size() &&
			beforeDev == afterDev && beforeInode == afterInode {
			snapshot := FileReadSnapshot{
				Mtime: after.ModTime(), Size: after.Size(), Dev: afterDev, Inode: afterInode,
			}
			// A bounded/ranged read is intentionally not a full-file
			// fingerprint. Edit/Write must require a subsequent complete Read,
			// otherwise a prefix hash could be mistaken for freshness of a large
			// file. An offset-zero read that reaches EOF is complete even when
			// its requested limit exceeds the file size.
			if offset <= 0 && int64(n) == after.Size() {
				snapshot.Digest = sha256.Sum256(buf[:n])
				snapshot.Complete = true
			}
			return buf[:n], snapshot, nil
		}
	}
	return nil, FileReadSnapshot{}, fmt.Errorf("file changed while it was being read")
}

// countLines estimates the 1-based line number at a byte offset. It reads in
// fixed 64KB chunks (an offset-sized allocation would let a huge offset OOM
// the process) and loops to survive short reads.
func countLines(path string, offset int) int {
	return countLinesChecked(path, offset, false)
}

func countLinesChecked(path string, offset int, noFollow bool) int {
	if offset <= 0 {
		return 1
	}
	flags := os.O_RDONLY
	if noFollow {
		flags |= syscall.O_NOFOLLOW
	}
	f, err := os.OpenFile(path, flags, 0)
	if err != nil {
		return 1
	}
	defer f.Close()

	buf := make([]byte, 64*1024)
	lines := 1
	for remaining := offset; remaining > 0; {
		chunk := buf
		if remaining < len(chunk) {
			chunk = chunk[:remaining]
		}
		n, err := f.Read(chunk)
		lines += bytes.Count(chunk[:n], []byte("\n"))
		remaining -= n
		if err != nil || n == 0 {
			break
		}
	}
	return lines
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
	if err := ctx.checkResources(); err != nil {
		return "", err
	}
	path, err := ctx.ResolveWrite(StringArg(ctx.Args, "file_path", ""))
	if err != nil {
		return "", err
	}
	content := StringArg(ctx.Args, "content", "")
	if path == "" {
		return "", fmt.Errorf("Write: missing file_path")
	}
	state, err := ctx.fileStateForUse()
	if err != nil {
		return "", err
	}
	state.writeMu.Lock()
	defer state.writeMu.Unlock()
	// Overwriting an existing file requires a fresh Read (file safety); new
	// files are exempt.
	if _, err := os.Stat(path); err == nil {
		if err := state.CheckFileFresh(path); err != nil {
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
	if err := writeFileNoFollow(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("Write: %w", err)
	}
	// The agent just wrote it: refresh the record so a follow-up Edit works.
	state.MarkFileRead(path)
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
	if err := ctx.checkResources(); err != nil {
		return "", err
	}
	path, err := ctx.ResolveWrite(StringArg(ctx.Args, "file_path", ""))
	if err != nil {
		return "", err
	}
	oldStr := StringArg(ctx.Args, "old_string", "")
	newStr := StringArg(ctx.Args, "new_string", "")
	if path == "" || oldStr == "" {
		return "", fmt.Errorf("Edit: file_path and old_string are required")
	}
	state, err := ctx.fileStateForUse()
	if err != nil {
		return "", err
	}
	state.writeMu.Lock()
	defer state.writeMu.Unlock()
	// File safety (Claude Code's freshness gate): the file must have been read
	// this session and must not have changed on disk since.
	if err := state.CheckFileFresh(path); err != nil {
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
	if err := writeFileNoFollow(path, []byte(updated), 0o644); err != nil {
		return "", fmt.Errorf("Edit: %w", err)
	}
	// The agent just wrote it: refresh the record so a follow-up Edit works.
	state.MarkFileRead(path)
	// A short diff summary helps the model verify the change.
	return diffSummary(oldStr, newStr), nil
}

// appendBounded appends at most limit bytes and records truncation explicitly.
// A tool result is model-visible output, so it must remain bounded even when
// line-number decoration makes it larger than the input read.
func appendBounded(sb *strings.Builder, value string, limit int) {
	if limit <= 0 {
		return
	}
	remaining := limit - sb.Len()
	if remaining <= 0 {
		return
	}
	if len(value) <= remaining {
		sb.WriteString(value)
		return
	}
	const marker = "\n…[tool output truncated]"
	if remaining <= len(marker) {
		sb.WriteString(value[:remaining])
		return
	}
	sb.WriteString(value[:remaining-len(marker)])
	sb.WriteString(marker)
}

// writeFileNoFollow rejects a final symlink immediately before opening it and
// uses O_NOFOLLOW where available. ResolveWrite performs the workspace check;
// this second check closes the common symlink-swap gap between resolution and
// mutation. Kernel sandboxing remains the authoritative boundary in strict
// mode, since userspace checks alone cannot eliminate all TOCTOU races.
func writeFileNoFollow(path string, data []byte, mode os.FileMode) error {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to write through symlink %s", path)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|syscall.O_NOFOLLOW, mode)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	// Re-check the opened descriptor, not just the path resolved by Sandbox.
	// A rename or hard-link can race the path check; refusing a multiply-linked
	// target here prevents truncating the protected inode through an alias.
	if info, err := f.Stat(); err != nil {
		return err
	} else if fileInfoHasMultipleLinks(info) {
		return fmt.Errorf("refusing to write multiply-linked file %s", path)
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Close()
}

func fileInfoHasMultipleLinks(info os.FileInfo) bool {
	if info == nil || !info.Mode().IsRegular() {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink > 1
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
