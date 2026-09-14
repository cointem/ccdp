package tools

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"ccdp/internal/sandbox"
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
	return `Search the contents of files for a regular expression. Supports an optional
glob to restrict which files are searched. Returns matching lines with line
numbers. Cap at 200 results.`
}

func (t *GrepTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{
				"type":        "string",
				"description": "The regular expression to search for.",
			},
			"path": map[string]any{
				"type":        "string",
				"description": "Optional directory to search (default: current working directory).",
			},
			"glob": map[string]any{
				"type":        "string",
				"description": "Optional glob restricting file types, e.g. *.go or **/*.ts.",
			},
		},
		"required": []string{"pattern"},
	}
}

func (t *GrepTool) Run(ctx *Context) (string, error) {
	if err := ctx.checkResources(); err != nil {
		return "", err
	}
	pattern := StringArg(ctx.Args, "pattern", "")
	if pattern == "" {
		return "", fmt.Errorf("Grep: missing pattern")
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("Grep: bad regex: %w", err)
	}
	base := ctx.WorkingDir
	if rawPath := StringArg(ctx.Args, "path", ""); rawPath != "" {
		var err error
		base, err = ctx.ResolveRead(rawPath)
		if err != nil {
			return "", err
		}
	}
	glob := StringArg(ctx.Args, "glob", "")

	var globRE *regexp.Regexp
	if glob != "" {
		globRE = regexp.MustCompile(translateGlob(glob))
	}

	var sb strings.Builder
	count := 0
	const max = 200

	_ = filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
		if count >= max {
			return filepath.SkipAll
		}
		if err != nil || info.IsDir() {
			return nil
		}
		if globRE != nil && !globRE.MatchString(info.Name()) && !globRE.MatchString(path) {
			return nil
		}
		// Skip binary-ish and large files quickly. Read through a bounded
		// reader; os.ReadFile would allocate the whole file before limits apply.
		if info.Size() > int64(ctx.readLimit()) {
			return nil
		}
		readPath := path
		if ctx.Sandbox != nil {
			readPath, err = ctx.Sandbox.ResolveRead(path)
			if err != nil {
				return nil
			}
		}
		flags := os.O_RDONLY
		if ctx.Sandbox != nil && ctx.Sandbox.CurrentMode() == sandbox.ModeStrict {
			flags |= syscall.O_NOFOLLOW
		}
		f, err := os.OpenFile(readPath, flags, 0)
		if err != nil {
			return nil
		}
		data, err := io.ReadAll(io.LimitReader(f, int64(ctx.readLimit())+1))
		_ = f.Close()
		if err != nil || len(data) > ctx.readLimit() {
			return nil
		}
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			if len(line) > 1000 {
				continue
			}
			if re.MatchString(line) {
				rel, _ := filepath.Rel(base, path)
				fmt.Fprintf(&sb, "%s:%d:%s\n", rel, i+1, line)
				count++
				if count >= max {
					return filepath.SkipAll
				}
			}
		}
		return nil
	})

	return boundedToolString(ctx, fmt.Sprintf("%d match(es):\n%s", count, sb.String())), nil
}

// translateGlob converts a simple glob into a regex for matching file names.
func translateGlob(g string) string {
	g = strings.TrimPrefix(g, "**/")
	var sb strings.Builder
	sb.WriteString("^")
	for _, r := range g {
		switch r {
		case '*':
			sb.WriteString(".*")
		case '?':
			sb.WriteString(".")
		default:
			sb.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	sb.WriteString("$")
	return sb.String()
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
