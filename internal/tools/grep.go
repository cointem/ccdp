package tools

import (
	"bufio"
	"context"
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

// Grep uses one Go/RE2 matcher on every platform so installation state never
// changes the accepted regex syntax or search results.

const (
	// grepDefaultMaxLines caps one search result set.
	grepDefaultMaxLines = 200
	// grepLineCap bounds characters retained from a single result line.
	grepLineCap = 2000
	// grepTimeout bounds one search invocation.
	grepTimeout = 60 * time.Second
)

// grepLine is one rendered result line. match reports whether the line
// satisfied the pattern; context lines carry match=false.
type grepLine struct {
	file  string
	line  int
	text  string
	match bool
	count int // preaggregated matching-line count from a streamed file
}

type grepRequest struct {
	singleFile string
	pattern    string
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

func (r *grepRequest) contextLines() int {
	if r.before > r.after {
		return r.before
	}
	return r.after
}

func (r *grepRequest) fetchLines() int {
	n := r.maxLines
	if n <= 0 {
		n = grepDefaultMaxLines
	}
	return n + r.offset
}

func canonicalGrepRequest(ctx *Context, r *grepRequest) (*grepRequest, error) {
	if ctx == nil || ctx.Sandbox == nil {
		return nil, fmt.Errorf("Grep: sandbox policy is unavailable")
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

func searchGrep(ctx *Context, r *grepRequest) (string, error) {
	lines, truncated, err := inProcessSearch(ctx, r)
	if err != nil {
		return "", err
	}
	return formatGrepResult(lines, r, truncated), nil
}

func clipLine(s string) string {
	if len(s) <= grepLineCap {
		return s
	}
	return truncateUTF8(s, grepLineCap) + "…"
}

// ---------- in-process engine ----------

const (
	grepModeContent = "content"
	grepModeFiles   = "files_with_matches"
	grepModeCount   = "count"
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

func inProcessSearch(ctx *Context, r *grepRequest) ([]grepLine, bool, error) {
	canonical, err := canonicalGrepRequest(ctx, r)
	if err != nil {
		return nil, false, err
	}
	r = canonical
	re, err := compileGrepPattern(r)
	if err != nil {
		return nil, false, err
	}
	filter, err := newFileFilter(r)
	if err != nil {
		return nil, false, err
	}
	call := ctx.Context
	if call == nil {
		call = context.Background()
	}
	timeout := grepTimeout
	if ctx.Timeout > 0 {
		timeout = ctx.Timeout
	}
	call, cancel := context.WithTimeout(call, timeout)
	defer cancel()
	local := *ctx
	local.Context = call
	ctx = &local
	ignoreRoot := grepIgnoreRoot(ctx, r.base)
	ign := workspace.NewMatcherSafe()
	loadIgnore := func(dir, rel string) {
		if data, ok := readGrepFileLimit(ctx, filepath.Join(dir, ".gitignore"), min(ctx.readLimit(), 256<<10)); ok {
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

	var (
		out       []grepLine
		truncated bool
	)
	limit := r.fetchLines()
	if r.outputMode == grepModeContent {
		limit *= 1 + r.before + r.after
	}
	entries := 0
	walkRoot := r.base
	if r.singleFile != "" {
		walkRoot = filepath.Join(r.base, r.singleFile)
	}
	walkErr := filepath.WalkDir(walkRoot, func(path string, d fs.DirEntry, err error) error {
		if err := call.Err(); err != nil {
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
		fileLines, incomplete, ferr := grepFile(ctx, path, filepath.ToSlash(rel), re, r)
		truncated = truncated || incomplete
		if ferr != nil {
			if errors.Is(ferr, errGrepReadLimit) {
				truncated = true
			}
			return nil
		}
		if r.outputMode == grepModeCount || r.outputMode == grepModeFiles {
			if len(fileLines) > 0 {
				entries++
				if r.outputMode == grepModeFiles {
					out = append(out, grepLine{file: filepath.ToSlash(rel), match: true})
				} else {
					out = append(out, fileLines...)
				}
				if entries > limit {
					truncated = true
					return fs.SkipAll
				}
			}
		} else {
			for _, l := range fileLines {
				out = append(out, l)
				if len(out) > limit {
					truncated = true
					return fs.SkipAll
				}
			}
		}
		return nil
	})
	if err := call.Err(); err != nil {
		return nil, false, err
	}
	if walkErr != nil && walkErr != fs.SkipAll {
		return nil, false, walkErr
	}
	return out, truncated, nil
}

// grepIgnoreRoot confines ignore discovery to the enclosing authorized
// workspace root. A separately requested path starts its own search scope.
func grepIgnoreRoot(ctx *Context, base string) string {
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

func compileGrepPattern(r *grepRequest) (*regexp.Regexp, error) {
	pat := r.pattern
	flags := ""
	if r.ignoreCase {
		flags += "i"
	}
	if r.multiline {
		// Go's (?s) is ripgrep's --multiline-dotall: `.` spans newlines.
		flags += "s"
	}
	if flags != "" {
		pat = "(?" + flags + ")" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return nil, fmt.Errorf("Grep: bad regex: %w", err)
	}
	return re, nil
}

// grepFile matches one file and expands before/after context into merged
// windows so overlapping matches are not emitted twice.
func grepFile(ctx *Context, path, rel string, re *regexp.Regexp, r *grepRequest) ([]grepLine, bool, error) {
	if !r.multiline {
		return grepFileLines(ctx, path, rel, re, r)
	}
	data, err := readGrepFileDetailed(ctx, path, ctx.readLimit())
	if err != nil {
		return nil, errors.Is(err, errGrepReadLimit), err
	}
	text := string(data)
	if strings.IndexByte(text, 0) >= 0 {
		return nil, false, nil // binary
	}
	lines := splitGrepLines(text)
	var matched []int
	if r.multiline {
		starts := lineStartOffsets(strings.Split(text, "\n"))[:len(lines)]
		for _, rng := range re.FindAllStringIndex(text, -1) {
			matched = append(matched, coveredLines(starts, rng[0], rng[1])...)
		}
		matched = dedupeSortedInts(matched)
	} else {
		for i, l := range lines {
			if re.MatchString(l) {
				matched = append(matched, i)
			}
		}
	}
	if len(matched) == 0 {
		return nil, false, nil
	}
	isMatch := make(map[int]bool, len(matched))
	for _, i := range matched {
		isMatch[i] = true
	}

	var out []grepLine
	start, end := -1, -1
	flush := func() {
		for i := max(start, 0); start >= 0 && i <= end && i < len(lines); i++ {
			text := lines[i]
			if isMatch[i] && r.onlyMatch {
				if m := re.FindString(text); m != "" {
					text = m
				}
			}
			out = append(out, grepLine{file: rel, line: i + 1, text: clipLine(text), match: isMatch[i]})
		}
		start, end = -1, -1
	}
	for _, i := range matched {
		lo, hi := i-r.before, i+r.after
		if lo < 0 {
			lo = 0
		}
		if hi > len(lines)-1 {
			hi = len(lines) - 1
		}
		if start >= 0 && lo <= end+1 {
			if hi > end {
				end = hi
			}
			continue
		}
		flush()
		start, end = lo, hi
	}
	flush()
	return out, false, nil
}

// readGrepFile admits bounded auxiliary input, such as .gitignore contents.
// Search files use streaming lines or explicitly report incomplete reads.
func readGrepFile(ctx *Context, path string) ([]byte, bool) {
	return readGrepFileLimit(ctx, path, ctx.readLimit())
}

var errGrepReadLimit = errors.New("Grep file read limit exceeded")

func readGrepFileLimit(ctx *Context, path string, limit int) ([]byte, bool) {
	data, err := readGrepFileDetailed(ctx, path, limit)
	return data, err == nil
}

func openGrepFile(ctx *Context, path string) (*os.File, error) {
	if err := ctx.checkResources(); err != nil {
		return nil, err
	}
	return fsops.OpenAuthorizedRegular(ctx.ReadRoot, path, ctx.ResolveRead)
}

func readGrepFileDetailed(ctx *Context, path string, limit int) ([]byte, error) {
	f, err := openGrepFile(ctx, path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(grepContextReader{ctx: ctx, reader: f}, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, errGrepReadLimit
	}
	return data, ctx.checkResources()
}

// Ordinary searches stream lines. Memory is bounded by one line, context and
// the requested result window; count mode finishes the file before reporting.
func grepFileLines(ctx *Context, path, rel string, re *regexp.Regexp, r *grepRequest) ([]grepLine, bool, error) {
	f, err := openGrepFile(ctx, path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	reader := bufio.NewReaderSize(grepContextReader{ctx: ctx, reader: f}, min(64<<10, ctx.readLimit()))
	var out, previous []grepLine
	var line []byte
	incomplete, oversized := false, false
	lineNumber, after, matches := 0, 0, 0
	lastAdded := 0
	resultLimit := r.fetchLines()*(1+r.before+r.after) + 1
	appendLine := func(row grepLine) {
		if row.line > lastAdded {
			out = append(out, row)
			lastAdded = row.line
		}
	}
	for {
		part, readErr := reader.ReadSlice('\n')
		if readErr != nil && readErr != io.EOF && readErr != bufio.ErrBufferFull {
			return nil, incomplete, readErr
		}
		if len(part) == 0 && readErr == io.EOF {
			break
		}
		if !oversized {
			if len(line)+len(part) > ctx.readLimit() {
				oversized, incomplete = true, true
				line = nil
			} else {
				line = append(line, part...)
			}
		}
		if readErr == bufio.ErrBufferFull {
			continue
		}
		lineNumber++
		if oversized {
			previous = nil
			after = 0
		} else {
			text := strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")
			if strings.IndexByte(text, 0) >= 0 {
				return nil, false, nil
			}
			hit := re.MatchString(text)
			if hit {
				matches++
			}
			if r.outputMode == grepModeFiles && hit {
				return []grepLine{{file: rel, match: true}}, incomplete, nil
			}
			if r.outputMode != grepModeCount {
				row := grepLine{file: rel, line: lineNumber, text: clipLine(text), match: hit}
				if hit {
					if r.onlyMatch {
						row.text = clipLine(re.FindString(text))
					}
					for _, before := range previous {
						appendLine(before)
					}
					appendLine(row)
					after = r.after
				} else if after > 0 {
					appendLine(row)
					after--
				}
				if r.before > 0 {
					previous = append(previous, row)
					if len(previous) > r.before {
						previous = previous[1:]
					}
				}
				if len(out) >= resultLimit {
					return out, true, nil
				}
			}
		}
		line, oversized = line[:0], false
		if readErr == io.EOF {
			break
		}
	}
	if r.outputMode == grepModeCount {
		if incomplete {
			return nil, true, nil
		} // a partial file count would be misleading
		if matches > 0 {
			return []grepLine{{file: rel, line: matches, match: true, count: matches}}, false, nil
		}
	}
	return out, incomplete, nil
}

type grepContextReader struct {
	ctx    *Context
	reader io.Reader
}

func (r grepContextReader) Read(p []byte) (int, error) {
	if r.ctx.Context != nil {
		if err := r.ctx.Context.Err(); err != nil {
			return 0, err
		}
	}
	if len(p) > 32<<10 {
		p = p[:32<<10]
	}
	return r.reader.Read(p)
}

func splitGrepLines(text string) []string {
	lines := strings.Split(text, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return lines
}

// lineStartOffsets returns the byte offset where each line begins.
func lineStartOffsets(lines []string) []int {
	starts := make([]int, len(lines))
	off := 0
	for i, l := range lines {
		starts[i] = off
		off += len(l) + 1
	}
	return starts
}

// coveredLines maps a multiline match's byte range onto the line indexes it
// touches.
func coveredLines(starts []int, from, to int) []int {
	lo := sort.SearchInts(starts, from)
	if lo == len(starts) || starts[lo] > from {
		lo--
	}
	if lo < 0 {
		lo = 0
	}
	var out []int
	for i := lo; i < len(starts); i++ {
		if starts[i] >= to {
			break
		}
		out = append(out, i)
	}
	return out
}

func dedupeSortedInts(in []int) []int {
	if len(in) < 2 {
		return in
	}
	sort.Ints(in)
	out := in[:1]
	for _, v := range in[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}

// ---------- file filter ----------

type fileFilter struct {
	globs    []*regexp.Regexp
	negGlobs []*regexp.Regexp
	exts     map[string]bool
	names    map[string]bool
}

func newFileFilter(r *grepRequest) (*fileFilter, error) {
	f := &fileFilter{}
	for _, g := range r.globs {
		neg := strings.HasPrefix(g, "!")
		pat := strings.TrimPrefix(g, "!")
		re, err := regexp.Compile(translateFileGlob(pat))
		if err != nil {
			return nil, fmt.Errorf("Grep: bad glob %q: %w", g, err)
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
			def, ok := grepTypeExts[strings.ToLower(t)]
			if !ok {
				return nil, fmt.Errorf("Grep: unknown type %q (supported: %s)", t, strings.Join(grepTypeNames(), ", "))
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

type grepTypeDef struct {
	exts  []string
	names []string
}

var grepTypeExts = map[string]grepTypeDef{
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

func grepTypeNames() []string {
	out := make([]string, 0, len(grepTypeExts))
	for k := range grepTypeExts {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------- rendering ----------

func formatGrepResult(lines []grepLine, r *grepRequest, truncated bool) string {
	var sb strings.Builder
	switch r.outputMode {
	case grepModeFiles:
		entries := grepFileEntries(lines)
		windowed := applyGrepWindow(entries, r)
		fmt.Fprintf(&sb, "Found %d file(s):\n", len(windowed))
		for _, l := range windowed {
			fmt.Fprintf(&sb, "%s\n", l.file)
		}
		truncated = truncated || len(entries) > r.fetchLines()
		if truncated {
			writeTruncationNote(&sb, max(0, len(entries)-r.offset-len(windowed)))
		}
	case grepModeCount:
		entries := grepCountEntries(lines)
		windowed := applyGrepWindow(entries, r)
		fmt.Fprintf(&sb, "Matches in %d file(s):\n", len(windowed))
		for _, l := range windowed {
			fmt.Fprintf(&sb, "%s:%d\n", l.file, l.line)
		}
		truncated = truncated || len(entries) > r.fetchLines()
		if truncated {
			writeTruncationNote(&sb, max(0, len(entries)-r.offset-len(windowed)))
		}
	default:
		windowed, omitted := grepContentWindow(lines, r)
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

// grepFileEntries collapses matches to their files, preserving first-seen order.
func grepFileEntries(lines []grepLine) []grepLine {
	var out []grepLine
	seen := map[string]bool{}
	for _, l := range lines {
		if !l.match || seen[l.file] {
			continue
		}
		seen[l.file] = true
		out = append(out, grepLine{file: l.file, match: true})
	}
	return out
}

// grepCountEntries aggregates one entry per file whose line field is the number
// of matching lines in it.
func grepCountEntries(lines []grepLine) []grepLine {
	var out []grepLine
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
		out = append(out, grepLine{file: l.file, line: max(1, l.count), match: true})
	}
	return out
}

func applyGrepWindow(lines []grepLine, r *grepRequest) []grepLine {
	if r.offset > 0 {
		if r.offset >= len(lines) {
			return nil
		}
		lines = lines[r.offset:]
	}
	limit := r.maxLines
	if limit <= 0 {
		limit = grepDefaultMaxLines
	}
	if len(lines) > limit {
		return lines[:limit]
	}
	return lines
}

func grepContentWindow(lines []grepLine, r *grepRequest) ([]grepLine, bool) {
	limit := r.maxLines
	if limit <= 0 {
		limit = grepDefaultMaxLines
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
