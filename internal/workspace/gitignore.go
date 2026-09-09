package workspace

import (
	"os"
	"path/filepath"
	"strings"
)

// Matcher evaluates gitignore-style patterns. It implements the common subset
// of the gitignore format: comments, negation (!), directory-only patterns
// (trailing /), the ** glob, and pattern anchoring (leading /). Non-matching
// patterns are ignored rather than erroring, so malformed user files degrade
// gracefully.
type Matcher struct {
	patterns []ignoreRule
}

type ignoreRule struct {
	pattern  string // normalized pattern (no leading ! or /)
	negate   bool
	dirOnly  bool
	anchored bool
	hasSlash bool // whether the pattern contains a slash
	hasDbl   bool // whether the pattern contains **
}

// NewMatcher returns an empty matcher.
func NewMatcher() (*Matcher, error) { return &Matcher{}, nil }

// NewMatcherSafe returns an empty matcher (no error path).
func NewMatcherSafe() *Matcher { return &Matcher{} }

// LoadGitignores loads every .gitignore from repoRoot downward, deepest last
// (mirroring git precedence: deeper files override shallower ones).
func (m *Matcher) LoadGitignores(repoRoot string) error {
	if m == nil {
		m = &Matcher{}
	}
	var files []string
	_ = filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !d.IsDir() && d.Name() == ".gitignore" {
			files = append(files, path)
		}
		return nil
	})
	sortDepthAsc(files)
	for _, f := range files {
		base, err := filepath.Rel(repoRoot, filepath.Dir(f))
		if err != nil {
			continue
		}
		base = filepath.ToSlash(base)
		if base == "." {
			base = ""
		}
		if err := m.loadFile(f, base); err != nil {
			continue
		}
	}
	return nil
}

func sortDepthAsc(paths []string) {
	for i := 1; i < len(paths); i++ {
		for j := i; j > 0; j-- {
			depth := func(p string) int { return strings.Count(filepath.ToSlash(p), "/") }
			if depth(paths[j-1]) > depth(paths[j]) {
				paths[j-1], paths[j] = paths[j], paths[j-1]
			} else {
				break
			}
		}
	}
}

// loadFile parses one .gitignore file whose patterns apply under baseDir.
func (m *Matcher) loadFile(path, baseDir string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, "\r")
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rule := parseIgnoreLine(line)
		// Patterns from a nested .gitignore are relative to that directory.
		if baseDir != "" && !rule.anchored {
			rule.pattern = baseDir + "/" + rule.pattern
			rule.hasSlash = strings.Contains(rule.pattern, "/")
		}
		m.patterns = append(m.patterns, rule)
	}
	return nil
}

func parseIgnoreLine(line string) ignoreRule {
	r := ignoreRule{}
	if strings.HasPrefix(line, "!") {
		r.negate = true
		line = line[1:]
	}
	if strings.HasPrefix(line, "/") {
		r.anchored = true
		line = strings.TrimPrefix(line, "/")
	}
	if strings.HasSuffix(line, "/") {
		r.dirOnly = true
		line = strings.TrimSuffix(line, "/")
	}
	line = strings.TrimSuffix(line, "/")
	if line == "" {
		return r
	}
	r.pattern = line
	r.hasSlash = strings.Contains(line, "/")
	r.hasDbl = strings.Contains(line, "**")
	return r
}

// Match reports whether relPath is ignored. If a later pattern negates an
// earlier ignore, Match returns (false, true) to signal "explicitly kept".
// The second return is true when the path matched any pattern at all.
func (m *Matcher) Match(rel string, isDir bool) (ignored, matched bool) {
	if m == nil || len(m.patterns) == 0 {
		return false, false
	}
	rel = filepath.ToSlash(rel)
	ignored = false
	for _, r := range m.patterns {
		if r.pattern == "" {
			continue
		}
		hit := matchRule(r, rel, isDir)
		if !hit {
			continue
		}
		matched = true
		ignored = !r.negate
	}
	return ignored, matched
}

func matchRule(r ignoreRule, rel string, isDir bool) bool {
	p := r.pattern
	if p == "" {
		return false
	}
	if p == rel {
		return true
	}

	// Directory-only rules also cover everything beneath the matched dir.
	if r.dirOnly {
		if strings.HasPrefix(rel, p+"/") {
			return true
		}
		if !isDir {
			return false
		}
	}

	// Patterns containing a slash are relative to the ignore-file location.
	if r.hasSlash {
		if r.hasDbl {
			return doubleStarMatch(p, rel)
		}
		if strings.HasSuffix(rel, "/"+p) {
			return true
		}
		if isDir && strings.HasPrefix(rel, p+"/") {
			return true
		}
		return false
	}

	// Slash-less pattern.
	if r.anchored {
		// Anchored to the ignore-file's directory: only matches at root.
		if strings.Contains(rel, "/") {
			return false
		}
		return globMatchSeg(p, rel)
	}

	// Non-anchored slash-less patterns match any basename segment.
	segments := strings.Split(rel, "/")
	for _, seg := range segments {
		if globMatchSeg(p, seg) {
			return true
		}
	}
	return false
}

// globMatchSeg matches a single segment (no slash in pattern).
func globMatchSeg(pattern, name string) bool {
	return simpleGlob(pattern, name)
}

// simpleGlob implements * and ? over a single string (no path separators).
func simpleGlob(pattern, s string) bool {
	if pattern == "*" {
		return true
	}
	// Classic iterative glob without star nesting issues.
	px, sx := 0, 0
	star := -1
	mark := 0
	for sx < len(s) {
		if px < len(pattern) && (pattern[px] == '?' || pattern[px] == s[sx]) {
			px++
			sx++
		} else if px < len(pattern) && pattern[px] == '*' {
			star = px
			mark = sx
			px++
		} else if star != -1 {
			px = star + 1
			mark++
			sx = mark
		} else {
			return false
		}
	}
	for px < len(pattern) && pattern[px] == '*' {
		px++
	}
	return px == len(pattern)
}

// doubleStarMatch matches patterns that may contain ** across segments.
func doubleStarMatch(pattern, s string) bool {
	// Split both into segments and do a DP match.
	psegs := strings.Split(pattern, "/")
	ssegs := strings.Split(s, "/")
	return dsMatch(psegs, ssegs)
}

func dsMatch(p, s []string) bool {
	dp := make([][]bool, len(p)+1)
	for i := range dp {
		dp[i] = make([]bool, len(s)+1)
	}
	dp[0][0] = true
	for i := 1; i <= len(p); i++ {
		if p[i-1] == "**" {
			dp[i][0] = dp[i-1][0]
			for j := 1; j <= len(s); j++ {
				dp[i][j] = dp[i-1][j] || dp[i][j-1]
			}
		} else {
			for j := 1; j <= len(s); j++ {
				if simpleGlob(p[i-1], s[j-1]) {
					dp[i][j] = dp[i-1][j-1]
				}
			}
		}
	}
	return dp[len(p)][len(s)]
}
