package tools

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"ccdp/internal/execution"
	"ccdp/internal/sandbox"
)

// Grep runs through ripgrep when it is available and falls back to an
// in-process engine otherwise. Both backends answer the same query and produce
// the same grepLine values, so a missing binary degrades speed, not semantics.

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
}

type grepRequest struct {
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

func searchGrep(ctx *Context, r *grepRequest) (string, error) {
	if ripgrepUsableFor(ctx) {
		lines, truncated, err := ripgrepSearch(ctx, r)
		if err == nil {
			return formatGrepResult(lines, r, truncated), nil
		}
		if !isRipgrepUnavailable(err) {
			return "", err
		}
		// The binary could not run at all (gone since probe, sandbox denial).
		// The in-process engine answers the same query.
	}
	lines, truncated, err := inProcessSearch(ctx, r)
	if err != nil {
		return "", err
	}
	return formatGrepResult(lines, r, truncated), nil
}

// ripgrepUsableFor reports whether the ripgrep backend can serve this request.
// Strict sandboxes confine reads to the workspace roots, which normally excludes
// the ripgrep binary itself, so strict mode always uses the in-process engine.
func ripgrepUsableFor(ctx *Context) bool {
	if ripgrepBinary() == "" {
		return false
	}
	return ctx.Sandbox == nil || ctx.Sandbox.CurrentMode() != sandbox.ModeStrict
}

var (
	rgOnce     sync.Once
	rgResolved string
)

// ripgrepBinary resolves the ripgrep executable once per process. An explicit
// CCDP_RG_PATH wins over PATH lookup so a bundled or relocated binary can be
// pinned without touching PATH.
func ripgrepBinary() string {
	rgOnce.Do(func() {
		if p := strings.TrimSpace(os.Getenv("CCDP_RG_PATH")); p != "" {
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				rgResolved = p
				return
			}
		}
		if p, err := exec.LookPath("rg"); err == nil {
			rgResolved = p
		}
	})
	return rgResolved
}

var errRipgrepUnavailable = fmt.Errorf("ripgrep unavailable")

func isRipgrepUnavailable(err error) bool {
	return err != nil && strings.Contains(err.Error(), errRipgrepUnavailable.Error())
}

// ripgrepSearch builds the rg invocation and normalizes its output.
func ripgrepSearch(ctx *Context, r *grepRequest) ([]grepLine, bool, error) {
	args := []string{"--no-heading", "--no-messages", "--color", "never", "--hidden", "--no-require-git", "--sort", "path"}
	// The pruned-directory table is shared with the in-process engine, so a
	// repository whose .gitignore omits node_modules still behaves identically
	// whichever backend answers.
	for _, dir := range skipDirNamesSorted() {
		args = append(args, "--glob", "!"+dir)
	}
	if r.ignoreCase {
		args = append(args, "-i")
	}
	if r.multiline {
		args = append(args, "-U", "--multiline-dotall")
	}
	if r.onlyMatch {
		args = append(args, "-o")
	}
	for _, g := range r.globs {
		args = append(args, "--glob", g)
	}
	for _, t := range r.types {
		args = append(args, "--type", t)
	}
	jsonOut := false
	switch r.outputMode {
	case grepModeFiles:
		args = append(args, "-l")
	default:
		// content and count both consume per-line events: count is aggregated
		// during rendering so the two backends cannot disagree on the numbers.
		jsonOut = true
		switch {
		case r.before > 0 && r.after > 0:
			args = append(args, "-B", strconv.Itoa(r.before), "-A", strconv.Itoa(r.after))
		case r.before > 0:
			args = append(args, "-B", strconv.Itoa(r.before))
		case r.after > 0:
			args = append(args, "-A", strconv.Itoa(r.after))
		}
		args = append(args, "--json")
	}
	args = append(args, "--", r.pattern, ".")

	timeout := ctx.Timeout
	if timeout <= 0 || timeout > grepTimeout {
		timeout = grepTimeout
	}
	// argv crosses the execution boundary unquoted: pattern and glob values
	// never reach a shell parser.
	res, err := execution.RunArgv(ctx.Context, append([]string{ripgrepBinary()}, args...), execution.Request{
		Context:     ctx.Context,
		Dir:         r.base,
		Timeout:     timeout,
		Sandbox:     ctx.Sandbox,
		OutputLimit: ctx.outputLimit(),
	})
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", errRipgrepUnavailable, err)
	}
	switch res.ExitCode {
	case 0, 1:
		// 1 means "no matches", a valid empty result.
	case 127:
		return nil, false, fmt.Errorf("%w: exit 127", errRipgrepUnavailable)
	default:
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = strings.TrimSpace(res.Output)
		}
		if strings.Contains(msg, "Operation not permitted") || strings.Contains(msg, "sandbox-exec") {
			return nil, false, fmt.Errorf("%w: %s", errRipgrepUnavailable, msg)
		}
		if msg == "" {
			msg = "unknown error"
		}
		return nil, false, fmt.Errorf("Grep: %s", firstLine(msg))
	}

	// A truncated JSON stream can end halfway through a file. Never report
	// that prefix as its complete count; the fallback counts complete files.
	if r.outputMode == grepModeCount && res.Truncated {
		return inProcessSearch(ctx, r)
	}
	if jsonOut {
		return parseRipgrepJSON(res.Stdout, r), res.Truncated, nil
	}
	return parseRipgrepList(res.Stdout), res.Truncated, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

type rgJSONEvent struct {
	Type string `json:"type"`
	Data struct {
		Path struct {
			Text string `json:"text"`
		} `json:"path"`
		Lines struct {
			Text string `json:"text"`
		} `json:"lines"`
		LineNumber int `json:"line_number"`
		Submatches []struct {
			Match struct {
				Text string `json:"text"`
			} `json:"match"`
		} `json:"submatches"`
	} `json:"data"`
}

func parseRipgrepJSON(out string, r *grepRequest) []grepLine {
	var lines []grepLine
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	limit := r.fetchLines()
	files := map[string]bool{}
	for sc.Scan() {
		var ev rgJSONEvent
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if ev.Type != "match" && ev.Type != "context" {
			continue
		}
		file := normalizeSearchPath(ev.Data.Path.Text)
		if r.outputMode == grepModeCount {
			if !files[file] && len(files) > limit {
				return lines
			}
			files[file] = true
		}
		// A multiline match arrives as one event whose text spans newlines. The
		// in-process engine reports each covered line, so expand here to keep the
		// two backends identical.
		for i, raw := range strings.Split(strings.TrimSuffix(ev.Data.Lines.Text, "\n"), "\n") {
			text := strings.TrimSuffix(raw, "\r")
			match := ev.Type == "match"
			if match && r.onlyMatch && i == 0 && len(ev.Data.Submatches) > 0 {
				text = ev.Data.Submatches[0].Match.Text
			}
			lines = append(lines, grepLine{
				file:  file,
				line:  ev.Data.LineNumber + i,
				text:  clipLine(text),
				match: match,
			})
			if r.outputMode != grepModeCount && len(lines) > limit {
				return lines
			}
		}
	}
	return lines
}

func parseRipgrepList(out string) []grepLine {
	var lines []grepLine
	for _, raw := range strings.Split(out, "\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		lines = append(lines, grepLine{file: normalizeSearchPath(raw), match: true})
	}
	return lines
}

// normalizeSearchPath strips the "./" prefix ripgrep adds for a "." search root
// so both backends report workspace-relative paths.
func normalizeSearchPath(p string) string {
	return strings.TrimPrefix(filepath.ToSlash(p), "./")
}

func clipLine(s string) string {
	if len(s) <= grepLineCap {
		return s
	}
	return s[:grepLineCap] + "…"
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

func skipDirNamesSorted() []string {
	out := make([]string, 0, len(skipDirNames))
	for name := range skipDirNames {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func inProcessSearch(ctx *Context, r *grepRequest) ([]grepLine, bool, error) {
	re, err := compileGrepPattern(r)
	if err != nil {
		return nil, false, err
	}
	filter, err := newFileFilter(r)
	if err != nil {
		return nil, false, err
	}
	ign := newIgnoreRules()
	_ = ign.loadDir(r.base, "")

	var (
		out       []grepLine
		truncated bool
	)
	limit := r.fetchLines()
	entries := 0
	walkErr := filepath.WalkDir(r.base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, rerr := filepath.Rel(r.base, path)
		if rerr != nil || rel == "." {
			return nil
		}
		if d.IsDir() {
			if skipDirNames[d.Name()] || ign.ignored(rel, true) {
				return fs.SkipDir
			}
			_ = ign.loadDir(path, filepath.ToSlash(rel))
			return nil
		}
		if ign.ignored(rel, false) || !filter.match(filepath.ToSlash(rel), d.Name()) {
			return nil
		}
		fileLines, ferr := grepFile(ctx, path, filepath.ToSlash(rel), re, r)
		if ferr != nil {
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
	if walkErr != nil && walkErr != fs.SkipAll {
		return nil, false, walkErr
	}
	return out, truncated, nil
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
func grepFile(ctx *Context, path, rel string, re *regexp.Regexp, r *grepRequest) ([]grepLine, error) {
	data, ok := readGrepFile(ctx, path)
	if !ok {
		return nil, nil
	}
	text := string(data)
	if strings.IndexByte(text, 0) >= 0 {
		return nil, nil // binary
	}
	lines := splitGrepLines(text)
	var matched []int
	if r.multiline {
		starts := lineStartOffsets(lines)
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
		return nil, nil
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
	return out, nil
}

// readGrepFile reads a bounded prefix of a file through the sandbox resolver,
// refusing symlinks under strict mode like every other read path. ok=false means
// "skip this file", not an error: an unreadable or oversized file is noise.
func readGrepFile(ctx *Context, path string) ([]byte, bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Size() > int64(ctx.readLimit()) {
		return nil, false
	}
	readPath := path
	if ctx.Sandbox != nil {
		readPath, err = ctx.Sandbox.ResolveRead(path)
		if err != nil {
			return nil, false
		}
	}
	flags := os.O_RDONLY
	if ctx.Sandbox != nil && ctx.Sandbox.CurrentMode() == sandbox.ModeStrict {
		flags |= syscall.O_NOFOLLOW
	}
	f, err := os.OpenFile(readPath, flags, 0)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, int64(ctx.readLimit())+1))
	if err != nil || len(data) > ctx.readLimit() {
		return nil, false
	}
	return data, true
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

// ---------- gitignore ----------

type ignoreRule struct {
	re *regexp.Regexp
	// contents is set for directory-only rules and matches paths underneath the
	// ignored directory, so a file is excluded even when the walk reached it
	// without pruning its parent first.
	contents *regexp.Regexp
	negate   bool
	dirOnly  bool
}

// ignoreRules accumulates .gitignore patterns as the walk descends, so a rule
// declared deeper in the tree overrides one declared above it.
type ignoreRules struct {
	rules []ignoreRule
}

func newIgnoreRules() *ignoreRules { return &ignoreRules{} }

func (ir *ignoreRules) loadDir(dir, relDir string) error {
	data, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	prefix := ""
	if relDir != "" {
		prefix = relDir + "/"
	}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, " \t")
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		negate := strings.HasPrefix(line, "!")
		if negate {
			line = strings.TrimSpace(line[1:])
		}
		line = strings.TrimPrefix(line, `\!`)
		dirOnly := strings.HasSuffix(line, "/")
		line = strings.TrimSuffix(line, "/")
		if line == "" {
			continue
		}
		anchored := strings.HasPrefix(line, "/") || strings.Contains(line, "/")
		line = strings.TrimPrefix(line, "/")
		body := ignorePatternBody(prefix, line, anchored)
		re, err := regexp.Compile(body + "(/.*)?$")
		if err != nil {
			continue
		}
		rule := ignoreRule{re: re, negate: negate, dirOnly: dirOnly}
		if dirOnly {
			if contents, cerr := regexp.Compile(body + "/[^/].*$"); cerr == nil {
				rule.contents = contents
			}
		}
		ir.rules = append(ir.rules, rule)
	}
	return nil
}

// ignorePatternBody translates a gitignore pattern into the anchored regexp
// prefix shared by the entry and contents forms.
func ignorePatternBody(prefix, pattern string, anchored bool) string {
	escaped := escapeIgnorePattern(pattern)
	// Ordered: `**` spans separators, `*` and `?` do not.
	escaped = strings.ReplaceAll(escaped, `\*\*`, "\x00")
	escaped = strings.ReplaceAll(escaped, `\*`, `[^/]*`)
	escaped = strings.ReplaceAll(escaped, `\?`, `[^/]`)
	body := regexp.QuoteMeta(prefix) + strings.ReplaceAll(escaped, "\x00", ".*")
	if anchored {
		return "^" + body
	}
	// Unanchored patterns match the basename at any depth.
	return "(^|.*/)" + body
}

func escapeIgnorePattern(p string) string {
	var b strings.Builder
	for _, r := range p {
		switch r {
		case '*', '?', '[', ']', '(', ')', '+', '.', '^', '$', '|', '{', '}', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (ir *ignoreRules) ignored(rel string, isDir bool) bool {
	ignored := false
	for _, rule := range ir.rules {
		re := rule.re
		if rule.dirOnly && !isDir {
			re = rule.contents
		}
		if re == nil {
			continue
		}
		if re.MatchString(rel) {
			ignored = !rule.negate
		}
	}
	return ignored
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
		windowed := applyGrepWindow(lines, r)
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
		truncated = truncated || len(lines) > r.fetchLines()
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
	fmt.Fprintf(sb, "…(%d more result(s) available — refine the pattern or pass offset/head_limit)\n", remaining)
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
			out[i].line++
			continue
		}
		index[l.file] = len(out)
		out = append(out, grepLine{file: l.file, line: 1, match: true})
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
