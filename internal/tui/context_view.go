package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

func (m *Model) renderContextDetailPopover() string {
	if m == nil {
		return ""
	}
	width := min(60, max(8, m.width-4))
	innerWidth := width - 4

	window := 0
	used := 0
	if m.hasSnapshot {
		window = m.snapshot.Settings.ContextWindow
		used = m.snapshot.ContextUsedTokens
	}
	if used == 0 && m.routing != nil && m.sessionID != m.routing.rootID {
		for _, row := range m.routing.rows {
			if string(row.SessionID) == m.sessionID {
				tokens := row.Run.Usage.InputTokens + row.Run.Usage.OutputTokens
				if tokens > 0 {
					used = tokens
				}
				break
			}
		}
	}
	if window == 0 && m.routing != nil {
		if root, ok := m.routing.views[m.routing.rootID]; ok && root.snapshot.Settings.ContextWindow > 0 {
			window = root.snapshot.Settings.ContextWindow
		}
	}
	if used < 0 {
		used = 0
	}
	pct := 0
	if window > 0 {
		pct = int(float64(used) * 100.0 / float64(window))
	}

	titleText := "Context Window"
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
		titleText = fmt.Sprintf("Context (%s)", truncateDisplay(agentName, 18))
	}

	titleLeft := styleModalTitle.Render(titleText)
	titleRight := styleRunning.Render(fmt.Sprintf("%d%%", pct))
	titleGap := max(1, innerWidth-lipgloss.Width(stripANSI(titleLeft))-lipgloss.Width(stripANSI(titleRight)))
	headerRow := titleLeft + strings.Repeat(" ", titleGap) + titleRight

	tokensText := ""
	if window > 0 {
		tokensText = fmt.Sprintf("%s / %s tokens", formatNumber(used), formatNumber(window))
	} else {
		tokensText = fmt.Sprintf("%s tokens (no limit)", formatNumber(used))
	}
	tokensRow := styleStatus.Render(tokensText)

	toolsCount := 0
	msgCount := 0
	sysTokens := 0
	toolsTokens := 0
	msgTokens := 0
	if m.hasSnapshot {
		toolsCount = len(m.snapshot.Catalog.Tools)
		msgCount = len(m.snapshot.History)
		// The runtime splits used tokens into messages / tools / system
		// (system = the whole system message, incl. repo layout and project
		// instructions). The three always sum to ContextUsedTokens.
		sysTokens = m.snapshot.SystemTokens
		toolsTokens = m.snapshot.ToolsTokens
		msgTokens = m.snapshot.MessagesTokens
	}
	// Degenerate fallback when the split is absent: attribute everything to the
	// system prompt so the bar still fills to the reported total.
	if used > 0 && sysTokens+toolsTokens+msgTokens == 0 {
		sysTokens = used
	}

	sysPct := 0
	toolsPct := 0
	msgPct := 0
	if window > 0 && used > 0 {
		sysPct = int(float64(sysTokens) * 100.0 / float64(window))
		toolsPct = int(float64(toolsTokens) * 100.0 / float64(window))
		msgPct = int(float64(msgTokens) * 100.0 / float64(window))
	}

	barWidth := innerWidth
	var progressBar string
	if window > 0 && barWidth > 0 && used > 0 {
		sysW := int(float64(sysTokens) * float64(barWidth) / float64(window))
		toolsW := int(float64(toolsTokens) * float64(barWidth) / float64(window))
		msgW := int(float64(msgTokens) * float64(barWidth) / float64(window))
		if sysTokens > 0 && sysW == 0 {
			sysW = 1
		}
		if toolsTokens > 0 && toolsW == 0 {
			toolsW = 1
		}
		if msgTokens > 0 && msgW == 0 {
			msgW = 1
		}
		totalW := sysW + toolsW + msgW
		if totalW > barWidth {
			msgW = max(0, barWidth-sysW-toolsW)
			totalW = sysW + toolsW + msgW
		}
		emptyW := max(0, barWidth-totalW)
		progressBar = styleToolOK.Render(strings.Repeat("█", sysW)) +
			styleBrand.Render(strings.Repeat("█", toolsW)) +
			styleRunning.Render(strings.Repeat("█", msgW)) +
			styleDivider.Render(strings.Repeat("░", emptyW))
	} else {
		progressBar = styleDivider.Render(strings.Repeat("░", barWidth))
	}

	renderLine := func(bulletStyle lipgloss.Style, label string, valPct int, hasContent bool) string {
		bullet := bulletStyle.Render("• ")
		left := bullet + styleToolOutput.Render(label)
		right := ""
		if valPct <= 0 {
			if hasContent && used > 0 {
				right = styleHints.Render("<1%")
			} else {
				right = styleHints.Render("0%")
			}
		} else {
			right = styleStatus.Render(fmt.Sprintf("%2d%%", valPct))
		}
		gap := max(1, innerWidth-lipgloss.Width(stripANSI(left))-lipgloss.Width(stripANSI(right)))
		return left + strings.Repeat(" ", gap) + right
	}

	sysLine := renderLine(styleToolOK, "System Prompt", sysPct, used > 0)
	toolsLine := renderLine(styleBrand, "System Tools", toolsPct, toolsCount > 0 && used > 0)
	msgLine := renderLine(styleRunning, "Messages", msgPct, msgCount > 0 && used > 0)

	hintRow := styleHints.Render("Esc or click to close")

	body := strings.Join([]string{
		headerRow,
		tokensRow,
		progressBar,
		"",
		sysLine,
		toolsLine,
		msgLine,
		"",
		styleStatus.Render(cacheDetail(m.sessionCache())),
		styleStatus.Render("Current session only; excludes subagents"),
		"",
		hintRow,
	}, "\n")

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colorBorder).
		Padding(0, 1).
		Width(width).
		MaxWidth(max(1, m.width)).
		Render(body)
}

func formatNumber(n int) string {
	if n < 0 {
		return "0"
	}
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var res []byte
	rem := len(s) % 3
	if rem > 0 {
		res = append(res, s[:rem]...)
	}
	for i := rem; i < len(s); i += 3 {
		if len(res) > 0 {
			res = append(res, ',')
		}
		res = append(res, s[i:i+3]...)
	}
	return string(res)
}
