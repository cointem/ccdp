package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/rivo/uniseg"
)

// View renders the whole screen.
func (m Model) View() string {
	if m.width == 0 || m.height == 0 {
		return "ccdp is starting…"
	}
	if m.reader != nil {
		return m.renderReaderView()
	}

	// Keep the managed frame at terminal height. Completed cells are still
	// delivered once to native scrollback; the cached viewport supplies their
	// visible tail so completing a turn cannot collapse the composer upward.
	body := m.viewport.View()
	if m.followOutput {
		lines := strings.Split(body, "\n")
		for len(lines) > 0 && strings.TrimSpace(sanitizeANSI(lines[len(lines)-1])) == "" {
			lines = lines[:len(lines)-1]
		}
		body = strings.Join(lines, "\n")
	}
	chrome := m.composeChrome()
	overlay := ""
	if m.overlaySurface() {
		overlay, chrome.surface = chrome.surface, ""
	}
	frame := fitPresentation(chrome.segments(body), m.width, m.height)
	if overlay != "" {
		rows := strings.Split(frame, "\n")
		popupWidth := m.width
		if m.activePanel() == panelContext {
			popupWidth = min(m.width, lipgloss.Width(overlay))
		}
		popup := presentationRows(overlay, popupWidth)
		bottom := max(0, len(rows)-presentationHeight(chrome.input, m.width)-presentationHeight(chrome.footer, m.width))
		// Focus-owning dialogs may cover the composer; its underlying position
		// stays intact and is restored when the dialog closes.
		if !m.composerVisible() {
			bottom = max(0, len(rows)-presentationHeight(chrome.footer, m.width))
			for i := bottom; i < len(rows); i++ {
				rows[i] = strings.ReplaceAll(rows[i], "enter send", "          ")
				rows[i] = strings.ReplaceAll(rows[i], "enter steer", "           ")
			}
		}
		// Retain the top of the menu on tiny screens; command selection itself
		// already scrolls to keep the focused option in its visible window.
		popup = popup[:min(len(popup), bottom)]
		start := bottom - len(popup)
		if m.activePanel() == panelDetail {
			start = min(max(0, m.detailRow), bottom)
			popup = popup[:min(len(popup), bottom-start)]
		}
		if m.activePanel() == panelContext {
			x := 0
			for _, hit := range m.footerHitCells() {
				if hit.kind == "context" {
					x = hit.x
					break
				}
			}
			x = min(x, max(0, m.width-popupWidth))
			for i, line := range popup {
				base := rows[start+i]
				// Blank/short transcript rows have no cells to slice up to x.
				// Materialize the missing columns before drawing the anchored card.
				prefix := ansi.Cut(base, 0, x)
				prefix += strings.Repeat(" ", max(0, x-lipgloss.Width(prefix)))
				rows[start+i] = prefix + line + ansi.Cut(base, x+popupWidth, m.width)
			}
		} else {
			copy(rows[start:start+len(popup)], popup)
		}
		frame = strings.Join(rows, "\n")
	}
	return m.applySelectionOverlay(frame)
}

// applySelectionOverlay draws the active drag-selection highlight onto the
// rendered frame. Content rows are mapped to screen rows through the same
// header-height/viewport-offset geometry used by selection and click handlers.
func (m *Model) applySelectionOverlay(frame string) string {
	if !m.selActive || m.selAnchor == nil || m.selFocus == nil || m.overlaySurface() {
		return frame
	}
	anchor, focus := *m.selAnchor, *m.selFocus
	if anchor == focus {
		return frame
	}
	headerH := presentationHeight(m.headerPresentation(), m.width)
	vpH := m.viewport.Height
	// leftPad is the number of cells the transcript content is indented from the
	// left edge of its frame line (e.g. the viewport's horizontal padding). The
	// highlight must be drawn at screen columns leftPad+start..leftPad+end.
	leftPad := m.viewport.Style.GetHorizontalFrameSize() / 2
	firstV := m.viewport.YOffset
	lastV := firstV + vpH - 1
	rows := strings.Split(frame, "\n")
	rect := normalizeSel(anchor, focus)
	for r := rect.Top.Row; r <= rect.Bottom.Row && r <= lastV; r++ {
		if r < firstV {
			continue
		}
		scr := headerH + (r - firstV)
		if scr < 0 || scr >= len(rows) {
			continue
		}
		plainWidth := 0
		if r < len(m.selGrid) {
			plainWidth = lipgloss.Width(m.selGrid[r])
		}
		start, end := selectionRowRange(anchor, focus, r)
		end = min(end, plainWidth)
		start = max(0, start)
		if start >= end {
			continue
		}
		rows[scr] = highlightRange(rows[scr], leftPad+start, leftPad+end)
	}
	return strings.Join(rows, "\n")
}

// selectionRowRange returns the display-cell column span [(start,end)) that a
// given content row occupies in a selection between anchor and focus, mirroring
// selectionText. Missing-edge rows clamp to the row width by the caller.
func selectionRowRange(anchor, focus selPoint, row int) (int, int) {
	const huge = 1 << 30
	rect := normalizeSel(anchor, focus)
	if rect.Single {
		if row != rect.Top.Row {
			return 0, 0
		}
		lo, hi := rect.Top.Col, rect.Bottom.Col
		if hi < lo {
			lo, hi = hi, lo
		}
		return lo, max(lo+1, hi)
	}
	if row == rect.Top.Row {
		if anchor.Row > focus.Row {
			return focus.Col, huge
		}
		return anchor.Col, huge
	}
	if row == rect.Bottom.Row {
		if anchor.Row > focus.Row {
			return 0, anchor.Col
		}
		return 0, focus.Col
	}
	return 0, huge
}

// highlightRange inverts the cells [from,to)) of a styled line so the
// selection is visible, preserving surrounding colors via cell-aware slicing.
// `to` is the line's full visible width (right edge), so the post slice never
// truncates trailing content.
func highlightRange(line string, from, to int) string {
	if from < 0 {
		from = 0
	}
	full := lipgloss.Width(stripANSI(line))
	from, to = min(from, full), min(to, full)
	if to <= from {
		return line
	}
	mid := ansi.Cut(line, from, to)
	if mid == "" {
		return line
	}
	pre := ansi.Cut(line, 0, from)
	post := ansi.Cut(line, to, full)
	// Reverse video marks the selection. Emitted directly because lipgloss's
	// Reverse() produces no sequence in this version, which would leave the
	// highlight invisible. The slice is stripped of its inner styling: an
	// embedded reset (\x1b[0m) would otherwise cancel the reverse video midway
	// and leave a patchy highlight over styled spans.
	return pre + "\x1b[7m" + stripANSI(mid) + "\x1b[0m" + post
}

func (m *Model) renderReaderView() string {
	if m == nil || m.reader == nil {
		return ""
	}
	width := max(1, m.width-2)
	state := m.renderPersistentStatus()
	readerTitle := styleModalTitle.Render(truncateDisplay(sanitizeANSI(m.readerHeader()), width))
	bodyHeight := max(1, m.height-presentationHeight(m.renderHeader(), width)-presentationHeight(readerTitle, width)-presentationHeight(m.readerStatus(), width)-presentationHeight(m.readerHint(), width)-presentationHeight(state, width)-1)
	body := m.readerBody(width, bodyHeight)
	segments := []presentationSegment{
		{text: m.renderHeader(), mandatory: true},
		{text: readerTitle, mandatory: true},
		{text: body, mandatory: false},
		{text: m.readerStatus(), mandatory: true},
		{text: styleHints.Render(m.readerHint()), mandatory: true},
		{text: state, mandatory: true},
	}
	return fitPresentation(segments, m.width, m.height)
}

// maxTaskRows bounds the checklist so a long todo list cannot crowd the
// transcript; overflow is summarized with a "+N more" line rather than hidden.
const maxTaskRows = 12

func wrapDisplay(s string, width int) []string {
	if s == "" {
		return nil
	}
	wrapped := lipgloss.NewStyle().Width(max(1, width)).Render(s)
	lines := strings.Split(wrapped, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	return lines
}

func truncateDisplay(s string, width int) string {
	s = strings.Join(strings.Fields(s), " ")
	if width <= 0 || lipgloss.Width(s) <= width {
		return s
	}
	var out strings.Builder
	used := 0
	graphemes := uniseg.NewGraphemes(s)
	for graphemes.Next() {
		cluster := graphemes.Str()
		clusterWidth := lipgloss.Width(cluster)
		if used+clusterWidth+1 > width {
			break
		}
		out.WriteString(cluster)
		used += clusterWidth
	}
	return out.String() + "…"
}

// render draws the conversation log into the viewport. Item text is
// sanitized once per item (cached in the item) so escape sequences embedded
// in tool output or model text can never corrupt the display, while
// lipgloss's own styling sequences are applied afterwards and survive.
func (m *Model) render() {
	var sb strings.Builder
	contentWidth := m.viewport.Width - m.viewport.Style.GetHorizontalFrameSize()
	currentLine := 0
	m.clickTargets = nil
	m.linkTargets = nil
	m.selGrid = m.selGrid[:0]
	for i := range m.items {
		if i > 0 && transcriptGap(m.items[i-1].kind, m.items[i].kind) {
			sb.WriteString("\n")
			m.selGrid = append(m.selGrid, "")
			currentLine++
		}
		item := m.renderLogItem(&m.items[i], contentWidth)
		if contentWidth > 0 {
			item = lipgloss.NewStyle().Width(contentWidth).Render(item)
		}
		itemLines := strings.Count(item, "\n") + 1
		// Capture each rendered row's visible text for in-app drag selection,
		// aligned 1:1 with the content row numbering used by clickTargets.
		// Trailing padding (added when the item is width-justified) is trimmed
		// so a mid-selection row highlights and copies only its real text.
		// The same rows yield the hyperlink spans, which let a click open the
		// link: mouse reporting is on, so the terminal cannot do it for us.
		var links linkScanner
		for li, l := range strings.Split(item, "\n") {
			m.selGrid = append(m.selGrid, strings.TrimRight(stripANSI(l), " "))
			for _, span := range links.spans(l) {
				span.row = currentLine + li
				m.linkTargets = append(m.linkTargets, span)
			}
		}
		if m.items[i].kind == "tool" {
			if isAgentTool(m.items[i].toolName) {
				cell := &m.items[i]
				children := cell.agent
				addTarget := func(start, count int, kind, id string) {
					for line := start; line < start+count; line++ {
						m.clickTargets = append(m.clickTargets, clickTarget{line: line, kind: kind, id: id})
					}
				}
				if len(children) > 1 {
					heights := make([]int, len(children))
					childRows := 0
					for j, child := range children {
						heights[j] = presentationHeight(renderAgentChildLine(child, j == len(children)-1, contentWidth), contentWidth)
						childRows += heights[j]
					}
					headerRows := max(1, itemLines-childRows)
					addTarget(currentLine, headerRows, "agents", "")
					row := currentLine + headerRows
					for j, child := range children {
						addTarget(row, heights[j], "agent", string(child.SessionID))
						row += heights[j]
					}
				} else if len(children) == 1 {
					addTarget(currentLine, itemLines, "agent", string(children[0].SessionID))
				} else if sid, ok := cell.toolArgs["session_id"].(string); ok && sid != "" {
					addTarget(currentLine, itemLines, "agent", sid)
				} else if cell.toolID != "" {
					addTarget(currentLine, itemLines, "tool", cell.toolID)
				}

			} else if m.items[i].toolID != "" {
				for l := 0; l < itemLines; l++ {
					m.clickTargets = append(m.clickTargets, clickTarget{
						line: currentLine + l,
						kind: "tool",
						id:   m.items[i].toolID,
					})
				}
			}
		} else if m.items[i].kind == "thinking" && !thinkingInProgress(&m.items[i]) {
			id := detailIdentity(&m.items[i], i)
			for l := 0; l < itemLines; l++ {
				m.clickTargets = append(m.clickTargets, clickTarget{
					line: currentLine + l,
					kind: "thought",
					id:   id,
				})
			}
		}
		currentLine += itemLines
		sb.WriteString(item)
		sb.WriteString("\n")
	}
	m.viewport.SetContent(strings.TrimRight(sb.String(), "\n"))
	m.transcriptScroll.Set(m.viewport.Height, m.viewport.TotalLineCount())
	m.transcriptScroll.Offset = min(m.viewport.YOffset, m.transcriptScroll.MaxOffset())
}

func renderItemWidth(it *historyCell, width int) string {
	text := it.cleanText()
	switch it.kind {
	case "welcome":
		return renderWelcome(text, width)

	case "user":
		return renderUserCell(text, width)

	case "command":
		return styleUser.Render("❯ " + text)

	case "assistant":
		return renderAssistantCell(text, width)

	case "thinking":
		return renderThinkingCell(it, width)

	case "tool":
		return renderToolView(it, width)

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

func indentAssistant(text string) string {
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = "  " + lines[i]
	}
	return strings.Join(lines, "\n")
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[len(id)-8:]
}

func (m *Model) renderVisibleTranscript() {
	m.render()
}
