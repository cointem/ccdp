package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

type diffLineKind uint8

const (
	diffContext diffLineKind = iota
	diffAddition
	diffDeletion
	diffHeader
	diffHunk
)

type diffLine struct {
	kind  diffLineKind
	oldNo int
	newNo int
	text  string
}

type diffFile struct {
	path      string
	lines     []diffLine
	additions int
	deletions int
}

// parseUnifiedDiff accepts ordinary unified diffs and keeps unfamiliar lines
// as context. It does not infer a diff from prose such as "Edited file".
func parseUnifiedDiff(source string) []diffFile {
	lines := strings.Split(strings.ReplaceAll(source, "\r\n", "\n"), "\n")
	files := make([]diffFile, 0, 1)
	current := -1
	oldNo, newNo := 0, 0
	for _, raw := range lines {
		if strings.HasPrefix(raw, "diff --git ") {
			path := strings.TrimSpace(raw)
			fields := strings.Fields(raw)
			if len(fields) >= 4 {
				path = strings.TrimPrefix(fields[len(fields)-1], "b/")
			}
			files = append(files, diffFile{path: path})
			current = len(files) - 1
			files[current].lines = append(files[current].lines, diffLine{kind: diffHeader, text: raw})
			continue
		}
		if strings.HasPrefix(raw, "--- ") || strings.HasPrefix(raw, "+++ ") {
			// Diffs without a preceding `diff --git` header use repeated ---/+++
			// pairs to delimit files. Once the current file has a path and some
			// content, a new --- line starts a new file projection. A git-style
			// `diff --git` header already names the current file, so its first ---
			// line belongs to that file rather than opening a duplicate heading.
			if strings.HasPrefix(raw, "--- ") && current >= 0 && files[current].path != "" && diffFileHasOldHeader(files[current]) {
				files = append(files, diffFile{})
				current = len(files) - 1
				oldNo, newNo = 0, 0
			}
			if current < 0 {
				files = append(files, diffFile{})
				current = len(files) - 1
			}
			if strings.HasPrefix(raw, "+++ ") && files[current].path == "" {
				path := strings.TrimSpace(strings.TrimPrefix(raw, "+++ "))
				if fields := strings.Fields(path); len(fields) > 0 {
					path = fields[0]
				}
				path = strings.TrimPrefix(path, "b/")
				files[current].path = strings.TrimPrefix(path, "a/")
			}
			files[current].lines = append(files[current].lines, diffLine{kind: diffHeader, text: raw})
			continue
		}
		if strings.HasPrefix(raw, "@@") {
			oldNo, newNo = parseHunkNumbers(raw)
			if current < 0 {
				files = append(files, diffFile{})
				current = len(files) - 1
			}
			files[current].lines = append(files[current].lines, diffLine{kind: diffHunk, text: raw})
			continue
		}
		if current < 0 || raw == "" || strings.HasPrefix(raw, "\\ No newline") || isDiffMetadata(raw) {
			continue
		}
		line := diffLine{kind: diffContext, oldNo: oldNo, newNo: newNo, text: raw}
		switch {
		case strings.HasPrefix(raw, "+"):
			line.kind = diffAddition
			line.oldNo = 0
			line.newNo = newNo
			newNo++
			files[current].additions++
		case strings.HasPrefix(raw, "-"):
			line.kind = diffDeletion
			line.oldNo = oldNo
			line.newNo = 0
			oldNo++
			files[current].deletions++
		default:
			if strings.HasPrefix(raw, " ") {
				line.text = raw
			}
			oldNo++
			newNo++
		}
		files[current].lines = append(files[current].lines, line)
	}
	return files
}

func diffFileHasOldHeader(file diffFile) bool {
	for _, line := range file.lines {
		if line.kind == diffHeader && strings.HasPrefix(line.text, "--- ") {
			return true
		}
	}
	return false
}

func isDiffMetadata(line string) bool {
	for _, prefix := range []string{
		"index ", "old mode ", "new mode ", "new file mode ", "deleted file mode ",
		"similarity index ", "dissimilarity index ", "rename from ", "rename to ",
		"copy from ", "copy to ", "Binary files ", "GIT binary patch", "literal ", "delta ",
	} {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

func parseHunkNumbers(line string) (int, int) {
	// @@ -old[,count] +new[,count] @@ optional heading
	fields := strings.Fields(line)
	oldNo, newNo := 0, 0
	for _, field := range fields {
		if strings.HasPrefix(field, "-") && len(field) > 1 {
			oldNo = atoiOrZero(strings.Trim(strings.TrimPrefix(field, "-"), ","))
			if comma := strings.IndexByte(strings.TrimPrefix(field, "-"), ','); comma >= 0 {
				oldNo = atoiOrZero(strings.TrimPrefix(field, "-")[:comma])
			}
		}
		if strings.HasPrefix(field, "+") && len(field) > 1 {
			value := strings.TrimPrefix(field, "+")
			newNo = atoiOrZero(strings.SplitN(value, ",", 2)[0])
		}
	}
	return oldNo, newNo
}

func isUnifiedDiff(source string) bool {
	return strings.Contains(source, "diff --git ") ||
		(strings.Contains(source, "\n@@ ") && strings.Contains(source, "\n+++ ")) ||
		(strings.HasPrefix(source, "@@ ") && strings.Contains(source, "\n+++ "))
}

func renderDiff(source string, width int) string {
	files := parseUnifiedDiff(source)
	if len(files) == 0 {
		return ""
	}
	rows := make([]string, 0, 16)
	for fi, file := range files {
		if fi > 0 {
			rows = append(rows, "")
		}
		name := file.path
		if name == "" {
			name = "changed file"
		}
		rows = append(rows, styleAssistant.Bold(true).Render("✎ "+truncateDisplay(name, max(1, width-2))))
		for li := 0; li < len(file.lines); li++ {
			line := file.lines[li]
			// Pair deletion followed directly by addition for word-level diff
			if line.kind == diffDeletion && li+1 < len(file.lines) && file.lines[li+1].kind == diffAddition {
				nextLine := file.lines[li+1]
				delRow, addRow := renderDiffLineWordPair(line, nextLine, width)
				rows = append(rows, delRow, addRow)
				li++ // skip nextLine since processed
				continue
			}
			rows = append(rows, renderDiffLine(line, width))
		}
	}
	return strings.Join(rows, "\n")
}

func renderDiffLine(line diffLine, width int) string {
	if line.kind == diffHeader || line.kind == diffHunk {
		return styleStatus.Render(truncateDisplay(line.text, width))
	}
	mark, style := " ", styleAssistant
	switch line.kind {
	case diffAddition:
		mark, style = "+", styleToolOK.Background(theme.Added).Width(max(1, width))
	case diffDeletion:
		mark, style = "-", styleToolErr.Background(theme.Removed).Width(max(1, width))
	}
	gutter := "      "
	if line.oldNo > 0 || line.newNo > 0 {
		gutter = formatDiffNumber(line.oldNo) + " " + formatDiffNumber(line.newNo) + " "
	}
	text := strings.TrimPrefix(line.text, mark)
	row := gutter + mark + " " + text
	return style.Render(ansi.Truncate(row, max(1, width), "…"))
}

type diffWordSegment struct {
	text    string
	changed bool
}

// tokenizeWords splits a line into alternating words and non-word tokens (spaces, symbols).
func tokenizeWords(s string) []string {
	if s == "" {
		return nil
	}
	tokens := make([]string, 0, 8)
	var cur strings.Builder
	isWordChar := func(r rune) bool {
		return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_'
	}
	runes := []rune(s)
	if len(runes) == 0 {
		return nil
	}
	inWord := isWordChar(runes[0])
	for _, r := range runes {
		w := isWordChar(r)
		if w != inWord {
			tokens = append(tokens, cur.String())
			cur.Reset()
			inWord = w
		}
		cur.WriteRune(r)
	}
	if cur.Len() > 0 {
		tokens = append(tokens, cur.String())
	}
	return tokens
}

// wordDiff computes token-level diff between oldLine and newLine using LCS.
func wordDiff(oldText, newText string) ([]diffWordSegment, []diffWordSegment) {
	oldTokens := tokenizeWords(oldText)
	newTokens := tokenizeWords(newText)

	// Bounded matrix: if too many tokens, fallback to unchanged
	if len(oldTokens) > 150 || len(newTokens) > 150 {
		return []diffWordSegment{{text: oldText, changed: false}}, []diffWordSegment{{text: newText, changed: false}}
	}

	m, n := len(oldTokens), len(newTokens)
	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}
	for i := 0; i < m; i++ {
		for j := 0; j < n; j++ {
			if oldTokens[i] == newTokens[j] {
				dp[i+1][j+1] = dp[i][j] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i+1][j+1] = dp[i+1][j]
			} else {
				dp[i+1][j+1] = dp[i][j+1]
			}
		}
	}

	// Backtrack to find unchanged vs changed tokens
	oldChanged := make([]bool, m)
	newChanged := make([]bool, n)
	i, j := m, n
	for i > 0 || j > 0 {
		if i > 0 && j > 0 && oldTokens[i-1] == newTokens[j-1] {
			oldChanged[i-1] = false
			newChanged[j-1] = false
			i--
			j--
		} else if j > 0 && (i == 0 || dp[i][j-1] >= dp[i-1][j]) {
			newChanged[j-1] = true
			j--
		} else if i > 0 && (j == 0 || dp[i][j-1] < dp[i-1][j]) {
			oldChanged[i-1] = true
			i--
		}
	}

	buildSegments := func(tokens []string, changed []bool) []diffWordSegment {
		segs := make([]diffWordSegment, 0, len(tokens))
		for k, t := range tokens {
			segs = append(segs, diffWordSegment{text: t, changed: changed[k]})
		}
		return segs
	}

	return buildSegments(oldTokens, oldChanged), buildSegments(newTokens, newChanged)
}

func renderDiffLineWordPair(delLine, addLine diffLine, width int) (string, string) {
	delGutter := "      "
	if delLine.oldNo > 0 || delLine.newNo > 0 {
		delGutter = formatDiffNumber(delLine.oldNo) + " " + formatDiffNumber(delLine.newNo) + " "
	}
	addGutter := "      "
	if addLine.oldNo > 0 || addLine.newNo > 0 {
		addGutter = formatDiffNumber(addLine.oldNo) + " " + formatDiffNumber(addLine.newNo) + " "
	}

	delRaw := strings.TrimPrefix(delLine.text, "-")
	addRaw := strings.TrimPrefix(addLine.text, "+")

	delSegs, addSegs := wordDiff(delRaw, addRaw)

	renderSegs := func(gutter, mark string, baseStyle, highlightStyle lipgloss.Style, segs []diffWordSegment) string {
		var row strings.Builder
		row.WriteString(baseStyle.Render(gutter + mark + " "))
		for _, s := range segs {
			if s.changed {
				row.WriteString(highlightStyle.Render(s.text))
			} else {
				row.WriteString(baseStyle.Render(s.text))
			}
		}
		return ansi.Truncate(row.String(), max(1, width), "…")
	}

	delOut := renderSegs(delGutter, "-", styleToolErr, styleDiffRemovedWord, delSegs)
	addOut := renderSegs(addGutter, "+", styleToolOK, styleDiffAddedWord, addSegs)
	return delOut, addOut
}

func formatDiffNumber(value int) string {
	if value <= 0 {
		return "   -"
	}
	return fmtDiffNumber(value)
}

func fmtDiffNumber(value int) string {
	text := strconvItoa(value)
	if len(text) >= 4 {
		return text
	}
	return strings.Repeat(" ", 4-len(text)) + text
}

func strconvItoa(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var buf [24]byte
	i := len(buf)
	for value > 0 {
		i--
		buf[i] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
