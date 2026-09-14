package tui

import (
	"strings"
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
		for _, line := range file.lines {
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
		mark, style = "+", styleToolOK
	case diffDeletion:
		mark, style = "-", styleToolErr
	}
	gutter := "      "
	if line.oldNo > 0 || line.newNo > 0 {
		gutter = formatDiffNumber(line.oldNo) + " " + formatDiffNumber(line.newNo) + " "
	}
	text := strings.TrimPrefix(line.text, mark)
	row := gutter + mark + " " + text
	return style.Render(truncateDisplay(row, max(1, width)))
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
