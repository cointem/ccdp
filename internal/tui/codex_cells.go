// Presentation adapted from OpenAI Codex (Apache-2.0).
// See third_party/codex for source revision, attribution and modifications.
package tui

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Explicit wrapping preserves the gutter and background when the terminal is narrow.
func renderUserCell(text string, width int) string {
	width = max(1, width)
	lines := wrapDisplay(strings.TrimRight(text, "\n"), max(1, width-3))
	rows := []string{""}
	for i, line := range lines {
		prefix := "  "
		if i == 0 {
			prefix = "› "
		}
		rows = append(rows, prefix+line)
	}
	rows = append(rows, "")
	return lipgloss.NewStyle().Background(colorInputBackground).Foreground(colorText).Width(width).MaxWidth(width).Render(strings.Join(rows, "\n"))
}

func renderAssistantCell(text string, width int, workspace ...string) string {
	body := renderMarkdown(text, max(1, width-2), workspace...)
	if strings.TrimSpace(body) == "" {
		return ""
	}
	lines := wrapDisplay(body, max(1, width-2))
	for i, line := range lines {
		prefix := "  "
		if i == 0 {
			prefix = styleStatus.Render("• ")
		}
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}

func displayToolPath(path, workspace string) string {
	if path == "" || workspace == "" || !filepath.IsAbs(path) {
		return path
	}
	rel, err := filepath.Rel(workspace, path)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return rel
	}
	return path
}

func explorationComplete(p toolPresentation) bool {
	if !isReadOnlyTool(p.Name) {
		return false
	}
	switch strings.ToLower(p.Status) {
	case "success", "done", "completed", "complete":
		return true
	}
	return false
}

func renderExplorationCell(p toolPresentation, width int) string {
	action := "Read"
	switch strings.ToLower(p.Name) {
	case "ls", "list", "listdir":
		action = "List"
	case "glob", "grep", "search", "find":
		action = "Search"
	}
	detail := toolArgumentSummary(p.Name, p.Args, p.Arguments)
	rows := []string{}
	if !p.ContinuesExploration {
		rows = append(rows, styleStatus.Render("• ")+styleAssistant.Bold(true).Render("Explored"))
	}
	for i, line := range wrapDisplay(action+" "+detail, max(1, width-4)) {
		prefix := "    "
		if i == 0 {
			prefix = "  └ "
		}
		rows = append(rows, styleStatus.Render(prefix)+styleToolOutput.Render(line))
	}
	return strings.Join(rows, "\n")
}

// Keep both the start and the outcome of command output, with an exact local
// omitted count. Upstream truncation is advertised separately.
func commandOutputPreview(output string, limit int, width int, truncated bool) []string {
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	if output == "" {
		lines = []string{"(no output)"}
	}
	selected := lines
	if len(lines) > limit {
		head := (limit + 1) / 2
		tail := limit / 2
		selected = append(append(append([]string{}, lines[:head]...), fmt.Sprintf("… +%d lines · /transcript", len(lines)-limit)), lines[len(lines)-tail:]...)
	}
	rows := []string{}
	for i, line := range selected {
		prefix := "    "
		if i == 0 {
			prefix = "  └ "
		}
		rows = append(rows, styleStatus.Render(prefix)+styleToolOutput.Render(ansi.Truncate(line, max(1, width-4), "…")))
	}
	if truncated {
		rows = append(rows, styleHints.Render("    … full output · /transcript"))
	}
	return rows
}

// Only confirmed writes get a preview. Edit arguments are replacement fragments,
// so omit absolute line numbers rather than claiming they start at file line 1.
func renderFileChangePreview(p toolPresentation, width int) string {
	if toolStatusIsFailure(strings.ToLower(p.Status)) || !toolStatusComplete(p.Status) || isUnifiedDiff(p.Output) {
		return ""
	}
	var lines []diffLine
	switch strings.ToLower(p.Name) {
	case "write":
		content, ok := p.Args["content"].(string)
		if !ok || content == "" {
			return ""
		}
		for i, line := range strings.Split(strings.TrimSuffix(content, "\n"), "\n") {
			lines = append(lines, diffLine{kind: diffContext, newNo: i + 1, text: " " + sanitizeANSI(line)})
		}
	case "edit":
		old, ok := p.Args["old_string"].(string)
		if !ok {
			return ""
		}
		updated, ok := p.Args["new_string"].(string)
		if !ok {
			return ""
		}
		for _, part := range []struct {
			text   string
			kind   diffLineKind
			prefix string
		}{{old, diffDeletion, "-"}, {updated, diffAddition, "+"}} {
			if part.text == "" {
				continue
			}
			for _, line := range strings.Split(strings.TrimSuffix(part.text, "\n"), "\n") {
				lines = append(lines, diffLine{kind: part.kind, text: part.prefix + sanitizeANSI(line)})
			}
		}
	default:
		return ""
	}
	label := "file contents"
	if strings.EqualFold(p.Name, "Edit") {
		label = "replacement preview"
	}
	rows := []string{styleHints.Render("  " + label)}
	selected := lines
	if len(lines) > 8 {
		selected = append(append(append([]diffLine{}, lines[:4]...),
			diffLine{kind: diffHunk, text: fmt.Sprintf("    … +%d lines · /transcript", len(lines)-8)}),
			lines[len(lines)-4:]...)
	}
	for _, line := range selected {
		if strings.EqualFold(p.Name, "Write") && line.kind == diffContext {
			rows = append(rows, styleAssistant.Render(ansi.Truncate(fmt.Sprintf("  %4d  %s", line.newNo, strings.TrimPrefix(line.text, " ")), max(1, width), "…")))
		} else {
			rows = append(rows, renderDiffLine(line, width))
		}
	}

	return strings.Join(rows, "\n")
}
func toolStatusComplete(status string) bool {
	switch strings.ToLower(status) {
	case "success", "done", "completed", "complete":
		return true
	}
	return false
}
