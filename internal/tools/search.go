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

	fullPattern := pattern
	if !filepath.IsAbs(fullPattern) {
		fullPattern = filepath.Join(base, pattern)
	}

	matches, err := filepath.Glob(fullPattern)
	if err != nil {
		return "", fmt.Errorf("Glob: bad pattern: %w", err)
	}

	// Glob does not recurse into ** on its own; do a best-effort walk for
	// patterns containing "**".
	if strings.Contains(pattern, "**") {
		matches = globWalk(base, pattern)
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

// globWalk implements a simple ** glob walk over the base directory.
func globWalk(base, pattern string) []string {
	var out []string
	// Strip a leading ** or **/ prefix, then walk.
	rest := strings.TrimPrefix(pattern, "**")
	rest = strings.TrimPrefix(rest, "/")
	_ = filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, rerr := filepath.Rel(base, path)
		if rerr != nil {
			return nil
		}
		ok, _ := filepath.Match(rest, rel)
		if ok && rel != "." {
			out = append(out, path)
		}
		return nil
	})
	return out
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
		resolved, err := ctx.ResolveRead(rawPath)
		if err != nil {
			return "", err
		}
		base = resolved
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
		var err error
		dir, err = ctx.ResolveRead(rawPath)
		if err != nil {
			return "", err
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("LS: %w", err)
	}
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
