package tui

// This file contains the deliberately small Markdown projection used by the
// transcript.  It is a display parser: historyCell.text remains the source of
// truth, and callers that copy or export a message continue to use that raw
// text.  Keeping the parser here also makes it possible to render streamed
// assistant text conservatively without teaching tool output about Markdown.

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/rivo/uniseg"
)

type markdownBlockKind uint8

const (
	markdownParagraph markdownBlockKind = iota
	markdownHeading
	markdownList
	markdownQuote
	markdownCode
	markdownTable
)

// markdownBlock is intentionally source-bearing.  Rendered lines are a
// projection and can be discarded or recomputed after a resize or theme
// change without losing the original message.
type markdownBlock struct {
	kind     markdownBlockKind
	source   string
	lines    []string
	level    int
	language string
}

var (
	markdownHeadingRE  = regexp.MustCompile(`^(#{1,6})[ \t]+(.+?)\s*$`)
	markdownListRE     = regexp.MustCompile(`^([ \t]*)([-+*]|[0-9]+[.)])[ \t]+(.*)$`)
	markdownFenceRE    = regexp.MustCompile("^```[ \\t]*([^ \\t`]*)[ \\t]*$")
	markdownLinkRE     = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)(?:\s+[^)]*)?\)`)
	markdownTableSepRE = regexp.MustCompile(`^:?-{1,}:?$`)
)

const maxMarkdownSourceBytes = 512 * 1024
const maxMarkdownLines = 8192

// A source block can be much larger than the managed frame. Keep parsing
// bounded while retaining the opening fence and the newest tail so an active
// stream shows its latest output; the complete source remains on historyCell and
// is available to the reader.
const maxMarkdownPreviewLines = 256

// parseMarkdownBlocks recognizes the block constructs we can lay out safely
// in a terminal.  Unknown syntax is retained as ordinary paragraph text.
func parseMarkdownBlocks(source string) []markdownBlock {
	blocks, _ := parseMarkdownBlocksBounded(source)
	return blocks
}

func parseMarkdownBlocksBounded(source string) ([]markdownBlock, bool) {
	truncated := false
	if len(source) > maxMarkdownSourceBytes {
		truncated = true
		source = source[:maxMarkdownSourceBytes]
	}
	raw := strings.Split(strings.ReplaceAll(source, "\r\n", "\n"), "\n")
	if len(raw) > maxMarkdownLines {
		truncated = true
		// Retain the first line (which is normally a fence/heading) and the
		// newest bounded tail. This prevents an unfinished code fence from
		// spending the whole frame on stale prefix output while preserving a
		// clear continuation marker below the projection.
		raw = append([]string{raw[0]}, raw[len(raw)-maxMarkdownPreviewLines+1:]...)
	}
	if len(raw) > maxMarkdownPreviewLines {
		// Even sources below the parser's safety cap can be too large for a
		// streaming frame. Render a tail-sized source projection. A normal
		// transcript keeps its raw source, and the reader can show every line.
		truncated = true
		raw = append([]string{raw[0]}, raw[len(raw)-maxMarkdownPreviewLines+1:]...)
	}
	blocks := make([]markdownBlock, 0, len(raw)/2+1)
	for i := 0; i < len(raw); {
		line := raw[i]
		if strings.TrimSpace(line) == "" {
			i++
			continue
		}
		if match := markdownFenceRE.FindStringSubmatch(line); match != nil {
			lang := match[1]
			start := i
			i++
			closed := false
			for i < len(raw) && !isMarkdownFenceEnd(raw[i]) {
				i++
			}
			if i < len(raw) {
				closed = true
				i++
			}
			contentEnd := i
			if closed {
				contentEnd = i - 1
			}
			if contentEnd < start+1 {
				contentEnd = start + 1
			}
			lines := append([]string(nil), raw[start+1:contentEnd]...)
			blocks = append(blocks, markdownBlock{
				kind: markdownCode, source: strings.Join(raw[start:i], "\n"),
				lines: lines, language: lang,
			})
			continue
		}
		if match := markdownHeadingRE.FindStringSubmatch(line); match != nil {
			blocks = append(blocks, markdownBlock{kind: markdownHeading, source: line, lines: []string{match[2]}, level: len(match[1])})
			i++
			continue
		}
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), ">") {
			start := i
			lines := make([]string, 0, 2)
			for i < len(raw) {
				trimmed := strings.TrimLeft(raw[i], " \t")
				if !strings.HasPrefix(trimmed, ">") {
					break
				}
				value := strings.TrimSpace(strings.TrimPrefix(trimmed, ">"))
				lines = append(lines, value)
				i++
			}
			blocks = append(blocks, markdownBlock{kind: markdownQuote, source: strings.Join(raw[start:i], "\n"), lines: lines})
			continue
		}
		if markdownListRE.MatchString(line) {
			start := i
			lines := make([]string, 0, 2)
			for i < len(raw) && markdownListRE.MatchString(raw[i]) {
				lines = append(lines, raw[i])
				i++
			}
			blocks = append(blocks, markdownBlock{kind: markdownList, source: strings.Join(raw[start:i], "\n"), lines: lines})
			continue
		}
		if i+1 < len(raw) && isMarkdownTableRow(line) && isMarkdownTableSeparator(raw[i+1]) {
			start := i
			i += 2
			for i < len(raw) && isMarkdownTableRow(raw[i]) && strings.TrimSpace(raw[i]) != "" {
				i++
			}
			blocks = append(blocks, markdownBlock{kind: markdownTable, source: strings.Join(raw[start:i], "\n"), lines: append([]string(nil), raw[start:i]...)})
			continue
		}

		start := i
		for i < len(raw) && strings.TrimSpace(raw[i]) != "" &&
			!markdownFenceRE.MatchString(raw[i]) &&
			markdownHeadingRE.FindStringSubmatch(raw[i]) == nil &&
			!strings.HasPrefix(strings.TrimLeft(raw[i], " \t"), ">") &&
			!markdownListRE.MatchString(raw[i]) {
			if i+1 < len(raw) && isMarkdownTableRow(raw[i]) && isMarkdownTableSeparator(raw[i+1]) {
				break
			}
			i++
		}
		if i == start {
			// Make progress for a line that looked like a table or another
			// construct but did not pass the complete block check.
			i++
		}
		blocks = append(blocks, markdownBlock{kind: markdownParagraph, source: strings.Join(raw[start:i], "\n"), lines: append([]string(nil), raw[start:i]...)})
	}
	return blocks, truncated
}

func isMarkdownFenceEnd(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, "```") && strings.Trim(trimmed, "`") == ""
}

func isMarkdownTableRow(line string) bool {
	return strings.Count(line, "|") >= 1
}

func isMarkdownTableSeparator(line string) bool {
	parts := markdownTableCells(line)
	if len(parts) < 1 {
		return false
	}
	for _, part := range parts {
		if !markdownTableSepRE.MatchString(strings.TrimSpace(part)) {
			return false
		}
	}
	return true
}

func markdownTableCells(line string) []string {
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "|") {
		line = strings.TrimPrefix(line, "|")
	}
	if strings.HasSuffix(line, "|") {
		line = strings.TrimSuffix(line, "|")
	}
	parts := strings.Split(line, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(strings.ReplaceAll(parts[i], `\|`, `|`))
	}
	return parts
}

// renderMarkdown is the assistant-only rich text projection.  width is a
// terminal cell width; source is never changed by this function.
func renderMarkdown(source string, width int, workspace ...string) string {
	width = max(1, width)
	blocks, truncated := parseMarkdownBlocksBounded(source)
	if len(blocks) == 0 {
		return ""
	}
	rows := make([]string, 0, len(blocks)*2)
	for bi, block := range blocks {
		if bi > 0 {
			rows = append(rows, "")
		}
		rows = append(rows, renderMarkdownBlock(block, width, workspace...)...)
	}
	if truncated {
		rows = append(rows, "", styleStatus.Render("… content continues in the reader"))
	}
	return strings.TrimRight(strings.Join(rows, "\n"), "\n")
}

func renderMarkdownBlock(block markdownBlock, width int, workspace ...string) []string {
	switch block.kind {
	case markdownHeading:
		if len(block.lines) == 0 {
			return nil
		}
		prefix := strings.Repeat("#", max(1, min(6, block.level)))
		_ = prefix // The source marker is intentionally omitted from display.
		return []string{styleAssistant.Bold(true).Render(renderMarkdownInline(block.lines[0], workspace...))}
	case markdownCode:
		if (strings.EqualFold(block.language, "diff") || strings.EqualFold(block.language, "patch")) && isUnifiedDiff(strings.Join(block.lines, "\n")) {
			return strings.Split(renderDiff(strings.Join(block.lines, "\n"), width), "\n")
		}
		return renderMarkdownCode(block, width)
	case markdownList:
		out := make([]string, 0, len(block.lines))
		for _, line := range block.lines {
			match := markdownListRE.FindStringSubmatch(line)
			if match == nil {
				out = append(out, styleAssistant.Render(line))
				continue
			}
			indent := match[1]
			marker := match[2]
			body := match[3]
			out = append(out, styleBrand.Render(indent+marker)+" "+renderMarkdownInline(body, workspace...))
		}
		return out
	case markdownQuote:
		out := make([]string, 0, len(block.lines))
		for _, line := range block.lines {
			out = append(out, styleDivider.Render("│ ")+styleStatus.Render(renderMarkdownInline(line, workspace...)))
		}
		return out
	case markdownTable:
		return renderMarkdownTable(block.lines, width, workspace...)
	default:
		out := make([]string, 0, len(block.lines))
		for _, line := range block.lines {
			out = append(out, renderMarkdownInline(line, workspace...))
		}
		return out
	}
}

func renderMarkdownCode(block markdownBlock, width int) []string {
	out := make([]string, 0, len(block.lines)+1)
	lang := strings.ToLower(strings.TrimSpace(block.language))
	if lang != "" && lang != "text" && lang != "txt" && lang != "plain" && lang != "plaintext" && lang != "output" && lang != "none" {
		out = append(out, styleStatus.Render("  "+block.language))
	}
	codeWidth := max(4, width-2)
	highlighted := highlightCode(block.lines, block.language, isDarkTheme())
	for i, line := range block.lines {
		styled := ""
		if i < len(highlighted) && highlighted[i] != "" {
			styled = highlighted[i]
		} else {
			styled = styleAssistant.Render(line)
		}
		// Keep source indentation visible. lipgloss wraps the final display
		// line at the component boundary if a code line is very long.
		if lipgloss.Width(line) <= codeWidth {
			out = append(out, styleDivider.Render("│ ")+styled)
			continue
		}
		parts := wrapMarkdownCodeLine(line, codeWidth)
		for pi, part := range parts {
			prefix := "│ "
			if pi > 0 {
				prefix = "│ ·"
			}
			out = append(out, styleDivider.Render(prefix)+styleAssistant.Render(part))
		}
	}
	return out
}

func wrapMarkdownCodeLine(line string, width int) []string {
	if width <= 0 || lipgloss.Width(line) <= width {
		return []string{line}
	}
	parts := make([]string, 0, 2)
	var current strings.Builder
	used := 0
	graphemes := uniseg.NewGraphemes(line)
	for graphemes.Next() {
		cluster := graphemes.Str()
		rw := lipgloss.Width(cluster)
		if rw > 0 && used+rw > width && current.Len() > 0 {
			parts = append(parts, current.String())
			current.Reset()
			used = 0
		}
		current.WriteString(cluster)
		used += rw
	}
	if current.Len() > 0 || len(parts) == 0 {
		parts = append(parts, current.String())
	}
	return parts
}

func renderMarkdownTable(raw []string, width int, workspace ...string) []string {
	if len(raw) < 2 {
		return nil
	}
	headers := markdownTableCells(raw[0])
	rows := make([][]string, 0, len(raw)-2)
	for _, line := range raw[2:] {
		rows = append(rows, markdownTableCells(line))
	}
	columns := len(headers)
	for _, row := range rows {
		columns = max(columns, len(row))
	}
	if columns == 0 {
		return nil
	}
	// A table should not make a narrow terminal unreadable. Key/value rows
	// retain every cell and are easier to scan than a clipped grid.
	if width < 48 || columns > 4 {
		return renderMarkdownTableKeyValue(headers, rows, width, workspace...)
	}
	widths := make([]int, columns)
	all := append([][]string{headers}, rows...)
	for _, row := range all {
		for ci, value := range row {
			widths[ci] = max(widths[ci], lipgloss.Width(value))
		}
	}
	for ci := range widths {
		cellLimit := max(3, width/columns-3)
		if widths[ci] > cellLimit {
			// Keep every table value available in the display projection. A
			// key/value fallback can wrap long cells without silently replacing
			// their tail with an ellipsis.
			return renderMarkdownTableKeyValue(headers, rows, width, workspace...)
		}
		widths[ci] = min(widths[ci], cellLimit)
	}
	separatorWidth := columns*3 + (columns-1)*3
	for separatorWidth > width && columns > 1 {
		columns--
		widths = widths[:columns]
		separatorWidth = columns*3 + (columns-1)*3
	}
	formatRow := func(row []string, header bool) string {
		cells := make([]string, columns)
		for ci := range cells {
			value := ""
			if ci < len(row) {
				value = row[ci]
			}
			value = truncateDisplay(value, widths[ci])
			value += strings.Repeat(" ", max(0, widths[ci]-lipgloss.Width(value)))
			cells[ci] = value
		}
		rowText := "│ " + strings.Join(cells, " │ ") + " │"
		if header {
			return styleAssistant.Bold(true).Render(rowText)
		}
		return styleAssistant.Render(rowText)
	}
	out := []string{formatRow(headers, true)}
	for _, row := range rows {
		out = append(out, formatRow(row, false))
	}
	return out
}

func renderMarkdownTableKeyValue(headers []string, rows [][]string, width int, workspace ...string) []string {
	out := make([]string, 0, len(rows)*2+1)
	for ri, row := range rows {
		for ci, value := range row {
			key := "value"
			if ci < len(headers) && strings.TrimSpace(headers[ci]) != "" {
				key = headers[ci]
			}
			prefix := styleStatus.Render(key + ": ")
			if ri == 0 {
				prefix = styleAssistant.Bold(true).Render(key + ": ")
			}
			out = append(out, presentationRows(prefix+renderMarkdownInline(value, workspace...), width)...)
		}
	}
	if len(out) == 0 {
		for _, header := range headers {
			out = append(out, presentationRows(styleAssistant.Bold(true).Render(header), width)...)
		}
	}
	return out
}

// renderMarkdownInline handles only inline constructs with unambiguous
// delimiters.  A malformed marker is emitted as ordinary text, avoiding the
// common failure mode where shell output gets swallowed by a greedy parser.
func renderMarkdownInline(source string, workspace ...string) string {
	if source == "" {
		return ""
	}
	var out strings.Builder
	flush := func(text string) {
		if text != "" {
			out.WriteString(styleAssistant.Render(text))
		}
	}
	plain := strings.Builder{}
	for i := 0; i < len(source); {
		if source[i] == '\\' && i+1 < len(source) && strings.ContainsRune(`\\*_`+"`~[]()", rune(source[i+1])) {
			plain.WriteByte(source[i+1])
			i += 2
			continue
		}
		if source[i] == '`' {
			if end := strings.IndexByte(source[i+1:], '`'); end >= 0 {
				flush(plain.String())
				plain.Reset()
				code := source[i+1 : i+1+end]
				out.WriteString(styleMarkdownCode.Render(code))
				i += end + 2
				continue
			}
		}
		if source[i] == '[' {
			if match := markdownLinkRE.FindStringSubmatchIndex(source[i:]); match != nil && match[0] == 0 {
				flush(plain.String())
				plain.Reset()
				label := source[i+match[2] : i+match[3]]
				url := source[i+match[4] : i+match[5]]
				out.WriteString(renderTerminalLink(renderMarkdownInlinePlain(label), url, workspace...))
				i += match[1]
				continue
			}
		}
		marker, style := markdownInlineMarker(source, i)
		if marker != "" {
			if end := strings.Index(source[i+len(marker):], marker); end >= 0 {
				flush(plain.String())
				plain.Reset()
				value := source[i+len(marker) : i+len(marker)+end]
				out.WriteString(style.Render(renderMarkdownInlinePlain(value)))
				i += len(marker)*2 + end
				continue
			}
		}
		_, _ = out.WriteString("")
		plain.WriteByte(source[i])
		i++
	}
	flush(plain.String())
	return out.String()
}

func markdownInlineMarker(source string, at int) (string, lipgloss.Style) {
	if at >= len(source) {
		return "", lipgloss.NewStyle()
	}
	switch {
	case strings.HasPrefix(source[at:], "**"):
		if markdownIntrawordMarker(source, at, 2) {
			return "", lipgloss.NewStyle()
		}
		return "**", styleAssistant.Bold(true)
	case strings.HasPrefix(source[at:], "__"):
		if markdownIntrawordMarker(source, at, 2) {
			return "", lipgloss.NewStyle()
		}
		return "__", styleAssistant.Bold(true)
	case strings.HasPrefix(source[at:], "~~"):
		if markdownIntrawordMarker(source, at, 2) {
			return "", lipgloss.NewStyle()
		}
		return "~~", styleStatus.Strikethrough(true)
	case source[at] == '*':
		if markdownIntrawordMarker(source, at, 1) {
			return "", lipgloss.NewStyle()
		}
		return "*", styleAssistant.Italic(true)
	case source[at] == '_':
		if markdownIntrawordMarker(source, at, 1) {
			return "", lipgloss.NewStyle()
		}
		return "_", styleAssistant.Italic(true)
	default:
		return "", lipgloss.NewStyle()
	}
}

func markdownIntrawordMarker(source string, at, size int) bool {
	if at > 0 {
		previous, _ := utf8.DecodeLastRuneInString(source[:at])
		if at+size < len(source) {
			next, _ := utf8.DecodeRuneInString(source[at+size:])
			return markdownTokenIsWord(previous) && markdownTokenIsWord(next)
		}
	}
	return false
}

// renderMarkdownInlinePlain returns visible link/emphasis content without
// recursively styling it. It is useful for nested constructs where terminal
// escape sequences would make a width calculation ambiguous.
func renderMarkdownInlinePlain(source string) string {
	source = markdownLinkRE.ReplaceAllString(source, "$1")
	source = strings.ReplaceAll(source, "**", "")
	source = strings.ReplaceAll(source, "__", "")
	source = strings.ReplaceAll(source, "~~", "")
	source = strings.ReplaceAll(source, "`", "")
	return source
}

func markdownTokenIsWord(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

func atoiOrZero(value string) int {
	n, _ := strconv.Atoi(value)
	return n
}
