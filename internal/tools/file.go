package tools

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"ccdp/internal/fsops"
)

type ReadTool struct{}

func NewReadTool() *ReadTool   { return &ReadTool{} }
func (*ReadTool) Name() string { return "Read" }
func (*ReadTool) Description() string {
	return "Read UTF-8 text with line numbers. offset is a 1-based LINE number, limit a LINE count (maximum 2000). Results include actual range and next_offset. Use Glob/LS for paths and Grep for text search."
}
func (*ReadTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"file_path": map[string]any{"type": "string"}, "offset": map[string]any{"type": "integer", "minimum": 1}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 2000}}, "required": []string{"file_path"}}
}
func (*ReadTool) Run(ctx *Context) (string, error) {
	if e := ctx.checkResources(); e != nil {
		return "", e
	}
	if _, ok := ctx.Args["unit"]; ok {
		return "", fmt.Errorf("Read: unit is no longer supported; offset and limit always use lines")
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
	canonical, e := filepath.EvalSymlinks(path)
	if e != nil {
		return "", e
	}
	if _, e = ctx.ResolveRead(canonical); e != nil {
		return "", e
	}
	f, e := fsops.OpenRegular(ctx.ReadRoot, canonical)
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
	count := 0
	eof := false
	longLine := false
	budgetStopped := false
	for count < limit {
		part, err := r.ReadSlice('\n')
		if err != nil && err != io.EOF && err != bufio.ErrBufferFull {
			return "", err
		}
		if err == bufio.ErrBufferFull {
			longLine = true
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
			}
			break
		}
		out.WriteString(decorated)
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
	if count == 0 && eof {
		return "[empty file; EOF]", nil
	}
	if longLine {
		fmt.Fprintf(&out, "[line %d exceeds output budget; content incomplete. Use Bash for targeted extraction; this Read cannot advance through that line.]", line)
	} else if eof {
		fmt.Fprintf(&out, "[lines %d-%d; EOF]", offset, line-1)
	} else {
		fmt.Fprintf(&out, "[lines %d-%d; more content; next_offset=%d]", offset, line-1, line)
	}
	return out.String(), nil
}

type WriteTool struct{}

func NewWriteTool() *WriteTool  { return &WriteTool{} }
func (*WriteTool) Name() string { return "Write" }
func (*WriteTool) Description() string {
	return "Create a file (mode=create, default), or explicitly replace an existing file (mode=replace). Replacement requires a successful recent Read of the current file version; a partial Read is sufficient. For local changes use Edit."
}
func (*WriteTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"file_path": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"}, "mode": map[string]any{"type": "string", "enum": []string{"create", "replace"}}}, "required": []string{"file_path", "content"}}
}
func (*WriteTool) Run(ctx *Context) (output string, runErr error) {
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
	mode := StringArg(ctx.Args, "mode", "create")
	if mode != "create" && mode != "replace" {
		return "", fmt.Errorf("Write: mode must be create or replace")
	}
	path, e := ctx.ResolveWrite(raw)
	if e != nil {
		return "", e
	}
	state, e := ctx.fileStateForUse()
	if e != nil {
		return "", e
	}
	lock, e := fsops.LockPath(path)
	if e != nil {
		return "", e
	}
	defer lock.Close()
	var expected *fsops.Version
	if mode == "create" {
		if _, e = os.Lstat(path); e == nil {
			return "", fmt.Errorf("Write: file exists; use mode=replace after Read, or Edit")
		} else if !os.IsNotExist(e) {
			return "", e
		}
	} else {
		v, e := state.ObservedVersion(path)
		if e != nil {
			return "", e
		}
		expected = &v
	}
	if ctx.BeforeWrite != nil {
		finish, e := ctx.BeforeWrite(path, nil)
		if e != nil {
			return "", e
		}
		defer func() {
			if runErr == nil {
				runErr = finish()
			}
		}()
	}
	version, e := fsops.Publish(path, []byte(content), 0644, expected)
	if e != nil {
		return "", e
	}
	state.MarkVersion(path, version)
	return fmt.Sprintf("%s: wrote %d bytes (%s)", path, len(content), mode), nil
}

type EditTool struct{}

func NewEditTool() *EditTool   { return &EditTool{} }
func (*EditTool) Name() string { return "Edit" }
func (*EditTool) Description() string {
	return "Apply exact, unique, nonoverlapping text replacements in one file. All edits match the original current content. Reading only the relevant portion is sufficient. A missing new_text is invalid; an explicit empty new_text deletes text. No fuzzy matching."
}
func (*EditTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"file_path": map[string]any{"type": "string"}, "edits": map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "object", "properties": map[string]any{"old_text": map[string]any{"type": "string", "minLength": 1}, "new_text": map[string]any{"type": "string"}}, "required": []string{"old_text", "new_text"}}}}, "required": []string{"file_path", "edits"}}
}
func (*EditTool) Run(ctx *Context) (output string, runErr error) {
	if e := ctx.checkResources(); e != nil {
		return "", e
	}
	raw := StringArg(ctx.Args, "file_path", "")
	if raw == "" {
		return "", fmt.Errorf("Edit: missing file_path")
	}
	path, e := ctx.ResolveWrite(raw)
	if e != nil {
		return "", e
	}
	edits, ok := ctx.Args["edits"].([]any)
	if !ok || len(edits) == 0 {
		return "", fmt.Errorf("Edit: supply edits=[{old_text,new_text}]; legacy old_string/new_string parameters are unsupported")
	}
	state, e := ctx.fileStateForUse()
	if e != nil {
		return "", e
	}
	lock, e := fsops.LockPath(path)
	if e != nil {
		return "", e
	}
	defer lock.Close()
	data, version, e := fsops.Read(path, 32<<20)
	if e != nil {
		return "", e
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("Edit: unsupported text encoding")
	}
	content := string(data)
	type replacement struct {
		start, end int
		old, next  string
	}
	var replacements []replacement
	for i, item := range edits {
		m, ok := item.(map[string]any)
		if !ok {
			return "", fmt.Errorf("Edit: invalid entry %d", i+1)
		}
		old, ok := m["old_text"].(string)
		next, nok := m["new_text"].(string)
		if !ok || old == "" || !nok {
			return "", fmt.Errorf("Edit: entry %d requires nonempty old_text and explicit new_text", i+1)
		}
		at := strings.Index(content, old)
		if at < 0 {
			return "", fmt.Errorf("Edit: old_text in entry %d not found; read the current region", i+1)
		}
		if strings.Contains(content[at+1:], old) {
			return "", fmt.Errorf("Edit: old_text in entry %d is not unique; include more context", i+1)
		}
		replacements = append(replacements, replacement{at, at + len(old), old, next})
	}
	sort.Slice(replacements, func(i, j int) bool { return replacements[i].start < replacements[j].start })
	for i := 1; i < len(replacements); i++ {
		if replacements[i].start < replacements[i-1].end {
			return "", fmt.Errorf("Edit: overlapping replacements; merge them into one")
		}
	}
	var b strings.Builder
	pos := 0
	var summary strings.Builder
	for _, r := range replacements {
		b.WriteString(content[pos:r.start])
		b.WriteString(r.next)
		pos = r.end
		fmt.Fprintf(&summary, "%s:%d\n%s\n", path, 1+strings.Count(content[:r.start], "\n"), diffSummary(r.old, r.next))
	}
	b.WriteString(content[pos:])
	updated := b.String()
	if updated == content {
		return "No changes.", nil
	}
	if ctx.BeforeWrite != nil {
		finish, e := ctx.BeforeWrite(path, data)
		if e != nil {
			return "", e
		}
		defer func() {
			if runErr == nil {
				runErr = finish()
			}
		}()
	}
	version, e = fsops.Publish(path, []byte(updated), 0644, &version)
	if e != nil {
		return "", e
	}
	state.MarkVersion(path, version)
	return boundedToolString(ctx, summary.String()), nil
}

func appendBounded(sb *strings.Builder, value string, limit int) {
	if remaining := limit - sb.Len(); remaining > 0 {
		if len(value) > remaining {
			value = value[:remaining]
			for !utf8.ValidString(value) && len(value) > 0 {
				value = value[:len(value)-1]
			}
		}
		sb.WriteString(value)
	}
}
func diffSummary(old, next string) string {
	return "Applied edit.\n- " + strings.ReplaceAll(strings.TrimSuffix(old, "\n"), "\n", "\n- ") + "\n+ " + strings.ReplaceAll(strings.TrimSuffix(next, "\n"), "\n", "\n+ ")
}
