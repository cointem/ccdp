package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ---------- Glob ----------

// GlobTool finds files by path pattern.
type GlobTool struct{}

// NewGlobTool creates the glob tool.
func NewGlobTool() *GlobTool { return &GlobTool{} }

func (t *GlobTool) Name() string { return "Glob" }

func (t *GlobTool) Description() string {
	return `Find files matching a glob pattern, like "**/*.go" or "src/*.ts".
Returns up to 100 matches sorted by modification time (newest last is typical;
here newest first for visibility). Use Grep to search file contents.`
}

func (t *GlobTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{
				"type":        "string",
				"description": "The glob pattern to match.",
			},
			"path": map[string]any{
				"type":        "string",
				"description": "Optional directory to search in (default: current working directory).",
			},
		},
		"required": []string{"pattern"},
	}
}

func (t *GlobTool) Run(ctx *Context) (string, error) {
	if err := ctx.checkResources(); err != nil {
		return "", err
	}
	if ctx.Sandbox == nil {
		return "", fmt.Errorf("Glob: sandbox policy is unavailable")
	}
	pattern := StringArg(ctx.Args, "pattern", "")
	if pattern == "" {
		return "", fmt.Errorf("Glob: missing pattern")
	}
	base := ctx.WorkingDir
	if rawPath := StringArg(ctx.Args, "path", ""); rawPath != "" {
		var err error
		base, err = ctx.ResolveRead(rawPath)
		if err != nil {
			return "", err
		}
	}
	// Even the default working directory must be admitted by the same policy as
	// an explicit path. In particular, it may have become disallowed after the
	// tool context was created.
	base, err := ctx.ResolveRead(base)
	if err != nil {
		return "", fmt.Errorf("Glob: %w", err)
	}
	fullPattern := pattern
	if !filepath.IsAbs(fullPattern) {
		fullPattern = filepath.Join(base, fullPattern)
	}
	fullPattern = filepath.Clean(fullPattern)
	anchor, parts, err := globPatternAnchor(fullPattern)
	if err != nil {
		return "", fmt.Errorf("Glob: bad pattern: %w", err)
	}
	// Match only below the literal prefix before the first wildcard. Requiring
	// that prefix to be readable prevents filepath.Glob from probing arbitrary
	// parent directories while expanding an absolute or ../ pattern.
	if _, err := ctx.ResolveRead(anchor); err != nil {
		return "", fmt.Errorf("Glob: %w", err)
	}
	matches, err := scopedGlob(ctx, anchor, parts)
	if err != nil {
		return "", fmt.Errorf("Glob: %w", err)
	}

	sort.Strings(matches)
	const max = 100
	if len(matches) > max {
		matches = matches[:max]
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Found %d matching path(s):\n", len(matches))
	for _, m := range matches {
		fmt.Fprintf(&sb, "  %s\n", m)
	}
	return boundedToolString(ctx, sb.String()), nil
}

// globPatternAnchor returns the literal directory prefix before the first
// wildcard and the remaining path components. Searching starts at the anchor
// so wildcard expansion never enumerates an unauthorized ancestor.
func globPatternAnchor(pattern string) (string, []string, error) {
	pattern, err := filepath.Abs(pattern)
	if err != nil {
		return "", nil, err
	}
	pattern = filepath.Clean(pattern)
	volume := filepath.VolumeName(pattern)
	root := volume
	if filepath.IsAbs(pattern) {
		root += string(filepath.Separator)
	}
	rest := strings.TrimPrefix(pattern, root)
	parts := make([]string, 0)
	if rest != "" {
		for _, part := range strings.Split(rest, string(filepath.Separator)) {
			if part != "" {
				parts = append(parts, part)
			}
		}
	}
	firstWildcard := len(parts)
	for i, part := range parts {
		if _, err := filepath.Match(part, ""); err != nil {
			return "", nil, err
		}
		if firstWildcard == len(parts) && strings.ContainsAny(part, "*?[") {
			firstWildcard = i
		}
	}
	if root == "" {
		root = string(filepath.Separator)
	}
	anchor := root
	for _, part := range parts[:firstWildcard] {
		anchor = filepath.Join(anchor, part)
	}
	if firstWildcard == len(parts) {
		// A literal path needs no parent enumeration. Resolve the candidate
		// directly so "." and an authorized absolute file do not require read
		// access to an otherwise out-of-scope parent directory.
		return filepath.Clean(pattern), nil, nil
	}
	return filepath.Clean(anchor), parts[firstWildcard:], nil
}

// scopedGlob expands path components one directory at a time. Every directory
// is resolved through the same application policy before it is enumerated, and
// denied matches are discarded before they can be returned or traversed.
// A complete ** component matches zero or more descendant path components.
func scopedGlob(ctx *Context, anchor string, parts []string) ([]string, error) {
	if len(parts) == 0 {
		if _, err := ctx.ResolveRead(anchor); err != nil {
			return nil, err
		}
		if _, err := os.Lstat(anchor); err == nil {
			return []string{anchor}, nil
		}
		return nil, nil
	}
	info, err := os.Stat(anchor)
	if err != nil || !info.IsDir() {
		return nil, nil
	}
	seen := make(map[string]bool)
	var matches []string
	var expand func(string, int, bool) error
	expand = func(current string, index int, initial bool) error {
		if _, err := ctx.ResolveRead(current); err != nil {
			return nil
		}
		if index == len(parts) {
			if initial {
				return nil
			}
			if _, err := os.Lstat(current); err == nil && !seen[current] {
				seen[current] = true
				matches = append(matches, current)
			}
			return nil
		}

		part := parts[index]
		if part == "**" {
			// First match zero levels of this recursive wildcard.
			if err := expand(current, index+1, initial); err != nil {
				return err
			}
			entries, err := os.ReadDir(current)
			if err != nil {
				return nil
			}
			for _, entry := range entries {
				child := filepath.Join(current, entry.Name())
				if _, err := ctx.ResolveRead(child); err != nil {
					continue
				}
				if index == len(parts)-1 {
					if _, err := os.Lstat(child); err == nil && !seen[child] {
						seen[child] = true
						matches = append(matches, child)
					}
				}
				// WalkDir's historical behavior did not follow symlinked
				// directories; keep that rule for the recursive wildcard too.
				if entry.IsDir() {
					if err := expand(child, index, false); err != nil {
						return err
					}
				}
			}
			return nil
		}

		entries, err := os.ReadDir(current)
		if err != nil {
			return nil
		}
		for _, entry := range entries {
			matched, err := filepath.Match(part, entry.Name())
			if err != nil {
				return err
			}
			if !matched {
				continue
			}
			child := filepath.Join(current, entry.Name())
			if _, err := ctx.ResolveRead(child); err != nil {
				continue
			}
			if index == len(parts)-1 {
				if _, err := os.Lstat(child); err == nil && !seen[child] {
					seen[child] = true
					matches = append(matches, child)
				}
				continue
			}
			childInfo, err := os.Stat(child)
			if err != nil || !childInfo.IsDir() {
				continue
			}
			if err := expand(child, index+1, false); err != nil {
				return err
			}
		}
		return nil
	}
	if err := expand(anchor, 0, true); err != nil {
		return nil, err
	}
	return matches, nil
}

// ---------- Grep ----------

// GrepTool searches file contents by regular expression.
type GrepTool struct{}

// NewGrepTool creates the grep tool.
func NewGrepTool() *GrepTool { return &GrepTool{} }

func (t *GrepTool) Name() string { return "Grep" }

func (t *GrepTool) Description() string {
	return `Search file contents with a regular expression (ripgrep syntax).
Files ignored by .gitignore and VCS/dependency/cache directories are skipped.
Output modes: "content" (default) prints matching lines with line numbers,
"files_with_matches" prints only paths, "count" prints matches per file.
Use -A/-B/-C for surrounding context, glob or type to restrict files, and
head_limit/offset to page through results (200 lines by default).`
}

func (t *GrepTool) Parameters() map[string]any {
	intArg := func(desc string) map[string]any {
		return map[string]any{"type": "integer", "description": desc}
	}
	boolArg := func(desc string) map[string]any {
		return map[string]any{"type": "boolean", "description": desc}
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{
				"type":        "string",
				"description": "The regular expression to search for.",
			},
			"path": map[string]any{
				"type":        "string",
				"description": "Directory to search (default: current working directory).",
			},
			"glob": map[string]any{
				"type":        "string",
				"description": `Glob restricting files, e.g. "*.go" or "**/*.ts". Prefix with "!" to exclude.`,
			},
			"type": map[string]any{
				"type":        "string",
				"description": "File type to restrict the search, e.g. go, ts, py, rust, json.",
			},
			"output_mode": map[string]any{
				"type":        "string",
				"enum":        []string{grepModeContent, grepModeFiles, grepModeCount},
				"description": "Output format (default: content).",
			},
			"-A":         intArg("Lines to show after each match (rg -A)."),
			"-B":         intArg("Lines to show before each match (rg -B)."),
			"-C":         intArg("Lines to show before and after each match (rg -C)."),
			"-i":         boolArg("Case-insensitive search."),
			"-n":         boolArg("Show line numbers (default: true)."),
			"-o":         boolArg("Print only the matched part, one match per line."),
			"multiline":  boolArg("Enable multiline mode where . matches newlines and patterns can span lines."),
			"head_limit": intArg("Limit output to the first N lines/entries (0 uses the default 200)."),
			"offset":     intArg("Skip the first N lines/entries before applying head_limit."),
		},
		"required": []string{"pattern"},
	}
}

func (t *GrepTool) Run(ctx *Context) (string, error) {
	if err := ctx.checkResources(); err != nil {
		return "", err
	}
	pattern := StringArg(ctx.Args, "pattern", "")
	if strings.TrimSpace(pattern) == "" {
		return "", fmt.Errorf("Grep: missing pattern")
	}
	base := ctx.WorkingDir
	if rawPath := StringArg(ctx.Args, "path", ""); rawPath != "" {
		base = rawPath
	}
	base, err := ctx.ResolveRead(base)
	if err != nil {
		return "", fmt.Errorf("Grep: %w", err)
	}
	info, err := os.Stat(base)
	if err != nil {
		return "", fmt.Errorf("Grep: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("Grep: path %q is not a directory", base)
	}

	mode := StringArg(ctx.Args, "output_mode", grepModeContent)
	switch mode {
	case grepModeContent, grepModeFiles, grepModeCount:
	default:
		return "", fmt.Errorf("Grep: unknown output_mode %q", mode)
	}
	headLimit, err := IntArgChecked(ctx.Args, "head_limit", 0)
	if err != nil {
		return "", fmt.Errorf("Grep: %w", err)
	}
	offset, err := IntArgChecked(ctx.Args, "offset", 0)
	if err != nil {
		return "", fmt.Errorf("Grep: %w", err)
	}
	contextLines := IntArg(ctx.Args, "-C", 0)
	before := IntArg(ctx.Args, "-B", 0)
	after := IntArg(ctx.Args, "-A", 0)
	if contextLines > 0 {
		if before == 0 {
			before = contextLines
		}
		if after == 0 {
			after = contextLines
		}
	}

	req := &grepRequest{
		pattern:    pattern,
		base:       base,
		outputMode: mode,
		ignoreCase: BoolArg(ctx.Args, "-i", false),
		multiline:  BoolArg(ctx.Args, "multiline", false),
		onlyMatch:  BoolArg(ctx.Args, "-o", false),
		before:     before,
		after:      after,
		maxLines:   headLimit,
		offset:     offset,
	}
	if glob := StringArg(ctx.Args, "glob", ""); glob != "" {
		req.globs = []string{glob}
	}
	if typ := StringArg(ctx.Args, "type", ""); typ != "" {
		for _, part := range strings.Split(typ, ",") {
			if part = strings.TrimSpace(part); part != "" {
				req.types = append(req.types, part)
			}
		}
	}

	out, err := searchGrep(ctx, req)
	if err != nil {
		return "", err
	}
	return boundedToolString(ctx, out), nil
}

// ---------- LS ----------

// LSTool lists a directory with sizes and timestamps.
type LSTool struct{}

// NewLSTool creates the ls tool.
func NewLSTool() *LSTool { return &LSTool{} }

func (t *LSTool) Name() string { return "LS" }

func (t *LSTool) Description() string {
	return `List the contents of a directory with file sizes and modification times.
Directories are listed first.`
}

func (t *LSTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Directory to list (default: current working directory).",
			},
		},
	}
}

func (t *LSTool) Run(ctx *Context) (string, error) {
	if err := ctx.checkResources(); err != nil {
		return "", err
	}
	dir := ctx.WorkingDir
	if rawPath := StringArg(ctx.Args, "path", ""); rawPath != "" {
		dir = rawPath
	}
	dir, err := ctx.ResolveRead(dir)
	if err != nil {
		return "", fmt.Errorf("LS: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("LS: %w", err)
	}
	visible := entries[:0]
	for _, entry := range entries {
		if _, err := ctx.ResolveRead(filepath.Join(dir, entry.Name())); err == nil {
			visible = append(visible, entry)
		}
	}
	entries = visible
	sort.Slice(entries, func(i, j int) bool {
		ei, ej := entries[i], entries[j]
		if ei.IsDir() != ej.IsDir() {
			return ei.IsDir()
		}
		return ei.Name() < ej.Name()
	})

	var sb strings.Builder
	fmt.Fprintf(&sb, "Contents of %s (%d entries):\n", dir, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		marker := " "
		if e.IsDir() {
			marker = "d"
		}
		fmt.Fprintf(&sb, "%s %10d  %s  %s\n", marker, info.Size(), info.ModTime().Format("2006-01-02 15:04"), e.Name())
	}
	return boundedToolString(ctx, sb.String()), nil
}
