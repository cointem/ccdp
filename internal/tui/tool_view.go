package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// toolPresentation is a typed projection of a log item. Its Output field is
// the complete sanitized output; the renderer chooses a bounded preview while
// readers and copy/export paths can still use the original logItem.text.
type toolPresentation struct {
	ID        string
	Name      string
	Status    string
	Args      map[string]any
	Arguments string
	Output    string
	Truncated bool
}

func toolPresentationFromItem(item *logItem) toolPresentation {
	if item == nil {
		return toolPresentation{}
	}
	// Protocol transcript rows retain typed source fields. Prefer them over
	// parsing the compact display string; the parser below is only for legacy
	// in-package callers that construct logItem with text/toolMeta.
	if item.toolName != "" || item.toolArgsRaw != "" || item.toolOutput != "" || item.toolTruncated {
		args := make(map[string]any, len(item.toolArgs))
		for key, value := range item.toolArgs {
			if text, ok := value.(string); ok {
				args[key] = sanitizeANSI(text)
			} else {
				args[key] = value
			}
		}
		return toolPresentation{ID: sanitizeANSI(item.toolID), Name: sanitizeANSI(item.toolName), Status: sanitizeANSI(item.status), Args: args,
			Arguments: sanitizeANSI(item.toolArgsRaw), Output: sanitizeANSI(item.toolOutput), Truncated: item.toolTruncated}
	}
	source := item.text
	if item.sanitizedLen == len(source) && item.sanitized != source {
		// The cache can contain an older equal-length snapshot. Re-sanitize the
		// current source before parsing, just as renderItemWidth does.
		item.sanitized = sanitizeANSI(source)
		item.sanitizedLen = len(source)
	} else if item.sanitizedLen != len(source) || item.sanitized == "" && source != "" {
		item.sanitized = sanitizeANSI(source)
		item.sanitizedLen = len(source)
	}
	source = item.sanitized
	lines := strings.Split(source, "\n")
	p := toolPresentation{ID: item.toolID, Status: item.status}
	if len(lines) == 0 {
		return p
	}
	p.Name = strings.TrimSpace(lines[0])
	if p.Name == "" {
		p.Name = "Tool"
	}
	outputStart := 1
	if len(lines) > 1 && item.toolMeta {
		p.Arguments = strings.TrimSpace(lines[1])
		outputStart = 2
		if strings.HasPrefix(p.Arguments, "$ ") {
			p.Args = map[string]any{"command": strings.TrimSpace(strings.TrimPrefix(p.Arguments, "$ "))}
		} else {
			var args map[string]any
			if json.Unmarshal([]byte(p.Arguments), &args) == nil {
				p.Args = args
			}
		}
	}
	if outputStart < len(lines) {
		p.Output = strings.TrimRight(strings.Join(lines[outputStart:], "\n"), "\n")
	}
	if strings.Contains(p.Output, "[preview truncated;") {
		p.Truncated = true
	}
	return p
}

// renderToolView is shared by the transcript and typed protocol projection.
// The output is intentionally non-Markdown: shell and tool output frequently
// contains punctuation that should remain literal terminal text.
func renderToolView(item *logItem, width int) string {
	return renderToolPresentation(toolPresentationFromItem(item), width)
}

func renderToolPresentation(p toolPresentation, width int) string {
	width = max(1, width)
	name := p.Name
	if name == "" {
		name = "Tool"
	}
	meta := toolArgumentSummary(p.Name, p.Args, p.Arguments)
	status := strings.ToLower(strings.TrimSpace(p.Status))
	mark, stateStyle, stateText := "●", styleToolRun, "running"
	switch status {
	case "success", "done", "completed", "complete":
		mark, stateStyle, stateText = "✓", styleToolOK, "done"
	case "error", "failed", "failure":
		mark, stateStyle, stateText = "✗", styleToolErr, "error"
	case "denied", "rejected":
		mark, stateStyle, stateText = "✗", styleToolErr, "denied"
	case "cancelled", "canceled", "stopped":
		mark, stateStyle, stateText = "■", styleStatus, "stopped"
	}
	first := stateStyle.Render(mark) + " " + styleAssistant.Bold(true).Render(name) + stateStyle.Render("  "+stateText)
	if meta != "" {
		first += " " + styleStatus.Render(truncateDisplay(meta, max(1, width-lipgloss.Width(stripANSI(first))-1)))
	}
	rows := []string{first}
	output := strings.TrimRight(p.Output, "\n")
	if output == "" {
		if p.Truncated {
			hint := "… output available · /transcript"
			rows = append(rows, styleStatus.Render("  ⎿ "+hint))
		}
		return strings.Join(rows, "\n")
	}

	maxOutput := 1
	switch status {
	case "running", "", "queued", "starting":
		maxOutput = 3
	case "error", "failed", "failure":
		// Five detail rows plus the tool title keeps the error surface within
		// the six-row budget.
		maxOutput = 5
	default:
		maxOutput = 2
	}
	outputLines := strings.Split(output, "\n")
	if isUnifiedDiff(output) {
		// A diff gets a semantic marker and compact file/line summary. The full
		// source remains available through the reader entry.
		diff := parseUnifiedDiff(output)
		if summary := diffSummary(diff); summary != "" {
			outputLines = []string{summary}
		}
	}
	preview, omitted := boundedToolLines(outputLines, maxOutput, status == "running")
	if status == "error" || status == "failed" || status == "failure" {
		preview, omitted = boundedToolErrorLines(outputLines, maxOutput)
	}
	for _, line := range preview {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		rows = append(rows, styleDivider.Render("  ⎿ ")+styleToolOutput.Render(truncateDisplay(line, max(1, width-6))))
	}
	if omitted || p.Truncated {
		hint := "… output available · /transcript"
		if len(rows) > 1 {
			// Keep a successful tool to one title plus two rows, and avoid
			// making a running tail jump when new output arrives.
			rows[len(rows)-1] += " " + styleStatus.Render(hint)
		} else {
			rows = append(rows, styleStatus.Render("  ⎿ "+hint))
		}
	}
	return strings.Join(rows, "\n")
}

func boundedToolLines(lines []string, maxRows int, tail bool) ([]string, bool) {
	if maxRows <= 0 {
		maxRows = 1
	}
	clean := make([]string, 0, len(lines))
	for _, line := range lines {
		clean = append(clean, strings.TrimRight(line, "\r"))
	}
	if len(clean) <= maxRows {
		return clean, false
	}
	if tail {
		return clean[len(clean)-maxRows:], true
	}
	// For completed output keep the first line (usually the useful result)
	// and the final line (usually the command's conclusion) when there is room.
	if maxRows == 1 {
		return []string{clean[0]}, true
	}
	preview := append([]string{}, clean[:maxRows-1]...)
	preview = append(preview, clean[len(clean)-1])
	return preview, true
}

func boundedToolErrorLines(lines []string, maxRows int) ([]string, bool) {
	if maxRows <= 0 {
		maxRows = 1
	}
	clean := make([]string, 0, len(lines))
	for _, line := range lines {
		clean = append(clean, strings.TrimRight(line, "\r"))
	}
	if len(clean) <= maxRows {
		return clean, false
	}
	// Error output is most useful as its first cause followed by the newest
	// context. Keeping only the head can hide the command's actual failure when
	// a tool emits a long diagnostic prelude.
	if maxRows == 1 {
		return []string{clean[len(clean)-1]}, true
	}
	tailRows := maxRows - 1
	preview := []string{clean[0]}
	preview = append(preview, clean[len(clean)-tailRows:]...)
	return preview, true
}

func toolArgumentSummary(name string, args map[string]any, raw string) string {
	if args == nil && raw != "" {
		if strings.HasPrefix(raw, "$ ") {
			return raw
		}
		return truncateDisplay(raw, 64)
	}
	value := func(key string) string {
		if args == nil {
			return ""
		}
		if v, ok := args[key].(string); ok {
			return strings.TrimSpace(v)
		}
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "bash", "shell", "exec", "git":
		if command := value("command"); command != "" {
			return "$ " + command
		}
	case "read", "edit", "write", "multiedit", "delete":
		if path := firstArg(args, "file_path", "path", "file"); path != "" {
			line := path
			if start := value("start_line"); start != "" {
				line += ":" + start
				if end := value("end_line"); end != "" {
					line += "-" + end
				}
			}
			return line
		}
	case "search", "grep", "glob", "find":
		pattern := firstArg(args, "pattern", "query", "regex")
		path := firstArg(args, "path", "directory", "file_path")
		if pattern != "" && path != "" {
			return fmt.Sprintf("%q in %s", pattern, path)
		}
		if pattern != "" {
			return fmt.Sprintf("%q", pattern)
		}
	}
	if raw != "" {
		return truncateDisplay(raw, 64)
	}
	if len(args) == 0 {
		return ""
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	return truncateDisplay(string(encoded), 64)
}

func firstArg(args map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := args[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func diffSummary(files []diffFile) string {
	if len(files) == 0 {
		return ""
	}
	adds, removes := 0, 0
	for _, file := range files {
		adds += file.additions
		removes += file.deletions
	}
	name := files[0].path
	if len(files) > 1 {
		name = fmt.Sprintf("%s + %d files", name, len(files)-1)
	}
	return fmt.Sprintf("%s  +%d -%d", name, adds, removes)
}

func stripANSI(text string) string {
	return sanitizeANSI(text)
}
