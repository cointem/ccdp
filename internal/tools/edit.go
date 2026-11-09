package tools

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"ccdp/internal/atomicfile"
	"ccdp/internal/fsops"
)

type EditTool struct{}

func NewEditTool() *EditTool   { return &EditTool{} }
func (*EditTool) Name() string { return "Edit" }
func (*EditTool) Description() string {
	return "Apply unique, nonoverlapping text replacements in one file. Every old_text matches the original current content; if any entry fails, none are applied. Include enough context to identify one occurrence. Exact text from Read, Rg, Bash or the user suffices; no prerequisite Read. BOM and CRLF/LF differences are tolerated; other text must match exactly. Explicit empty new_text deletes text."
}
func (*EditTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"file_path": map[string]any{"type": "string"}, "edits": map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "object", "properties": map[string]any{"old_text": map[string]any{"type": "string", "minLength": 1}, "new_text": map[string]any{"type": "string"}}, "required": []string{"old_text", "new_text"}}}}, "required": []string{"file_path", "edits"}}
}
func (*EditTool) Execute(ctx *Context, args map[string]any) (Result, error) {
	return executeFileWorker(ctx, "Edit", args)
}

// executeEdit runs only inside the fixed file worker.
func executeEdit(ctx *Context, args map[string]any) (Result, error) {
	return executeText(ctx, args, func(ctx *Context) (output string, runErr error) {
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
			return "", fmt.Errorf("Edit: supply edits=[{old_text,new_text}]")
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
		data, version, e := fsops.ReadAt(root, relative, 32<<20)
		if e != nil {
			return "", e
		}
		if !utf8.Valid(data) {
			return "", fmt.Errorf("Edit: unsupported text encoding")
		}
		content := string(data)
		text, positions := editText(content)
		var replacements []editReplacement
		var diagnostics []string
		problems := 0
		reject := func(message string) {
			problems++
			if len(diagnostics) < 8 {
				diagnostics = append(diagnostics, message)
			}
		}
		for i, item := range edits {
			if err := ctx.checkResources(); err != nil {
				return "", err
			}
			m, ok := item.(map[string]any)
			if !ok {
				reject(fmt.Sprintf("entry %d: expected {old_text,new_text}", i+1))
				continue
			}
			old, ok := m["old_text"].(string)
			next, nok := m["new_text"].(string)
			old = normalizeEditText(old)
			if !ok || old == "" || !nok {
				reject(fmt.Sprintf("entry %d: requires nonempty old_text and explicit new_text", i+1))
				continue
			}
			matches := editMatches(text, old)
			if len(matches) == 0 {
				message := fmt.Sprintf("entry %d: old_text not found. Use the current text; earlier entries in this batch have not been applied.", i+1)
				if anchor := editAnchor(text, old); anchor >= 0 {
					message += "\nCurrent region near an unchanged anchor:\n" + editExcerpt(text, anchor)
				} else {
					message += " Read or Rg the intended region."
				}
				reject(message)
				continue
			}
			if len(matches) > 1 {
				var locations []string
				for _, at := range matches {
					locations = append(locations, fmt.Sprint(1+strings.Count(text[:at], "\n")))
				}
				more := ""
				if len(matches) == 4 {
					more = " (first 4 matches)"
				}
				reject(fmt.Sprintf("entry %d: old_text is not unique; matches at lines %s%s. Include surrounding context.\n%s", i+1, strings.Join(locations, ", "), more, editExcerpt(text, matches[0])))
				continue
			}
			at := matches[0]
			start, end := positions(at), positions(at+len(old))
			next = strings.ReplaceAll(next, "\r\n", "\n")
			if at == 0 {
				next = strings.TrimPrefix(next, "\uFEFF")
			}
			// Use the replacement region's newline convention; untouched bytes,
			// including mixed line endings elsewhere and a leading BOM, stay intact.
			region := content[start:end]
			if !strings.ContainsRune(region, '\n') {
				region = content[start:]
				if !strings.ContainsRune(region, '\n') {
					region = content
				}
			}
			if nl := strings.IndexByte(region, '\n'); nl > 0 && region[nl-1] == '\r' {
				next = strings.ReplaceAll(next, "\n", "\r\n")
			}
			replacements = append(replacements, editReplacement{start: start, end: end, entry: i + 1, next: next})
		}
		sort.Slice(replacements, func(i, j int) bool { return replacements[i].start < replacements[j].start })
		for i, furthest := 1, 0; i < len(replacements); i++ {
			if replacements[i].start < replacements[furthest].end {
				reject(fmt.Sprintf("entries %d and %d overlap at line %d; merge them into one replacement", replacements[furthest].entry, replacements[i].entry, 1+strings.Count(content[:replacements[i].start], "\n")))
			}
			if replacements[i].end > replacements[furthest].end {
				furthest = i
			}
		}
		if problems > 0 {
			if problems > len(diagnostics) {
				diagnostics = append(diagnostics, fmt.Sprintf("%d more invalid entries omitted.", problems-len(diagnostics)))
			}
			return "", fmt.Errorf("Edit: %s\n%s", path, strings.Join(diagnostics, "\n"))
		}
		var b strings.Builder
		pos := 0
		changed := replacements[:0]
		for _, r := range replacements {
			b.WriteString(content[pos:r.start])
			b.WriteString(r.next)
			pos = r.end
			old := content[r.start:r.end]
			if old != r.next {
				changed = append(changed, r)
			}
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
					if err := finish(); err != nil {
						runErr = &atomicfile.PublishedError{Err: err}
					}
				}
			}()
		}
		version, e = fsops.PublishAt(ctx.Context, root, relative, []byte(updated), 0644, &version)
		if e != nil {
			return "", e
		}
		state.MarkVersion(path, version)
		return boundedToolString(ctx, fmt.Sprintf("Applied %d edits to %s.\n%s", len(changed), path, editSummary(path, content, updated, changed))), nil
	})
}

type editReplacement struct {
	start, end, entry int
	next              string
}

func normalizeEditText(text string) string {
	return strings.ReplaceAll(strings.TrimPrefix(text, "\uFEFF"), "\r\n", "\n")
}

// Map normalized byte boundaries back to the original file without allocating
// an index for every byte. Only CRLF positions need an entry.
func editText(content string) (string, func(int) int) {
	body := strings.TrimPrefix(content, "\uFEFF")
	bom := len(content) - len(body)
	var removed []int
	for start := 0; ; {
		i := strings.Index(body[start:], "\r\n")
		if i < 0 {
			break
		}
		i += start
		removed = append(removed, i-len(removed))
		start = i + 2
	}
	return normalizeEditText(content), func(at int) int { return bom + at + sort.SearchInts(removed, at) }
}

func editMatches(content, old string) []int {
	var matches []int
	for start := 0; start <= len(content)-len(old) && len(matches) < 4; {
		at := strings.Index(content[start:], old)
		if at < 0 {
			break
		}
		at += start
		matches = append(matches, at)
		start = at + 1 // Include overlapping matches.
	}
	return matches
}

func editAnchor(content, old string) int {
	for i := 0; i < 12 && old != ""; i++ {
		var line string
		line, old, _ = strings.Cut(old, "\n")
		if len(strings.TrimSpace(line)) < 8 {
			continue
		}
		if matches := editMatches(content, line); len(matches) == 1 {
			return matches[0]
		}
	}
	return -1
}

func editExcerpt(content string, at int) string {
	start := strings.LastIndexByte(content[:at], '\n') + 1
	if start > 0 {
		start = strings.LastIndexByte(content[:start-1], '\n') + 1
	}
	line := 1 + strings.Count(content[:start], "\n")
	var out strings.Builder
	for n := 0; n < 5 && start < len(content); n++ {
		end := strings.IndexByte(content[start:], '\n')
		if end < 0 {
			end = len(content) - start
		}
		text := content[start : start+end]
		if len(text) > 200 {
			text = truncateUTF8(text, 200) + "…"
		}
		fmt.Fprintf(&out, "%d\t%s\n", line+n, text)
		start += end + 1
	}
	return out.String()
}

func editSummary(path, before, after string, edits []editReplacement) string {
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", path, path)
	byteDelta, lineDelta := 0, 0
	for i := 0; i < len(edits); {
		start := strings.LastIndexByte(before[:edits[i].start], '\n') + 1
		newStart := start + byteDelta
		oldLine := 1 + strings.Count(before[:start], "\n")
		newLine := oldLine + lineDelta
		end := start
		// Merge changes sharing a line, including a deleted newline that joins
		// the next line. The preview then describes the actual final file.
		for {
			r := edits[i]
			end = len(before)
			if nl := strings.IndexByte(before[r.end:], '\n'); nl >= 0 {
				end = r.end + nl + 1
			}
			byteDelta += len(r.next) - (r.end - r.start)
			lineDelta += strings.Count(r.next, "\n") - strings.Count(before[r.start:r.end], "\n")
			i++
			if i == len(edits) || edits[i].start >= end {
				break
			}
		}
		out.WriteString(editDiff(before[start:end], after[newStart:end+byteDelta], oldLine, newLine))
		if out.Len() > 4096 {
			return truncateUTF8(out.String(), 4096) + "\n[diff preview shortened]"
		}
	}
	return out.String()
}

// Discard shared context and cap changed lines. Large old_text anchors must
// not be echoed twice just to confirm a small successful edit.
func editDiff(old, next string, oldLine, newLine int) string {
	lines := func(text string) []string {
		parts := strings.SplitAfter(text, "\n")
		if parts[len(parts)-1] == "" {
			parts = parts[:len(parts)-1]
		}
		return parts
	}
	before, after := lines(old), lines(next)
	for len(before) > 0 && len(after) > 0 && before[0] == after[0] {
		before, after = before[1:], after[1:]
		oldLine++
		newLine++
	}
	for len(before) > 0 && len(after) > 0 && before[len(before)-1] == after[len(after)-1] {
		before, after = before[:len(before)-1], after[:len(after)-1]
	}
	var out strings.Builder
	if len(before) == 0 {
		oldLine--
	}
	if len(after) == 0 {
		newLine--
	}
	fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", oldLine, len(before), newLine, len(after))
	for i, lines := range [][]string{before, after} {
		prefix := "-"
		if i == 1 {
			prefix = "+"
		}
		for _, line := range lines[:min(len(lines), 8)] {
			text := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
			if len(text) > 240 {
				text = truncateUTF8(text, 240) + "…"
			}
			fmt.Fprintf(&out, "%s%s\n", prefix, text)
			if !strings.HasSuffix(line, "\n") {
				out.WriteString("\\ No newline at end of file\n")
			}
		}
		if len(lines) > 8 {
			fmt.Fprintf(&out, "… %d more %s lines\n", len(lines)-8, prefix)
		}
	}
	return out.String()
}
