// Package workspace builds a model of the user's working directory: it
// detects the enclosing git repository, honors .gitignore files, and produces
// a compact "repo map" (a file inventory) that is injected into the system
// prompt so the model can plan tool calls without blind globbing.
//
// This mirrors Codex's workspace awareness and Claude Code's repo-map
// feature, adapted to a lightweight, dependency-free implementation.
package workspace

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"unicode/utf8"
)

// Instruction limits are deliberately independent of the model context
// window.  Instructions are discovered from files outside the session log,
// so an unexpectedly large AGENTS.md must not turn prompt construction into
// an unbounded read or allocation.
const (
	DefaultMaxInstructionFileBytes = 256 << 10
	DefaultMaxInstructionsBytes    = 512 << 10
)

// Info is a snapshot of a workspace.
type Info struct {
	Root      string // absolute workspace root
	IsGitRepo bool
	GitRoot   string  // absolute repo root (== Root when not a git repo)
	Branch    string  // current branch, empty when not a git repo
	FileCount int     // number of tracked/visible files
	TotalSize int64   // total bytes of visible files
	Ignored   int     // files skipped via .gitignore / rules
	Files     []Entry // visible files, sorted by relative path
}

// Entry is one visible file in the workspace.
type Entry struct {
	RelPath  string // path relative to Root, using forward slashes
	Size     int64
	IsDir    bool
	Language string
}

// LangByExt maps common extensions to display labels.
var LangByExt = map[string]string{
	".go": "go", ".rs": "rust", ".py": "python", ".js": "js", ".ts": "ts",
	".tsx": "tsx", ".jsx": "jsx", ".java": "java", ".c": "c", ".h": "c",
	".cpp": "cpp", ".hpp": "cpp", ".cc": "cpp", ".cs": "csharp", ".rb": "ruby",
	".php": "php", ".swift": "swift", ".kt": "kotlin", ".kts": "kotlin",
	".sh": "shell", ".bash": "shell", ".zsh": "shell", ".ps1": "powershell",
	".md": "markdown", ".json": "json", ".yaml": "yaml", ".yml": "yaml",
	".toml": "toml", ".xml": "xml", ".html": "html", ".css": "css",
	".scss": "scss", ".sql": "sql", ".dockerfile": "docker", ".tf": "terraform",
	".proto": "proto", ".lua": "lua", ".pl": "perl", ".r": "r",
	".zig": "zig", ".dart": "dart", ".ex": "elixir", ".exs": "elixir",
	".hs": "haskell", ".ml": "ocaml", ".sol": "solidity",
}

// langOf guesses a language label from a file name.
func langOf(name string) string {
	base := strings.ToLower(filepath.Base(name))
	if base == "dockerfile" {
		return "docker"
	}
	if strings.HasPrefix(base, "makefile") {
		return "make"
	}
	ext := filepath.Ext(name)
	if l, ok := LangByExt[ext]; ok {
		return l
	}
	return ""
}

// Scan walks the workspace and returns a model of it. maxFiles caps the
// inventory (cheap guard against enormous trees).
func Scan(root string, maxFiles int) (*Info, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if maxFiles <= 0 {
		maxFiles = 300
	}

	gitRoot, isRepo, branch := detectGit(abs)
	info := &Info{
		Root:      abs,
		IsGitRepo: isRepo,
		GitRoot:   gitRoot,
		Branch:    branch,
	}

	ignore, err := NewMatcher()
	if err != nil {
		return nil, err
	}
	if isRepo {
		if err := ignore.LoadGitignores(gitRoot); err != nil {
			ignore = NewMatcherSafe()
		}
	}

	// Default skips: version control, dependency dirs, hidden files.
	alwaysSkip := map[string]bool{
		".git": true, ".hg": true, ".svn": true, "node_modules": true,
		"__pycache__": true, ".venv": true, "venv": true, "vendor": true,
		"target": true, "dist": true, "build": true, ".next": true,
		".turbo": true, ".idea": true, ".vscode": true, "Pods": true,
		"derived_data": true, ".gradle": true, "go.sum": false,
	}

	_ = filepath.WalkDir(abs, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if path == abs {
			return nil
		}
		rel, rerr := filepath.Rel(abs, path)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)

		// Apply skip rules to every path component. Dependency/build folders
		// can occur below package roots (for example packages/app/node_modules),
		// and checking only the first component would descend into them.
		if hasAlwaysSkippedComponent(rel, alwaysSkip) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			info.Ignored++
			return nil
		}

		// Hidden entries (dot-prefixed) are skipped unless explicitly wanted.
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			info.Ignored++
			return nil
		}

		if ignored, ok := ignore.Match(rel, d.IsDir()); ok && ignored {
			if d.IsDir() {
				return filepath.SkipDir
			}
			info.Ignored++
			return nil
		}

		if len(info.Files) >= maxFiles {
			return filepath.SkipAll
		}

		info.FileCount++
		e := Entry{
			RelPath: rel,
			IsDir:   d.IsDir(),
		}
		if !d.IsDir() {
			if fi, ferr := d.Info(); ferr == nil {
				e.Size = fi.Size()
				info.TotalSize += fi.Size()
			}
			e.Language = langOf(d.Name())
		}
		info.Files = append(info.Files, e)
		return nil
	})

	sort.Slice(info.Files, func(i, j int) bool {
		a, b := info.Files[i], info.Files[j]
		if a.IsDir != b.IsDir {
			return a.IsDir
		}
		return a.RelPath < b.RelPath
	})

	// Recompute FileCount as the number of non-directory files.
	nonDir := 0
	for _, f := range info.Files {
		if !f.IsDir {
			nonDir++
		}
	}
	info.FileCount = nonDir
	return info, nil
}

func hasAlwaysSkippedComponent(rel string, alwaysSkip map[string]bool) bool {
	for _, component := range strings.Split(rel, "/") {
		if alwaysSkip[component] {
			return true
		}
	}
	return false
}

// detectGit finds the nearest .git directory and current branch.
func detectGit(dir string) (gitRoot string, isRepo bool, branch string) {
	cur := dir
	for {
		if _, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
			branch = currentBranch(cur)
			return cur, true, branch
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return dir, false, ""
		}
		cur = parent
	}
}

// currentBranch reads HEAD to determine the branch name.
func currentBranch(repo string) string {
	data, err := os.ReadFile(filepath.Join(repo, ".git", "HEAD"))
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(data))
	const prefix = "ref: refs/heads/"
	if rest, ok := strings.CutPrefix(line, prefix); ok {
		return rest
	}
	return line // detached HEAD → commit hash
}

// RepoMap renders a compact file inventory suitable for a system prompt.
// It groups files by top-level directory and annotates languages.
func (i *Info) RepoMap(maxEntries int) string {
	if i == nil {
		return ""
	}
	if maxEntries <= 0 {
		maxEntries = 200
	}

	var sb strings.Builder
	sb.WriteString("Workspace files")
	if i.IsGitRepo {
		fmt.Fprintf(&sb, " (git branch %s)", i.Branch)
	}
	sb.WriteString(":\n")

	shown := 0
	var lastDir string
	for _, f := range i.Files {
		if f.IsDir {
			continue
		}
		if shown >= maxEntries {
			fmt.Fprintf(&sb, "… and %d more files\n", i.FileCount-shown)
			break
		}
		shown++

		dir, base := filepath.Split(f.RelPath)
		dir = strings.TrimSuffix(dir, "/")
		if dir != lastDir {
			if lastDir != "" {
				sb.WriteString("\n")
			}
			if dir == "" {
				dir = "."
			}
			fmt.Fprintf(&sb, "  %s/\n", dir)
			lastDir = dir
		}
		size := ""
		if f.Size >= 1<<20 {
			size = fmt.Sprintf(" (%.1f MB)", float64(f.Size)/(1<<20))
		} else if f.Size >= 1<<10 {
			size = fmt.Sprintf(" (%d KB)", f.Size>>10)
		}
		lang := ""
		if f.Language != "" {
			lang = "  [" + f.Language + "]"
		}
		fmt.Fprintf(&sb, "    %s%s%s\n", base, lang, size)
	}
	if shown == 0 {
		sb.WriteString("  (empty)\n")
	}
	return sb.String()
}

// Rel returns the forward-slash path of an absolute path relative to Root.
func (i *Info) Rel(abs string) string {
	rel, err := filepath.Rel(i.Root, abs)
	if err != nil {
		return abs
	}
	return filepath.ToSlash(rel)
}

// cache guards a per-workspace Info so repeated prompts don't rescan.
var cache sync.Map // root → *Info

// CachedScan returns a cached Info for root, rescanning if force is true.
func CachedScan(root string, force bool) (*Info, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if !force {
		if v, ok := cache.Load(abs); ok {
			return v.(*Info), nil
		}
	}
	info, err := Scan(abs, 300)
	if err != nil {
		return nil, err
	}
	cache.Store(abs, info)
	return info, nil
}

// Invalidate removes the cached model for root (e.g. after file changes).
func Invalidate(root string) {
	if abs, err := filepath.Abs(root); err == nil {
		cache.Delete(abs)
	}
}

// InstructionFile is the project-level agent instructions file (AGENTS.md),
// mirroring Codex's AGENTS.md and Claude Code's CLAUDE.md.
const InstructionFile = "AGENTS.md"

// ProjectInstructionFile is the per-project overrides file.
const ProjectInstructionFile = ".ccdp" + string(filepath.Separator) + "AGENTS.md"

// UserInstructionFile is the user-level instructions file (~/.ccdp/AGENTS.md),
// mirroring Claude Code's ~/.claude/CLAUDE.md.
const UserInstructionFile = ".ccdp" + string(filepath.Separator) + "AGENTS.md"

// InstructionTemplate is what /init writes.
const InstructionTemplate = `# Project Instructions

Write guidance for ccdp (or any coding agent) working in this repository.

## Build & Test

- What command builds the project?
- What command runs the tests?

## Conventions

- Code style, naming, and architecture rules the agent must follow.
- Files or directories the agent should never modify.

## Commands

- Frequently used commands and their effects.
`

// LoadInstructions collects agent instruction files for a workspace and
// returns their concatenated contents, or "" when none exist. Lookup order,
// mirroring Claude Code's memory files: user-level ~/.ccdp/AGENTS.md, then the
// project chain (git-root AGENTS.md → workspace AGENTS.md → workspace
// .ccdp/AGENTS.md, project-specific wins).
func LoadInstructions(root string) string {
	instructions, _ := loadInstructionsBounded(root, DefaultMaxInstructionsBytes, false)
	return instructions
}

// LoadInstructionsBounded is LoadInstructions with an explicit total output
// bound.  Each source file is read with a separate bound before it is added to
// the aggregate, and an oversized file is represented by a deterministic
// marker rather than silently disappearing.  The returned string is always at
// most maxBytes bytes (when maxBytes > 0).
func LoadInstructionsBounded(root string, maxBytes int) string {
	instructions, _ := loadInstructionsBounded(root, maxBytes, false)
	return instructions
}

// LoadInstructionsChecked is the error-returning form used by callers that
// need to distinguish an absent optional instruction file from an invalid
// present input (for example, a FIFO or directory at AGENTS.md). Missing
// candidates discovered during the normal lookup remain optional and are
// skipped. Oversized regular files are rejected with the offending path and
// limit instead of being silently admitted into the required prompt.
func LoadInstructionsChecked(root string) (string, error) {
	return LoadInstructionsBoundedChecked(root, DefaultMaxInstructionsBytes)
}

// LoadInstructionsBoundedChecked is LoadInstructionsBounded with errors from
// present instruction candidates surfaced to the caller. The legacy helpers
// intentionally retain their best-effort string-only API; this checked path
// rejects both per-file and aggregate output overflow.
func LoadInstructionsBoundedChecked(root string, maxBytes int) (string, error) {
	return loadInstructionsBounded(root, maxBytes, true)
}

func loadInstructionsBounded(root string, maxBytes int, strict bool) (string, error) {
	if maxBytes <= 0 {
		return "", fmt.Errorf("workspace: invalid instruction output limit")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("workspace: resolve root: %w", err)
	}
	if strict {
		rootInfo, err := os.Stat(abs)
		if err != nil {
			return "", fmt.Errorf("workspace: stat root %q: %w", abs, err)
		}
		if !rootInfo.IsDir() {
			return "", fmt.Errorf("workspace: root %q is not a directory", abs)
		}
	}
	gitRoot, isRepo, _ := detectGit(abs)

	var files []string
	if home, herr := os.UserHomeDir(); herr == nil {
		files = append(files, filepath.Join(home, UserInstructionFile))
	}
	if isRepo && gitRoot != abs {
		files = append(files, filepath.Join(gitRoot, InstructionFile))
	}
	files = append(files, filepath.Join(abs, InstructionFile))
	files = append(files, filepath.Join(abs, ProjectInstructionFile))

	var sb strings.Builder
	used := 0
	seen := map[string]bool{}
	for _, f := range files {
		if !strict && used >= maxBytes {
			break
		}
		if seen[f] {
			continue
		}
		seen[f] = true
		data, truncated, err := readBounded(f, DefaultMaxInstructionFileBytes)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if !strict {
				continue
			}
			// Preserve candidates already read for a checked caller while
			// surfacing the offending path. Legacy callers continue past this
			// candidate through the best-effort branch above.
			return strings.TrimSpace(sb.String()), fmt.Errorf("workspace: read instructions %q: %w", f, err)
		}
		if truncated && strict {
			return strings.TrimSpace(sb.String()), fmt.Errorf("workspace: instructions %q exceeds %d bytes", f, DefaultMaxInstructionFileBytes)
		}
		if len(strings.TrimSpace(string(data))) == 0 {
			continue
		}
		content := strings.TrimSpace(string(data))
		if truncated {
			content += fmt.Sprintf("\n\n…[instructions truncated at %d bytes]", DefaultMaxInstructionFileBytes)
		}
		piece := fmt.Sprintf("### Instructions from %s\n%s\n\n", f, content)
		if len(piece) > maxBytes-used {
			if strict {
				return strings.TrimSpace(sb.String()), fmt.Errorf("workspace: instructions %q exceed aggregate limit %d bytes", f, maxBytes)
			}
			piece = truncateUTF8(piece, maxBytes-used)
		}
		if piece == "" {
			break
		}
		sb.WriteString(piece)
		used += len(piece)
		if !strict && used >= maxBytes {
			break
		}
	}
	return strings.TrimSpace(sb.String()), nil
}

// readBounded reads at most maxBytes+1 bytes so the caller can distinguish an
// exact-size file from one that exceeded the limit.  It intentionally avoids
// os.ReadFile, whose allocation is based on an attacker-controlled file size.
func readBounded(path string, maxBytes int) ([]byte, bool, error) {
	if maxBytes <= 0 {
		return nil, false, fmt.Errorf("workspace: invalid read limit")
	}
	// Stat before opening: opening a FIFO for reading can block until another
	// process writes it.  Instructions are regular-file inputs; reject special
	// files before the open and re-check after opening to close the replacement
	// race between the two operations.
	info, err := os.Stat(path)
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("workspace: %s is not a regular file", path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	openedInfo, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	if !openedInfo.Mode().IsRegular() {
		return nil, false, fmt.Errorf("workspace: %s is not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)+1))
	if err != nil {
		return nil, false, err
	}
	if len(data) > maxBytes {
		return data[:maxBytes], true, nil
	}
	return data, false, nil
}

func truncateUTF8(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

// InitInstructionsFile writes the AGENTS.md template into the workspace root
// and returns its path. It refuses to overwrite an existing non-empty file.
func InitInstructionsFile(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	path := filepath.Join(abs, InstructionFile)
	if data, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(data))) > 0 {
		return path, fmt.Errorf("%s already exists", path)
	}
	if err := os.WriteFile(path, []byte(InstructionTemplate), 0o644); err != nil {
		return "", err
	}
	return path, nil
}
