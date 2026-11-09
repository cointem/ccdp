package tools

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"ccdp/internal/atomicfile"
	"ccdp/internal/execution"
	"ccdp/internal/fsops"
)

type ReadTool struct{}

func NewReadTool() *ReadTool   { return &ReadTool{} }
func (*ReadTool) Name() string { return "Read" }
func (*ReadTool) Description() string {
	return "Read UTF-8 text with line numbers. Relative paths start at the session workspace, not a previous Bash command's cwd; use absolute paths for Bash-created files and log_path. Paths are literal: shell variables and ~ are not expanded. offset is a 1-based LINE number, limit a LINE count (maximum 2000); do not pass Process byte offsets here. Results include actual range and next_offset. Use Glob/LS for paths and Rg for text search."
}
func (*ReadTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"file_path": map[string]any{"type": "string"}, "offset": map[string]any{"default": 1, "type": "integer", "minimum": 1}, "limit": map[string]any{"default": 2000, "type": "integer", "minimum": 1, "maximum": 2000}}, "required": []string{"file_path"}}
}
func (*ReadTool) Execute(ctx *Context, args map[string]any) (Result, error) {
	return executeFileWorker(ctx, "Read", args)
}

// executeRead runs only inside the fixed file worker.
func executeRead(ctx *Context, args map[string]any) (Result, error) {
	return executeText(ctx, args, func(ctx *Context) (string, error) {
		if e := ctx.checkResources(); e != nil {
			return "", e
		}
		raw := StringArg(ctx.Args, "file_path", "")
		if raw == "" {
			return "", fmt.Errorf("Read: missing file_path")
		}
		path, e := ctx.ResolveRead(raw)
		if e != nil {
			return "", e
		}
		offset, e := IntArgChecked(ctx.Args, "offset", 1)
		if e != nil || offset < 1 {
			return "", fmt.Errorf("Read: offset must be a positive line number")
		}
		limit, e := IntArgChecked(ctx.Args, "limit", 2000)
		if e != nil || limit < 1 || limit > 2000 {
			return "", fmt.Errorf("Read: limit must be 1..2000 lines")
		}
		state, e := ctx.fileStateForUse()
		if e != nil {
			return "", e
		}
		f, e := fsops.OpenAuthorizedRegular(ctx.ReadRoot, path, ctx.ResolveRead)
		if e != nil {
			return "", e
		}
		defer f.Close()
		info, e := f.Stat()
		if e != nil {
			return "", e
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("Read: not a regular file; use LS for directories")
		}
		budget := min(50*1024, ctx.readLimit(), ctx.outputLimit())
		if budget < 256 {
			return "", fmt.Errorf("Read: output budget too small (minimum 256 bytes)")
		}
		r := bufio.NewReaderSize(f, budget)
		line := 1
		for line < offset {
			if err := ctx.checkResources(); err != nil {
				return "", err
			}
			_, e = r.ReadSlice('\n')
			if e == bufio.ErrBufferFull {
				continue
			}
			if e == io.EOF {
				return "", fmt.Errorf("Read: offset %d is beyond EOF", offset)
			}
			if e != nil {
				return "", e
			}
			line++
		}
		var out strings.Builder
		lines := make([]map[string]any, 0)
		count := 0
		eof := false
		longLine := false
		var preview string
		budgetStopped := false
		for count < limit {
			if err := ctx.checkResources(); err != nil {
				return "", err
			}
			part, err := r.ReadSlice('\n')
			if err != nil && err != io.EOF && err != bufio.ErrBufferFull {
				return "", err
			}
			if err == bufio.ErrBufferFull {
				if count == 0 {
					longLine = true
					preview = string(part)
				} else {
					budgetStopped = true
				}
				break
			}
			if len(part) == 0 && err == io.EOF {
				eof = true
				break
			}
			if !utf8.Valid(part) || bytes.IndexByte(part, 0) >= 0 {
				return "", fmt.Errorf("Read: binary or unsupported text encoding; use an appropriate binary tool")
			}
			decorated := fmt.Sprintf("%d\t%s", line, part)
			if !strings.HasSuffix(decorated, "\n") {
				decorated += "\n"
			}
			if out.Len()+len(decorated) > budget-180 {
				budgetStopped = true
				if count == 0 {
					longLine = true
					preview = string(part)
				}
				break
			}
			out.WriteString(decorated)
			lines = append(lines, map[string]any{"number": line, "text": strings.TrimSuffix(string(part), "\n")})
			count++
			line++
			if err == io.EOF {
				eof = true
				break
			}
		}
		if !eof && !longLine && !budgetStopped {
			_, err := r.Peek(1)
			eof = err == io.EOF
			if err != nil && err != io.EOF {
				return "", err
			}
		}
		if count == 0 && eof && offset > 1 {
			return "", fmt.Errorf("Read: offset %d is beyond EOF", offset)
		}
		if err := ctx.checkResources(); err != nil {
			return "", err
		}
		after, e := f.Stat()
		if e != nil {
			return "", e
		}
		if fsops.FromInfo(info) != fsops.FromInfo(after) {
			return "", fmt.Errorf("Read: file changed while reading; retry the requested region")
		}
		if count > 0 || eof {
			state.MarkVersion(path, fsops.FromInfo(after))
		}
		data := map[string]any{"path": path, "lines": lines, "offset": offset, "end": line - 1, "next_offset": line, "eof": eof, "truncated": !eof}
		if count == 0 && eof {
			ctx.recordData(data, false)
			return "[empty file; EOF]", nil
		}
		if longLine {
			// A preview is a partial observation, but not a complete line:
			// keep its content separate and do not advance the line cursor.
			note := fmt.Sprintf("[line %d exceeds output budget; preview only. Use Bash for bounded extraction: sed -n '%dp' %s | head -c 4096. Read offset=%d skips this line.]", line, line, execution.QuoteArg(path), line+1)
			if len(note)+32 > budget {
				note = fmt.Sprintf("[line %d exceeds output budget; preview only. Use Bash to extract this line in bounded chunks; Read offset=%d skips it.]", line, line+1)
			}
			preview = preview[:min(len(preview), 2000, max(0, budget-len(note)-32))]
			valid := 0
			for valid < len(preview) && utf8.FullRuneInString(preview[valid:]) {
				r, width := utf8.DecodeRuneInString(preview[valid:])
				if r == 0 || r == utf8.RuneError && width == 1 {
					return "", fmt.Errorf("Read: binary or unsupported text encoding; use an appropriate binary tool")
				}
				valid += width
			}
			preview = preview[:valid]
			data["preview"] = preview
			if preview != "" {
				state.MarkVersion(path, fsops.FromInfo(after))
				fmt.Fprintf(&out, "%d\t%s\n", line, preview)
			}
			out.WriteString(note)
		} else if eof {
			fmt.Fprintf(&out, "[lines %d-%d; EOF]", offset, line-1)
		} else {
			fmt.Fprintf(&out, "[lines %d-%d; more content; next_offset=%d; continue with Read offset=%d (lines)]", offset, line-1, line, line)
		}
		ctx.recordData(data, !eof)
		return out.String(), nil
	})
}

type WriteTool struct{}

func NewWriteTool() *WriteTool  { return &WriteTool{} }
func (*WriteTool) Name() string { return "Write" }
func (*WriteTool) Description() string {
	return "Write a file, creating it if missing. Existing files must have been Read or written by this session and unchanged since then. A partial Read suffices; use Edit for local changes."
}
func (*WriteTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"file_path": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"}}, "required": []string{"file_path", "content"}}
}
func (*WriteTool) Execute(ctx *Context, args map[string]any) (Result, error) {
	return executeFileWorker(ctx, "Write", args)
}

// executeWrite runs only inside the fixed file worker.
func executeWrite(ctx *Context, args map[string]any) (Result, error) {
	return executeText(ctx, args, func(ctx *Context) (output string, runErr error) {
		if e := ctx.checkResources(); e != nil {
			return "", e
		}
		raw := StringArg(ctx.Args, "file_path", "")
		if raw == "" {
			return "", fmt.Errorf("Write: missing file_path")
		}
		content, ok := ctx.Args["content"].(string)
		if !ok {
			return "", fmt.Errorf("Write: content must be explicitly supplied")
		}
		path, e := ctx.ResolveWrite(raw)
		if e != nil {
			return "", e
		}
		state, e := ctx.fileStateForUse()
		if e != nil {
			return "", e
		}
		root, relative, e := ctx.openWriteRoot(path)
		if e != nil {
			return "", e
		}
		defer root.Close()
		var expected *fsops.Version
		if _, e := root.Lstat(relative); e == nil {
			v, e := state.ObservedVersion(path)
			if e != nil {
				return "", e
			}
			expected = &v
		} else if !os.IsNotExist(e) {
			return "", e
		}
		if ctx.BeforeWrite != nil {
			finish, e := ctx.BeforeWrite(path, nil)
			if e != nil {
				return "", e
			}
			defer func() {
				if runErr == nil {
					if err := finish(); err != nil {
						runErr = &atomicfile.PublishedError{Err: err}
					}
				}
			}()
		}
		version, e := fsops.PublishAt(ctx.Context, root, relative, []byte(content), 0644, expected)
		if e != nil {
			return "", e
		}
		state.MarkVersion(path, version)
		return fmt.Sprintf("%s: wrote %d bytes", path, len(content)), nil
	})
}

// Pin an admitted writable root before reading or recording a mutation. Every
// later traversal stays within this handle even if a target ancestor changes.
func (ctx *Context) openWriteRoot(path string) (*os.Root, string, error) {
	// Resolve existing ancestors once, including harmless host aliases such as
	// /var -> /private/var. Keep the final entry unresolved (writes reject links).
	parent, tail := filepath.Dir(path), filepath.Base(path)
	for {
		canonical, err := filepath.EvalSymlinks(parent)
		if err == nil {
			path = filepath.Join(canonical, tail)
			break
		}
		if !os.IsNotExist(err) {
			return nil, "", err
		}
		next := filepath.Dir(parent)
		if next == parent {
			return nil, "", err
		}
		tail, parent = filepath.Join(filepath.Base(parent), tail), next
	}
	if _, err := ctx.ResolveWrite(path); err != nil {
		return nil, "", err
	}
	policy := ctx.Sandbox.Snapshot()
	roots := append([]string{policy.Workspace}, policy.AdditionalDirs...)
	roots = append(roots, policy.ScratchDirs...)
	for _, candidate := range roots {
		canonical, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			continue
		}
		relative, err := filepath.Rel(canonical, path)
		if err != nil || !filepath.IsLocal(relative) {
			continue
		}
		handle, err := os.OpenRoot(canonical)
		if err != nil {
			return nil, "", err
		}
		return handle, relative, nil
	}
	return nil, "", fmt.Errorf("write has no authorized filesystem root: %s", path)
}
