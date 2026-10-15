package tools

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"ccdp/internal/executable"
	"ccdp/internal/fsops"
	"ccdp/internal/workspace"
)

// Rg authorizes search paths before handing content matching to ripgrep.

const (
	// rgDefaultMaxLines caps one search result set.
	rgDefaultMaxLines = 200
	// rgLineCap bounds characters retained from a single result line.
	rgLineCap = 2000
	// rgTimeout bounds one search invocation.
	rgTimeout = 60 * time.Second
)

// rgLine is one rendered result line. match reports whether the line
// satisfied the pattern; context lines carry match=false.
type rgLine struct {
	file  string
	line  int
	text  string
	match bool
	count int // preaggregated matching-line count from a streamed file
}

type rgRequest struct {
	singleFile string
	pattern    string
	literal    bool
	base       string
	globs      []string
	types      []string
	ignoreCase bool
	multiline  bool
	before     int
	after      int
	onlyMatch  bool
	outputMode string
	// maxLines bounds the lines reported to the model; fetchLines adds the
	// offset so pagination still sees real results.
	maxLines int
	offset   int
}

func (r *rgRequest) contextLines() int {
	if r.before > r.after {
		return r.before
	}
	return r.after
}

func (r *rgRequest) fetchLines() int {
	n := r.maxLines
	if n <= 0 {
		n = rgDefaultMaxLines
	}
	return n + r.offset
}

func canonicalRgRequest(ctx *Context, r *rgRequest) (*rgRequest, error) {
	if ctx == nil || ctx.Sandbox == nil {
		return nil, fmt.Errorf("Rg: sandbox policy is unavailable")
	}
	path, err := ctx.ResolveRead(r.base)
	if err != nil {
		return nil, err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	if _, err := ctx.ResolveRead(path); err != nil {
		return nil, err
	}
	copy := *r
	copy.base = path
	return &copy, nil
}

func searchRg(ctx *Context, r *rgRequest) (string, error) {
	lines, truncated, err := ripgrepSearch(ctx, r)
	if err != nil {
		return "", err
	}
	return formatRgResult(lines, r, truncated), nil
}

func clipLine(s string) string {
	if len(s) <= rgLineCap {
		return s
	}
	return truncateUTF8(s, rgLineCap) + "…"
}

// ---------- authorized file discovery ----------

const (
	rgModeContent = "content"
	rgModeFiles   = "files"
	rgModeCount   = "count"
)

// skipDirNames are pruned even when no .gitignore covers them: VCS, dependency
// and cache trees that only add noise to a code search.
var skipDirNames = map[string]bool{
	".git":             true,
	".hg":              true,
	".svn":             true,
	"node_modules":     true,
	"bower_components": true,
	"__pycache__":      true,
	".mypy_cache":      true,
	".pytest_cache":    true,
	".ruff_cache":      true,
	".tox":             true,
}

// walkRgFiles applies path, ignore and type policy before native execution.
func walkRgFiles(ctx *Context, r *rgRequest, visit func(string, string) error) error {
	filter, err := newFileFilter(r)
	if err != nil {
		return err
	}
	ignoreRoot := rgIgnoreRoot(ctx, r.base)
	ign := workspace.NewMatcherSafe()
	loadIgnore := func(dir, rel string) {
		if data, ok := readRgFileLimit(ctx, filepath.Join(dir, ".gitignore"), min(ctx.readLimit(), 256<<10)); ok {
			ign.AddGitignore(data, rel)
		}
	}
	ignored := func(rel string, isDir bool) bool { hit, _ := ign.Match(rel, isDir); return hit }
	// Match coordinates remain relative to the authorized enclosing root so
	// anchored parent rules keep their meaning when searching a subdirectory.
	var ancestors []string
	for dir := r.base; ; dir = filepath.Dir(dir) {
		ancestors = append(ancestors, dir)
		if dir == ignoreRoot {
			break
		}
	}
	for i := len(ancestors) - 1; i >= 0; i-- {
		rel, _ := filepath.Rel(ignoreRoot, ancestors[i])
		if rel == "." {
			rel = ""
		}
		loadIgnore(ancestors[i], filepath.ToSlash(rel))
	}

	walkRoot := r.base
	if r.singleFile != "" {
		walkRoot = filepath.Join(r.base, r.singleFile)
	}
	return filepath.WalkDir(walkRoot, func(path string, d fs.DirEntry, err error) error {
		if err := ctx.Context.Err(); err != nil {
			return err
		}
		if err != nil {
			return nil
		}
		if _, err := ctx.ResolveRead(path); err != nil {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(r.base, path)
		if rerr != nil || rel == "." {
			return nil
		}
		ignoreRel, _ := filepath.Rel(ignoreRoot, path)
		if d.IsDir() {
			if skipDirNames[d.Name()] || ignored(ignoreRel, true) {
				return fs.SkipDir
			}
			loadIgnore(path, filepath.ToSlash(ignoreRel))
			return nil
		}
		if r.singleFile == "" && d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if (r.singleFile == "" && ignored(ignoreRel, false)) || !filter.match(filepath.ToSlash(rel), d.Name()) {
			return nil
		}

		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		return visit(path, filepath.ToSlash(rel))
	})
}

// rgIgnoreRoot confines ignore discovery to the enclosing authorized
// workspace root. A separately requested path starts its own search scope.
func rgIgnoreRoot(ctx *Context, base string) string {
	policy := ctx.Sandbox.Snapshot()
	roots := []string{policy.Workspace}
	if ctx.ReadRoot != "" {
		roots = []string{ctx.ReadRoot}
	}
	if ctx.ReadRoot == "" {
		roots = append(roots, policy.AdditionalDirs...)
		roots = append(roots, policy.ReadOnlyDirs...)
	}
	root := base
	for _, candidate := range roots {
		if candidate == "" {
			continue
		}
		canonical, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			continue
		}
		if executable.Within(canonical, base) && (root == base || executable.Within(canonical, root)) {
			root = canonical
		}
	}
	// A denied or snapshot-external ancestor cannot contribute rules.
	for dir := base; dir != root; dir = filepath.Dir(dir) {
		parent := filepath.Dir(dir)
		if _, err := ctx.ResolveRead(parent); err != nil {
			return dir
		}
	}
	return root
}

// readRgFile admits bounded auxiliary input, such as .gitignore contents.
// Search files use streaming lines or explicitly report incomplete reads.
func readRgFile(ctx *Context, path string) ([]byte, bool) {
	return readRgFileLimit(ctx, path, ctx.readLimit())
}

var errRgReadLimit = errors.New("Rg file read limit exceeded")

func readRgFileLimit(ctx *Context, path string, limit int) ([]byte, bool) {
	data, err := readRgFileDetailed(ctx, path, limit)
	return data, err == nil
}

func openRgFile(ctx *Context, path string) (*os.File, error) {
	if err := ctx.checkResources(); err != nil {
		return nil, err
	}
	return fsops.OpenAuthorizedRegular(ctx.ReadRoot, path, ctx.ResolveRead)
}

func readRgFileDetailed(ctx *Context, path string, limit int) ([]byte, error) {
	f, err := openRgFile(ctx, path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, errRgReadLimit
	}
	return data, ctx.checkResources()
}

// ---------- file filter ----------

type fileFilter struct {
	globs    []*regexp.Regexp
	negGlobs []*regexp.Regexp
	exts     map[string]bool
	names    map[string]bool
}

func newFileFilter(r *rgRequest) (*fileFilter, error) {
	f := &fileFilter{}
	for _, g := range r.globs {
		neg := strings.HasPrefix(g, "!")
		pat := strings.TrimPrefix(g, "!")
		re, err := regexp.Compile(translateFileGlob(pat))
		if err != nil {
			return nil, fmt.Errorf("Rg: bad glob %q: %w", g, err)
		}
		if neg {
			f.negGlobs = append(f.negGlobs, re)
		} else {
			f.globs = append(f.globs, re)
		}
	}
	if len(r.types) > 0 {
		f.exts = map[string]bool{}
		f.names = map[string]bool{}
		for _, t := range r.types {
			def, ok := rgTypeExts[strings.ToLower(t)]
			if !ok {
				return nil, fmt.Errorf("Rg: unknown type %q (supported: %s)", t, strings.Join(rgTypeNames(), ", "))
			}
			for _, e := range def.exts {
				f.exts[e] = true
			}
			for _, n := range def.names {
				f.names[n] = true
			}
		}
	}
	return f, nil
}

func (f *fileFilter) match(rel, name string) bool {
	for _, re := range f.negGlobs {
		if re.MatchString(rel) || re.MatchString(name) {
			return false
		}
	}
	if f.exts != nil {
		ext := strings.ToLower(filepath.Ext(name))
		if !f.exts[ext] && !f.names[name] && !f.names[strings.ToLower(name)] {
			return false
		}
	}
	if len(f.globs) == 0 {
		return true
	}
	for _, re := range f.globs {
		if re.MatchString(rel) || re.MatchString(name) {
			return true
		}
	}
	return false
}

// translateFileGlob converts a ripgrep-style glob into a path regexp. A glob
// without a slash matches the basename at any depth, as in rg.
func translateFileGlob(g string) string {
	g = strings.TrimPrefix(g, "./")
	anchored := strings.Contains(strings.TrimSuffix(g, "/"), "/")
	var parts []string
	for _, alt := range expandBraces(g) {
		parts = append(parts, regexp.QuoteMeta(alt))
	}
	body := strings.Join(parts, "|")
	if len(parts) > 1 {
		body = "(?:" + body + ")"
	}
	// Ordered: `**` spans separators, `*` and `?` do not.
	body = strings.ReplaceAll(body, regexp.QuoteMeta("**"), "\x00")
	body = strings.ReplaceAll(body, regexp.QuoteMeta("*"), "[^/]*")
	body = strings.ReplaceAll(body, regexp.QuoteMeta("?"), "[^/]")
	body = strings.ReplaceAll(body, "\x00", ".*")
	if anchored {
		return "^" + body + "$"
	}
	return "(^|.*/)" + body + "$"
}

// expandBraces handles a single non-nested `{a,b}` group, which covers the glob
// syntax models emit in practice.
func expandBraces(g string) []string {
	open := strings.IndexByte(g, '{')
	if open < 0 {
		return []string{g}
	}
	rest := strings.IndexByte(g[open:], '}')
	if rest < 0 {
		return []string{g}
	}
	close := open + rest
	prefix, suffix := g[:open], g[close+1:]
	var out []string
	for _, alt := range strings.Split(g[open+1:close], ",") {
		out = append(out, prefix+alt+suffix)
	}
	return out
}

type rgTypeDef struct {
	exts  []string
	names []string
}

var rgTypeExts = map[string]rgTypeDef{
	"go":         {exts: []string{".go"}},
	"py":         {exts: []string{".py", ".pyi"}},
	"python":     {exts: []string{".py", ".pyi"}},
	"js":         {exts: []string{".js", ".jsx", ".mjs", ".cjs"}},
	"ts":         {exts: []string{".ts", ".tsx", ".mts", ".cts"}},
	"tsx":        {exts: []string{".tsx"}},
	"jsx":        {exts: []string{".jsx"}},
	"rs":         {exts: []string{".rs"}},
	"rust":       {exts: []string{".rs"}},
	"java":       {exts: []string{".java"}},
	"c":          {exts: []string{".c", ".h"}},
	"cpp":        {exts: []string{".cc", ".cpp", ".cxx", ".hpp", ".hh", ".h"}},
	"cs":         {exts: []string{".cs"}},
	"rb":         {exts: []string{".rb"}},
	"php":        {exts: []string{".php"}},
	"sh":         {exts: []string{".sh", ".bash", ".zsh"}},
	"bash":       {exts: []string{".sh", ".bash"}},
	"json":       {exts: []string{".json"}},
	"yaml":       {exts: []string{".yml", ".yaml"}},
	"toml":       {exts: []string{".toml"}},
	"md":         {exts: []string{".md", ".markdown"}},
	"html":       {exts: []string{".html", ".htm"}},
	"css":        {exts: []string{".css", ".scss", ".less"}},
	"sql":        {exts: []string{".sql"}},
	"proto":      {exts: []string{".proto"}},
	"dockerfile": {names: []string{"Dockerfile", "dockerfile"}},
	"make":       {names: []string{"Makefile", "makefile", "GNUmakefile"}},
}

func rgTypeNames() []string {
	out := make([]string, 0, len(rgTypeExts))
	for k := range rgTypeExts {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------- rendering ----------

func formatRgResult(lines []rgLine, r *rgRequest, truncated bool) string {
	var sb strings.Builder
	switch r.outputMode {
	case rgModeFiles:
		entries := rgFileEntries(lines)
		windowed := applyRgWindow(entries, r)
		fmt.Fprintf(&sb, "Found %d file(s):\n", len(windowed))
		for _, l := range windowed {
			fmt.Fprintf(&sb, "%s\n", l.file)
		}
		truncated = truncated || len(entries) > r.fetchLines()
		if truncated {
			writeTruncationNote(&sb, max(0, len(entries)-r.offset-len(windowed)))
		}
	case rgModeCount:
		entries := rgCountEntries(lines)
		windowed := applyRgWindow(entries, r)
		fmt.Fprintf(&sb, "Matches in %d file(s):\n", len(windowed))
		for _, l := range windowed {
			fmt.Fprintf(&sb, "%s:%d\n", l.file, l.line)
		}
		truncated = truncated || len(entries) > r.fetchLines()
		if truncated {
			writeTruncationNote(&sb, max(0, len(entries)-r.offset-len(windowed)))
		}
	default:
		windowed, omitted := rgContentWindow(lines, r)
		matches := 0
		for _, l := range windowed {
			if l.match {
				matches++
			}
		}
		fmt.Fprintf(&sb, "%d match(es):\n", matches)
		withContext := r.contextLines() > 0
		prevFile, prevLine := "", -1
		for _, l := range windowed {
			if withContext && prevLine >= 0 && (l.file != prevFile || l.line != prevLine+1) {
				sb.WriteString("--\n")
			}
			sep := ":"
			if !l.match {
				sep = "-"
			}
			fmt.Fprintf(&sb, "%s%s%d%s%s\n", l.file, sep, l.line, sep, l.text)
			prevFile, prevLine = l.file, l.line
		}
		truncated = truncated || omitted
		if truncated {
			writeTruncationNote(&sb, max(0, len(lines)-r.offset-len(windowed)))
		}
	}
	return sb.String()
}

func writeTruncationNote(sb *strings.Builder, remaining int) {
	if remaining < 0 {
		remaining = 0
	}
	fmt.Fprintf(sb, "…(%d more result(s) buffered; search incomplete — refine the query or use Read/Bash for oversized lines or multiline files)\n", remaining)
}

// rgFileEntries collapses matches to their files, preserving first-seen order.
func rgFileEntries(lines []rgLine) []rgLine {
	var out []rgLine
	seen := map[string]bool{}
	for _, l := range lines {
		if !l.match || seen[l.file] {
			continue
		}
		seen[l.file] = true
		out = append(out, rgLine{file: l.file, match: true})
	}
	return out
}

// rgCountEntries aggregates one entry per file whose line field is the number
// of matching lines in it.
func rgCountEntries(lines []rgLine) []rgLine {
	var out []rgLine
	index := map[string]int{}
	for _, l := range lines {
		if !l.match {
			continue
		}
		if i, ok := index[l.file]; ok {
			out[i].line += max(1, l.count)
			continue
		}
		index[l.file] = len(out)
		out = append(out, rgLine{file: l.file, line: max(1, l.count), match: true})
	}
	return out
}

func applyRgWindow(lines []rgLine, r *rgRequest) []rgLine {
	if r.offset > 0 {
		if r.offset >= len(lines) {
			return nil
		}
		lines = lines[r.offset:]
	}
	limit := r.maxLines
	if limit <= 0 {
		limit = rgDefaultMaxLines
	}
	if len(lines) > limit {
		return lines[:limit]
	}
	return lines
}

func rgContentWindow(lines []rgLine, r *rgRequest) ([]rgLine, bool) {
	limit := r.maxLines
	if limit <= 0 {
		limit = rgDefaultMaxLines
	}
	seen := 0
	first, last := -1, -1
	omitted := false
	for i, l := range lines {
		if !l.match {
			continue
		}
		seen++
		if seen <= r.offset {
			continue
		}
		if seen > r.offset+limit {
			omitted = true
			break
		}
		if first < 0 {
			first = i
		}
		last = i
	}
	if first < 0 {
		return nil, omitted
	}
	lo, hi := first, last+1
	for lo > 0 && !lines[lo-1].match && lines[lo-1].file == lines[first].file && lines[first].line-lines[lo-1].line <= r.before {
		lo--
	}
	for hi < len(lines) && !lines[hi].match && lines[hi].file == lines[last].file && lines[hi].line-lines[last].line <= r.after {
		hi++
	}
	return lines[lo:hi], omitted
}
