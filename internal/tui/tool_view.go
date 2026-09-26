package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"ccdp/internal/protocol"
)

// toolPresentation is a typed projection of a log item. Its Output field is
// the complete sanitized output; the renderer chooses a bounded preview while
// readers and copy/export paths can still use the original historyCell.text.
type toolPresentation struct {
	ID        string
	Name      string
	Status    string
	Args      map[string]any
	Arguments string
	Output    string
	Truncated bool
	// Agent holds the child sessions spawned by a Task/Agent call, resolved by
	// ParentCallID. It drives the compact agent marker so the child's final
	// answer never floods the parent transcript as ordinary tool output.
	Agent                []protocol.ChildSession
	ContinuesExploration bool
	Expanded             bool
}

func toolPresentationFromItem(item *historyCell) toolPresentation {
	args := make(map[string]any, len(item.toolArgs))
	for key, value := range item.toolArgs {
		if text, ok := value.(string); ok {
			args[key] = sanitizeANSI(text)
		} else {
			args[key] = value
		}
	}
	return toolPresentation{ID: sanitizeANSI(item.toolID), Name: sanitizeANSI(item.toolName),
		Status: sanitizeANSI(item.status), Args: args, Arguments: sanitizeANSI(item.toolArgsRaw),
		Output: item.cleanText(), Truncated: item.toolTruncated, Agent: item.agent}
}

// renderToolView is shared by the transcript and typed protocol projection.
// The output is intentionally non-Markdown: shell and tool output frequently
// contains punctuation that should remain literal terminal text.
func renderToolView(item *historyCell, width int) string {
	return renderToolPresentation(toolPresentationFromItem(item), width)
}

func (m *Model) renderToolView(item *historyCell, width int) string {
	p := toolPresentationFromItem(item)
	if m != nil {
		for _, key := range []string{"file_path", "path", "directory"} {
			if path, ok := p.Args[key].(string); ok {
				p.Args[key] = displayToolPath(path, m.workspace)
			}
		}
		if explorationComplete(p) {
			for i := range m.items {
				if &m.items[i] == item || (p.ID != "" && m.items[i].toolID == p.ID) {
					if i > 0 && m.items[i-1].kind == "tool" {
						p.ContinuesExploration = explorationComplete(toolPresentationFromItem(&m.items[i-1]))
					}
					break
				}
			}
		}
	}
	view := renderToolPresentation(p, width)
	if m.animateWork() && !toolStatusComplete(p.Status) && !toolStatusIsFailure(p.Status) && !isAgentTool(p.Name) {
		view = strings.Replace(view, styleToolRun.Render("●"), m.spinner.View(), 1)
	}
	return view
}

func renderToolPresentation(p toolPresentation, width int) string {
	width = max(1, width)
	// Task/Agent rows are delegations, not ordinary tools. They render a compact
	// status marker (with a bounded preview of the child's answer) instead of
	// streaming the child's full output into the parent transcript.
	if isAgentTool(p.Name) {
		return renderAgentPresentation(p, width)
	}
	if explorationComplete(p) && !p.Expanded {
		return renderExplorationCell(p, width)
	}
	name := p.Name
	if name == "" {
		name = "Tool"
	}
	meta := toolArgumentSummary(p.Name, p.Args, p.Arguments)
	switch strings.ToLower(p.Name) {
	case "bash":
		name = "Ran"
		if !toolStatusComplete(p.Status) && !toolStatusIsFailure(p.Status) {
			name = "Running"
		}
		meta = strings.TrimPrefix(meta, "$ ")
	case "write":
		name = "Wrote"
	case "edit":
		name = "Edited"
	}
	status := strings.ToLower(strings.TrimSpace(p.Status))
	mark, stateStyle, stateText := "●", styleToolRun, "running"
	switch status {
	case "success", "done", "completed", "complete":
		mark, stateStyle, stateText = "•", styleToolOK, "done"
	case "error", "failed", "failure":
		mark, stateStyle, stateText = "✗", styleToolErr, "error"
	case "denied", "rejected":
		mark, stateStyle, stateText = "✗", styleToolErr, "denied"
	case "cancelled", "canceled", "stopped":
		mark, stateStyle, stateText = "■", styleStatus, "stopped"
	}
	first := stateStyle.Render(mark) + " " + styleAssistant.Bold(true).Render(name)
	if stateText != "done" && stateText != "" && !(name == "Running" && stateText == "running") {
		first += stateStyle.Render("  " + stateText)
	}
	if meta != "" {
		first += " " + styleStatus.Render(truncateDisplay(meta, max(1, width-lipgloss.Width(stripANSI(first))-1)))
	}
	if p.Expanded {
		rows := []string{first + "  " + styleHints.Render("[click to collapse]")}
		if p.Arguments != "" {
			rows = append(rows, styleDivider.Render("  ├─ Arguments:"))
			for _, argLine := range wrapDisplay(p.Arguments, max(1, width-8)) {
				rows = append(rows, "     "+styleToolOutput.Render(argLine))
			}
		}
		if output := strings.TrimRight(p.Output, "\n"); output != "" {
			rows = append(rows, styleDivider.Render("  └─ Output:"))
			outLines := strings.Split(output, "\n")
			maxLines := min(20, len(outLines))
			for _, outLine := range outLines[:maxLines] {
				rows = append(rows, "     "+styleToolOutput.Render(truncateDisplay(outLine, max(1, width-8))))
			}
			if len(outLines) > maxLines {
				rows = append(rows, styleHints.Render(fmt.Sprintf("     … (%d more lines)", len(outLines)-maxLines)))
			}
		}
		return strings.Join(rows, "\n")
	}
	if preview := renderFileChangePreview(p, width); preview != "" {
		return first + "\n" + preview
	}
	if strings.EqualFold(p.Name, "Bash") && (p.Output != "" || toolStatusComplete(p.Status) || toolStatusIsFailure(status)) {
		return first + "\n" + strings.Join(commandOutputPreview(p.Output, 4, width, p.Truncated), "\n")
	}

	rows := []string{first}
	output := strings.TrimRight(p.Output, "\n")
	if output == "" {
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
		preview := strings.Split(renderDiff(output, width), "\n")
		rows = append(rows, preview[:min(10, len(preview))]...)
		if len(preview) > 10 {
			rows = append(rows, styleHints.Render(fmt.Sprintf("    … +%d lines · /transcript", len(preview)-10)))
		}
		return strings.Join(rows, "\n")
	}
	preview, _ := boundedToolLines(outputLines, maxOutput, status == "running")
	if status == "error" || status == "failed" || status == "failure" {
		preview, _ = boundedToolErrorLines(outputLines, maxOutput)
	}
	for _, line := range preview {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		rows = append(rows, styleDivider.Render("  ⎿ ")+styleToolOutput.Render(truncateDisplay(line, max(1, width-6))))
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
	case "ls", "listdir", "list", "dir", "filetree":
		if path := firstArg(args, "path", "dir", "directory"); path != "" {
			return path
		}
		return "."
	case "gitstatus":
		summary := "status"
		if s, ok := args["short"].(bool); ok && s {
			summary += " (short)"
		}
		return summary
	case "gitdiff":
		path := firstArg(args, "path", "file")
		if path != "" {
			return "diff " + path
		}
		if staged, ok := args["staged"].(bool); ok && staged {
			return "diff (staged)"
		}
		return "diff"
	case "gitlog":
		return "log"
	case "webfetch", "fetch", "curl", "http":
		if url := firstArg(args, "url", "uri", "endpoint"); url != "" {
			return url
		}
	case "websearch":
		if q := firstArg(args, "query", "q"); q != "" {
			return fmt.Sprintf("%q", q)
		}
	case "todo", "task", "todowrite", "todoread", "todo_write", "todo_read":
		if action := firstArg(args, "action", "task"); action != "" {
			return action
		}
		if todosRaw, ok := args["todos"]; ok {
			if todoList, ok := todosRaw.([]any); ok {
				if len(todoList) > 0 {
					if first, ok := todoList[0].(map[string]any); ok {
						if c, ok := first["content"].(string); ok {
							return fmt.Sprintf("update (%d tasks: %s)", len(todoList), truncateDisplay(c, 24))
						}
					}
					return fmt.Sprintf("update (%d tasks)", len(todoList))
				}
				return "clear tasks"
			}
		}
		return "tasks"
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
		if strings.HasPrefix(raw, "{") || strings.HasPrefix(raw, "[") {
			var parsed map[string]any
			if json.Unmarshal([]byte(raw), &parsed) == nil {
				if todosRaw, ok := parsed["todos"]; ok {
					if todoList, ok := todosRaw.([]any); ok {
						if len(todoList) > 0 {
							if first, ok := todoList[0].(map[string]any); ok {
								if c, ok := first["content"].(string); ok {
									return fmt.Sprintf("update (%d tasks: %s)", len(todoList), truncateDisplay(c, 24))
								}
							}
							return fmt.Sprintf("update (%d tasks)", len(todoList))
						}
					}
				}
				if c := firstArg(parsed, "path", "file_path", "command", "query", "pattern", "url", "name"); c != "" {
					return truncateDisplay(c, 64)
				}
			}
		}
		return truncateDisplay(raw, 64)
	}
	// Fallback: look for common descriptive keys before falling back to raw JSON
	if common := firstArg(args, "path", "url", "command", "query", "pattern", "name", "target", "file"); common != "" {
		return truncateDisplay(common, 64)
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

// isReadOnlyTool reports whether a tool only inspects files or state. A
// successful call produces no output the user needs inline, so it collapses to
// a single summary line.
func isReadOnlyTool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "read", "view", "cat", "glob", "grep", "search", "find", "ls", "list", "listdir":
		return true
	}
	return false
}

// toolStatusIsFailure reports whether a normalized status still needs its output
// rendered inline. Failures and denials keep their diagnostic preview.
func toolStatusIsFailure(status string) bool {
	switch status {
	case "error", "failed", "failure", "denied", "rejected":
		return true
	}
	return false
}

func firstArg(args map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := args[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func stripANSI(text string) string {
	return sanitizeANSI(text)
}

// agentPreviewLimit bounds the inline preview of a child's final answer. The
// full text stays available through /transcript; the parent transcript only
// carries enough to recognize the result (aligned with codex's 240-cell mark).
const agentPreviewLimit = 240

// renderAgentPresentation renders a Task/Agent row as a compact status marker.
// A single delegation becomes one marker line (plus a bounded answer preview on
// completion); a batch fan-out becomes a header with one line per child.
func renderAgentPresentation(p toolPresentation, width int) string {
	notify := isNotifyPolicy(p.Args)
	if len(p.Agent) > 1 {
		return renderAgentGroup(p, p.Agent, notify, width)
	}
	var child *protocol.ChildSession
	if len(p.Agent) == 1 {
		child = &p.Agent[0]
	}
	return renderAgentSingle(p, child, notify, width)
}

// agentMarkerStatus maps a tool or child-run status onto the marker glyph, its
// color and a short label. A live child is cyan, completion green, failure red.
func agentMarkerStatus(status string) (string, lipgloss.Style, string) {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "success", "done", "completed", "complete", "succeeded":
		return "✓", styleToolOK, "Done"
	case "error", "failed", "failure", "denied", "rejected":
		return "✗", styleToolErr, "error"
	case "cancelled", "canceled", "stopped":
		return "■", styleStatus, "stopped"
	case "waiting_approval":
		return "◆", styleAgentRun, "needs approval"
	default:
		return "◆", styleAgentRun, "running"
	}
}

// agentUsageSummary compacts a child run's activity into
// "12 tool uses · 45.2k tokens · 1m 03s". Each part is shown only when known, so
// a freshly started child may show just its elapsed time.
func agentUsageSummary(run protocol.RunView) string {
	var parts []string
	if run.ToolUses > 0 {
		label := "tool uses"
		if run.ToolUses == 1 {
			label = "tool use"
		}
		parts = append(parts, fmt.Sprintf("%d %s", run.ToolUses, label))
	}
	if tokens := run.Usage.InputTokens + run.Usage.OutputTokens; tokens > 0 {
		parts = append(parts, formatContextTokens(tokens)+" tokens")
	}
	if !run.StartedAt.IsZero() {
		end := run.FinishedAt
		if end.IsZero() {
			end = time.Now()
		}
		if d := end.Sub(run.StartedAt); d > 0 {
			parts = append(parts, formatElapsed(d))
		}
	}
	return strings.Join(parts, " · ")
}

// agentChildStats is the usage tail for one child. Read-only guardian agents are
// one-shot inspections, so they skip the token/duration note (aligned with
// claude's ONE_SHOT_BUILTIN_AGENT_TYPES).
func agentChildStats(c protocol.ChildSession) string {
	if strings.EqualFold(strings.TrimSpace(c.Purpose), "guardian") {
		return ""
	}
	return agentUsageSummary(c.Run)
}

// agentDescription is the short brief shown after the tool name: the child's
// title for a delegation, the action for an Agent control call, or the batch
// size before any child has registered.
func agentDescription(p toolPresentation, child *protocol.ChildSession) string {
	if child != nil {
		if title := strings.TrimSpace(child.Title); title != "" {
			return title
		}
	}
	if strings.EqualFold(strings.TrimSpace(p.Name), "agent") {
		action := firstArg(p.Args, "action")
		if action != "" {
			if sid := firstArg(p.Args, "session_id"); sid != "" {
				return action + " " + shortID(sid)
			}
			return action
		}
	}
	if desc := firstArg(p.Args, "description"); desc != "" {
		return desc
	}
	if agents, ok := p.Args["agents"].([]any); ok && len(agents) > 0 {
		return fmt.Sprintf("%d agents", len(agents))
	}
	if p.Arguments != "" {
		return truncateDisplay(p.Arguments, 64)
	}
	return ""
}

// agentPreview collapses a child answer to a single bounded line.
func agentPreview(text string) string {
	return truncateDisplay(sanitizeANSI(text), agentPreviewLimit)
}

func isNotifyPolicy(args map[string]any) bool {
	return strings.EqualFold(strings.TrimSpace(firstArg(args, "wait_policy")), "notify")
}

// aggregateChildStatus folds a batch into one label: any live child keeps the
// group running, otherwise a failure dominates, else the group is done.
func aggregateChildStatus(children []protocol.ChildSession) string {
	failed := false
	for _, c := range children {
		switch strings.ToLower(strings.TrimSpace(c.Run.Status)) {
		case "success", "done", "completed", "complete", "succeeded":
			continue
		case "error", "failed", "failure", "denied", "rejected", "cancelled", "canceled", "stopped":
			failed = true
		default:
			return "running"
		}
	}
	if failed {
		return "failed"
	}
	return "succeeded"
}

func renderAgentSingle(p toolPresentation, child *protocol.ChildSession, notify bool, width int) string {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		name = "Task"
	}
	// For join the parent tool result is the freshest completion signal, so it
	// drives the glyph. For notify the parent returns immediately as "accepted"
	// while the child keeps running, so the child's own state is authoritative.
	status := p.Status
	if notify && child != nil {
		status = child.Run.Status
	}
	glyph, stateStyle, label := agentMarkerStatus(status)

	head := stateStyle.Render(glyph) + " " + styleAssistant.Bold(true).Render(name)
	var tail string
	stats := ""
	if child != nil {
		stats = agentChildStats(*child)
	}
	if label == "Done" && stats != "" {
		tail = stateStyle.Render(" · Done (" + stats + ")")
	} else {
		tail = stateStyle.Render(" · " + label)
		if stats != "" {
			tail += styleStatus.Render(" · " + stats)
		}
	}
	if desc := agentDescription(p, child); desc != "" {
		budget := width - lipgloss.Width(stripANSI(head)) - lipgloss.Width(stripANSI(tail)) - 2
		if budget > 0 {
			head += "  " + truncateDisplay(desc, budget)
		}
	}
	rows := []string{head + tail}

	switch label {
	case "error", "stopped":
		detail := p.Output
		if child != nil && strings.TrimSpace(child.Run.Error) != "" {
			detail = child.Run.Error
		}
		if msg := agentPreview(detail); msg != "" {
			rows = append(rows, styleDivider.Render("  ⎿ ")+styleToolErr.Render(truncateDisplay(msg, max(1, width-6))))
		}
	case "Done":
		// A notify parent's own output is just the "accepted" receipt; the real
		// answer arrives later, so only a joined delegation previews it here.
		if !notify {
			out := p.Output
			if strings.TrimSpace(out) == "" && child != nil {
				out = child.Run.Output
			}
			if preview := agentPreview(out); preview != "" {
				rows = append(rows, styleDivider.Render("  ⎿ ")+styleToolOutput.Render(preview)+
					styleStatus.Render("  · /transcript full · /agents switch"))
			}
		}
	}
	return strings.Join(rows, "\n")
}

func renderAgentGroup(p toolPresentation, children []protocol.ChildSession, notify bool, width int) string {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		name = "Task"
	}
	overall := p.Status
	if notify {
		overall = aggregateChildStatus(children)
	}
	glyph, stateStyle, label := agentMarkerStatus(overall)
	count := styleStatus.Render(fmt.Sprintf("  %d agents", len(children)))
	head := stateStyle.Render(glyph) + " " + styleAssistant.Bold(true).Render(name) + count
	if label == "Done" {
		head += styleStatus.Render(" finished")
		if stats := agentGroupUsageSummary(children); stats != "" {
			head += stateStyle.Render(" (" + stats + ")")
		}
	} else {
		head += stateStyle.Render(" · " + label)
	}
	rows := []string{head}
	for i, c := range children {
		rows = append(rows, renderAgentChildLine(c, i == len(children)-1, width))
	}
	return strings.Join(rows, "\n")
}

// agentGroupUsageSummary folds a batch into total tokens and the parallel wall
// duration (earliest start to latest finish, or to now while one is active).
func agentGroupUsageSummary(children []protocol.ChildSession) string {
	tokens := 0
	var earliest, latest time.Time
	for _, c := range children {
		tokens += c.Run.Usage.InputTokens + c.Run.Usage.OutputTokens
		if !c.Run.StartedAt.IsZero() && (earliest.IsZero() || c.Run.StartedAt.Before(earliest)) {
			earliest = c.Run.StartedAt
		}
		end := c.Run.FinishedAt
		if end.IsZero() {
			end = time.Now()
		}
		if end.After(latest) {
			latest = end
		}
	}
	var parts []string
	if tokens > 0 {
		parts = append(parts, formatContextTokens(tokens)+" tokens")
	}
	if !earliest.IsZero() && latest.After(earliest) {
		parts = append(parts, formatElapsed(latest.Sub(earliest)))
	}
	return strings.Join(parts, " · ")
}

func renderAgentChildLine(c protocol.ChildSession, last bool, width int) string {
	_, stateStyle, label := agentMarkerStatus(c.Run.Status)
	branch := "├─"
	if last {
		branch = "└─"
	}
	line := "  " + styleDivider.Render(branch) + " "
	if title := strings.TrimSpace(c.Title); title != "" {
		line += truncateDisplay(title, max(1, width-8)) + " "
	}
	stats := agentChildStats(c)
	if label == "Done" && stats != "" {
		line += stateStyle.Render("· Done (" + stats + ")")
	} else {
		line += stateStyle.Render("· " + label)
		if stats != "" {
			line += styleStatus.Render(" · " + stats)
		}
	}
	return line
}
