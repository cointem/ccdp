package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// View renders the whole screen.
func (m Model) View() string {
	if m.width == 0 || m.height == 0 {
		return "ccdp is starting…"
	}

	header := m.renderHeader()
	body := m.viewport.View()
	status := m.renderStatus()
	footer := m.renderFooter()

	var sb strings.Builder
	sb.WriteString(header)
	sb.WriteString("\n")
	sb.WriteString(body)
	sb.WriteString("\n")
	sb.WriteString(status)
	sb.WriteString("\n")
	sb.WriteString(m.renderCmdSuggest())
	sb.WriteString(m.renderInput())
	sb.WriteString("\n")
	sb.WriteString(footer)

	out := sb.String()

	// Approval modal overlays everything.
	if m.approval != nil {
		out = lipgloss.Place(
			m.width, m.height,
			lipgloss.Center, lipgloss.Center,
			m.renderApproval(),
			lipgloss.WithWhitespaceChars(" "),
		)
	}
	return out
}

func (m *Model) renderHeader() string {
	modeColor := "240"
	switch m.mode {
	case "acceptEdits":
		modeColor = "39"
	case "bypassPermissions":
		modeColor = "196"
	}
	modeBadge := lipgloss.NewStyle().Foreground(lipgloss.Color(modeColor)).Bold(true).Render(string(m.mode))

	// Plan mode gets its own amber badge.
	if m.planMode {
		planBadge := lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true).Render("plan")
		modeBadge = modeBadge + " " + planBadge
	}

	ws := m.workspace
	if len(ws) > 24 {
		ws = "…" + ws[len(ws)-23:]
	}

	hasCost := m.usage.TurnCount > 0 || m.usage.InputTokens > 0 || m.usage.OutputTokens > 0
	parts := make([]string, 0, len(m.statusItems))
	for _, item := range m.statusItems {
		switch item {
		case "version":
			parts = append(parts, "ccdp v"+appVersion)
		case "model":
			parts = append(parts, m.modelName)
		case "mode":
			parts = append(parts, "mode:"+modeBadge)
		case "session":
			parts = append(parts, "session:"+shortID(m.sessionID))
		case "workspace":
			parts = append(parts, ws)
		case "cost":
			if hasCost {
				parts = append(parts, fmt.Sprintf("%.1fk→%.1fk · $%.3f",
					float64(m.usage.InputTokens)/1000,
					float64(m.usage.OutputTokens)/1000,
					m.usage.Cost))
			}
		case "context":
			// Percent of the context window consumed; turns amber near the
			// auto-compact threshold (Claude Code's context-left indicator).
			used, window := m.ag.ContextUsage()
			pct := 100 * used / window
			if pct > 0 {
				style := styleContextOK
				if pct >= 80 {
					style = styleContextWarn
				}
				parts = append(parts, style.Render(fmt.Sprintf("ctx:%d%%", pct)))
			}
		}
	}
	return styleHeader.Render(strings.Join(parts, "  ·  "))
}

func (m *Model) renderStatus() string {
	if m.approval != nil {
		return styleStatus.Render("awaiting your decision…")
	}
	prefix := ""
	if m.busy {
		prefix = m.spinner.View() + " "
	}
	if m.status == "" && !m.busy {
		return ""
	}
	return styleStatus.Render(prefix + m.status)
}

func (m *Model) renderInput() string {
	var ta string
	if m.busy {
		ta = styleInput.Render(m.textarea.Value()) + "\n"
	} else {
		ta = m.textarea.View()
	}
	// Wrap the textarea in a thin frame.
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder(), true, false, false, false).
		BorderForeground(lipgloss.Color("62")).
		Padding(0, 1).
		MaxWidth(m.width).
		Render(ta)
}

// renderCmdSuggest draws the slash-command autocomplete popup above the input
// while the user is typing a "/" command. The highlighted entry carries a ❯.
func (m *Model) renderCmdSuggest() string {
	if len(m.cmdSug) == 0 {
		return ""
	}
	maxLen := 0
	for _, name := range m.cmdSug {
		if l := len(name) + 1; l > maxLen {
			maxLen = l
		}
	}
	var sb strings.Builder
	for i, name := range m.cmdSug {
		line := "/" + name + strings.Repeat(" ", maxLen-len(name)-1)
		if i == m.cmdSugIdx {
			line = "❯ " + styleCmdSugSel.Render(line)
		} else {
			line = "  " + styleCmdSug.Render(line)
		}
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder(), true, false, false, false).
		BorderForeground(lipgloss.Color("62")).
		Padding(0, 1).
		Render(strings.TrimRight(sb.String(), "\n")) + "\n"
}

func (m *Model) renderFooter() string {
	hints := "pgup/pgdn scroll · ctrl+p/n history · ctrl+c interrupt · /help for commands"
	if m.busy {
		hints = "ctrl+c interrupt"
	}
	return styleHints.Render(hints)
}

func (m *Model) renderApproval() string {
	req := m.approval
	if req == nil {
		return ""
	}
	var title string
	if req.Tool == "Plan" {
		title = styleModalTitle.Render("📋  Plan ready — approve?")
	} else {
		title = styleModalTitle.Render(fmt.Sprintf("⚠  %s requires approval", req.Tool))
	}
	var body strings.Builder
	body.WriteString(title)
	body.WriteString("\n\n")
	body.WriteString(styleInput.Render(req.Command))
	body.WriteString("\n\n")
	body.WriteString(styleStatus.Render(req.Reason))
	body.WriteString("\n\n")
	body.WriteString("  [y] allow    [n] deny\n")
	body.WriteString("  [a] always   [x] never  ·  [esc] deny")
	return styleModal.Render(body.String())
}

// render draws the conversation log into the viewport. Item text is
// sanitized once per item (cached in the item) so escape sequences embedded
// in tool output or model text can never corrupt the display, while
// lipgloss's own styling sequences are applied afterwards and survive.
func (m *Model) render() {
	var sb strings.Builder
	for i := range m.items {
		sb.WriteString(renderItem(&m.items[i]))
		sb.WriteString("\n")
	}
	m.viewport.SetContent(sb.String())
}

func renderItem(it *logItem) string {
	// Strip terminal escape sequences embedded in the message text itself,
	// once per item version; the lipgloss styling around it is applied after
	// this point. Streaming appends grow it.text, so a stale cache is
	// detected by length and recomputed.
	if it.sanitizedLen != len(it.text) {
		it.sanitized = sanitizeANSI(it.text)
		it.sanitizedLen = len(it.text)
	}
	text := it.sanitized
	switch it.kind {
	case "user":
		return lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder(), false, false, false, true).
			BorderForeground(lipgloss.Color("39")).
			PaddingLeft(1).
			Render(styleUser.Render("You") + "\n" + text)

	case "assistant":
		return lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder(), false, false, false, true).
			BorderForeground(lipgloss.Color("255")).
			PaddingLeft(1).
			Render(styleAssistant.Render("Assistant") + "\n" + text)

	case "thinking":
		// Chain-of-thought is rendered muted and collapsible-looking.
		return lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Italic(true).
			Render("🤔 " + text)

	case "tool":
		style := styleToolRun
		badge := "▶ running"
		switch it.status {
		case "success":
			style = styleToolOK
			badge = "✓ done"
		case "error":
			style = styleToolErr
			badge = "✗ error"
		case "denied":
			style = styleToolErr
			badge = "✗ denied"
		}
		return style.Render(badge + "\n" + text)

	case "error":
		return styleError.Render("✗ " + text)

	case "status":
		return styleStatus.Render("· " + text)

	case "system":
		return styleSystem.Render(text)

	default:
		return text
	}
}

// renderToolText produces the display body of a tool entry.
func renderToolText(name string, args map[string]any, status, output string) string {
	var sb strings.Builder
	sb.WriteString(styleToolRun.Render(name))
	sb.WriteString("\n")
	if cmd, ok := args["command"].(string); ok {
		sb.WriteString(styleStatus.Render("$ " + cmd))
		sb.WriteString("\n")
	} else if p, ok := args["file_path"].(string); ok {
		sb.WriteString(styleStatus.Render("📄 " + p))
		sb.WriteString("\n")
	} else if len(args) > 0 {
		// Compact one-line summary for other argument shapes.
		summary, err := json.Marshal(args)
		if err == nil && len(summary) <= 120 {
			sb.WriteString(styleStatus.Render(string(summary)))
			sb.WriteString("\n")
		}
	}
	if output != "" {
		sb.WriteString(strings.TrimRight(output, "\n"))
	}
	return sb.String()
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[len(id)-8:]
}
