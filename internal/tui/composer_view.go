package tui

import (
	"fmt"
	"github.com/charmbracelet/lipgloss"
	"regexp"
	"strings"
)

func (m *Model) renderInput() string {
	if !m.composerVisible() {
		return ""
	}
	placeholder := ""
	if m.routing != nil && m.sessionID != m.routing.rootID {
		agentName := m.sessionID
		for _, row := range m.routing.rows {
			if string(row.SessionID) == m.sessionID {
				if t := strings.TrimSpace(row.Title); t != "" {
					agentName = t
				}
				break
			}
		}
		placeholder = fmt.Sprintf("Message %s directly... (Esc to return to main)", truncateDisplay(agentName, 20))
	} else {
		placeholder = composerPlaceholder
	}

	return m.composerState.render(m.width, placeholder)
}

func (m *composerState) render(width int, placeholder string) string {
	m.textarea.Placeholder = placeholder

	// A full-width background separates the transcript from the composer so the
	// input stays easy to find as output streams. The background is neutral: brand
	// colour is reserved for the cursor and header, which keeps the chrome calm
	// while a turn is running.
	composer := m.textarea.View()
	if m.pasteFold != nil && !m.pasteFold.Expanded {
		// The full literal remains in textarea.Value for submit/copy, but a
		// collapsed paste should not occupy five ordinary composer rows. Ctrl+E
		// switches back to the editable textarea without touching that source.
		composer = styleStatus.Render(truncateDisplay("  "+m.pasteFold.summary(), max(1, width-4)))
	}
	return paintComposerBackground(lipgloss.NewStyle().Background(colorInputBackground).
		Padding(1, 0).
		Width(max(1, width)).
		MaxWidth(width).
		Render(m.imageInputSummary(width) + composer))
}

// A nested viewport pads its rows after resetting SGR. An enclosing Lip Gloss
// background does not repaint those existing spaces. Restore the composer's
// background after each SGR sequence, preserving foreground/bold/reverse cursor
// attributes, and reset at the boundary so the footer is unaffected.
var composerSGR = regexp.MustCompile(`\x1b\[[0-9;:]*m`)

func paintComposerBackground(view string) string {
	sample := lipgloss.NewStyle().Background(colorInputBackground).Render(" ")
	background := strings.SplitN(sample, " ", 2)[0]
	if background == "" {
		return view
	} // NO_COLOR / ASCII output
	return background + composerSGR.ReplaceAllStringFunc(view, func(s string) string {
		return s + background
	}) + "\x1b[0m"
}

// renderCmdSuggest draws the slash-command autocomplete popup above the input
// while the user is typing a "/" command. The highlighted entry carries a ❯.
func (m *Model) renderCmdSuggest() string {
	if len(m.cmdSug) == 0 {
		return ""
	}
	start, end := m.cmdSuggestionWindow()
	visible := m.cmdSug[start:end]
	available := 72
	if m.width > 0 {
		available = max(4, m.width-6)
	}
	maxName := 0
	for _, name := range visible {
		if l := lipgloss.Width("/" + name); l > maxName {
			maxName = l
		}
	}
	maxName = min(maxName, max(1, available-4))
	var sb strings.Builder
	for i, name := range visible {
		absolute := start + i
		label := truncateDisplay("/"+name, maxName)
		label += strings.Repeat(" ", max(0, maxName-lipgloss.Width(label)))
		description := ""
		if entry, ok := commandCatalog.Lookup(name); ok {
			description = entry.Help
		} else if custom := m.findCustomCommand(name); custom != nil {
			description = "custom command · " + custom.source + " scope"
		}
		remaining := max(1, available-2-maxName-2)
		if description != "" {
			description = truncateDisplay(description, remaining)
		}
		line := label
		if description != "" {
			line += "  " + styleStatus.Render(description)
		}
		if absolute == m.cmdSugIdx {
			line = "❯ " + styleCmdSugSel.Render(line)
		} else {
			line = "  " + styleCmdSug.Render(line)
		}
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	if len(m.cmdSug) > len(visible) {
		sb.WriteString("  " + styleCmdSug.Render(fmt.Sprintf("↑/↓ %d–%d of %d  ·  fn+↑/↓ jump",
			start+1, end, len(m.cmdSug))))
		sb.WriteString("\n")
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder(), true, false, false, false).
		BorderForeground(colorBorder).
		Padding(0, 1).
		Render(strings.TrimRight(sb.String(), "\n")) + "\n"
}

func (m *Model) cmdSuggestionWindow() (start, end int) {
	pageSize := m.cmdSuggestionPageSize()
	end = min(len(m.cmdSug), pageSize)
	if m.cmdSugIdx >= end {
		end = min(len(m.cmdSug), m.cmdSugIdx+1)
		start = end - pageSize
	}
	return max(0, start), end
}
