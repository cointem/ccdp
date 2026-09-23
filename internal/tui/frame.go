package tui

import (
	"fmt"
	"github.com/charmbracelet/lipgloss"
	"os"
	"strings"
)

// layout recomputes viewport/input sizes after a resize.
func (m *Model) layout() {
	if m.width <= 0 || m.height <= 0 {
		return
	}
	m.textarea.SetWidth(max(1, m.width-1))
	m.textarea.SetHeight(m.desiredInputHeight())
	// Keep the no-popup baseline in terms of the same wrapped presentation
	// segments View uses. A fixed one-row guess causes the transcript anchor to
	// drift when the status/footer or composer wraps on a narrow terminal.
	m.baseVpH = max(1, m.height-m.fixedPresentationHeight())
	m.syncViewportHeight()
	m.viewport.Width = m.width
	m.render()
	if m.followOutput {
		m.viewport.GotoBottom()
	}
}

// fixedPresentationHeight measures the chrome that is present even when no
// popup is open. Optional decision, selector, and command-suggestion surfaces
// are measured separately so baseVpH remains a stable no-popup anchor.
func (m *Model) fixedPresentationHeight() int {
	if m == nil {
		return 0
	}
	width := m.width
	if width <= 0 {
		width = m.viewport.Width
	}
	width = max(1, width)
	return m.composeChrome().fixedHeight(width)
}

// headerPresentation is shared by View and the viewport row budget.
// A single source lets state changes (active child count,
// target selection, or a narrow route label) update the viewport immediately,
// without waiting for a terminal resize.
func (m *Model) headerPresentation() string {
	if m == nil {
		return ""
	}
	if m.inlineMode && !m.childDetailScreen() {
		if summary := m.pendingDecisionSummary(); summary != "" {
			return styleRunning.Render(summary)
		}
		return ""
	}
	header := m.renderHeader()
	if header != "" {
		return header
	}
	// Compact header for the managed terminal frame:
	// ccdp │ ● main                                    ~/project/ccdp
	// Or when viewing subagent:
	// ccdp │ ◐ sec-auditor · running · 1/2             [Esc] Return to main
	brand := styleBrand.Bold(true).Render("ccdp")
	div := styleDivider.Render(" │ ")

	var leftCrumb string
	var right string

	isSubagent := m.routing != nil && m.sessionID != m.routing.rootID
	if isSubagent {
		dot := styleAgentRun.Render("◐")
		name := m.sessionID
		status := "running"
		for _, row := range m.routing.rows {
			if string(row.SessionID) == m.sessionID {
				if t := strings.TrimSpace(row.Title); t != "" {
					name = t
				}
				if s := strings.TrimSpace(row.Run.Status); s != "" {
					status = s
				}
				break
			}
		}
		crumb := lipgloss.NewStyle().Bold(true).Foreground(colorText).Render(name)
		if status != "" {
			crumb += styleHints.Render(" · " + status)
		}
		if idx, total := m.agentOrdinal(); idx > 0 && total > 1 {
			crumb += styleHints.Render(fmt.Sprintf(" · %d/%d", idx, total))
		}
		leftCrumb = dot + " " + crumb
		right = styleBrand.Render("[Esc] Return to main")
	} else {
		dot := styleToolOK.Render("●")
		crumb := lipgloss.NewStyle().Bold(true).Foreground(colorText).Render("main")
		var active, approvals int
		if m.routing != nil {
			for _, row := range m.routing.rows {
				if row.Run.Active() {
					active++
				}
				if row.Approval != nil {
					approvals++
				}
			}
		}
		if active > 0 {
			crumb += " " + styleHints.Render(fmt.Sprintf("(%d active · Ctrl+A)", active))
		}
		if approvals > 0 {
			crumb += " " + styleRunning.Render(fmt.Sprintf("(%d awaiting decision)", approvals))
		}
		leftCrumb = dot + " " + crumb
		ws := shortenPath(m.workspace)
		if ws != "" {
			right = styleHints.Render(truncateDisplay(ws, 36))
		}
	}

	left := brand + div + leftCrumb
	leftWidth := lipgloss.Width(stripANSI(left))
	rightWidth := lipgloss.Width(stripANSI(right))

	if m.width <= 0 {
		return left
	}
	if leftWidth+rightWidth+1 <= m.width {
		gap := m.width - leftWidth - rightWidth
		return left + strings.Repeat(" ", gap) + right
	}
	if leftWidth <= m.width {
		return left
	}
	return truncateDisplay(left, m.width)
}

func shortenPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err == nil && home != "" && strings.HasPrefix(path, home) {
		return "~" + strings.TrimPrefix(path, home)
	}
	return path
}

func (m *Model) optionalPresentationHeight() int {
	if m == nil {
		return 0
	}
	width := m.width
	if width <= 0 {
		width = m.viewport.Width
	}
	return presentationHeight(m.composeChrome().surface, max(1, width))
}

// syncViewportHeight reserves exactly the rows occupied by the currently
// rendered decision/selector/suggestion surfaces. The measurements use the
// presentation helpers, so wrapped descriptions and multi-line hints consume
// their real terminal rows instead of a guessed popup constant.
func (m *Model) syncViewportHeight() {
	if m.height > 0 {
		// Status, routing, and footer rows can change without a WindowSizeMsg.
		// Refresh the baseline on every sync so a child target or a wrapped
		// notice never steals the first transcript row.
		m.baseVpH = max(1, m.height-m.fixedPresentationHeight())
	}
	h := m.baseVpH
	if h == 0 {
		if m.height > 0 {
			h = max(1, m.height-m.fixedPresentationHeight())
		} else {
			h = 20 // sane default before the first WindowSizeMsg
		}
	}
	if m.height <= 0 {
		// Before the first WindowSizeMsg the renderer has no terminal width or
		// height to measure against. Keep the constructor/test fallback usable;
		// real frames take the measured path below as soon as the window size is
		// known.
		if n := len(m.cmdSug); n > 0 {
			n = min(n, m.cmdSuggestionPageSize())
			if len(m.cmdSug) > n {
				n++
			}
			h -= n + 3
		}
		if m.picker != nil && m.picker.inline {
			h -= m.pickerVisibleRows() + 3
		}
	} else {
		if !m.overlaySurface() {
			h -= m.optionalPresentationHeight()
		}
	}
	if h < 1 {
		h = 1
	}
	m.viewport.Height = h
}
