package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// presentationSegment lets View reserve rows from the bottom of the screen
// while keeping the recent transcript as the only elastic region. A segment
// may contain styled lines; measurement always uses terminal cells rather than
// bytes.
type presentationSegment struct {
	text      string
	mandatory bool
}

func presentationRows(text string, width int) []string {
	if text == "" {
		return nil
	}
	width = max(1, width)
	wrapped := lipgloss.NewStyle().Width(width).Render(text)
	rows := strings.Split(strings.TrimRight(wrapped, "\n"), "\n")
	if len(rows) == 1 && rows[0] == "" {
		return nil
	}
	return rows
}

func presentationHeight(text string, width int) int {
	return len(presentationRows(text, width))
}

// fitPresentation keeps the bottom controls visible and trims only the
// elastic transcript segment when a short terminal cannot show all history.
// Decision surfaces and persistent settings are mandatory, so their rows are
// retained even when the transcript has to collapse to its last line.
func fitPresentation(segments []presentationSegment, width, height int) string {
	if width <= 0 {
		width = 1
	}
	if height <= 0 {
		return ""
	}
	rows := make([][]string, len(segments))
	total := 0
	elastic := -1
	for i, segment := range segments {
		rows[i] = presentationRows(segment.text, width)
		total += len(rows[i])
		if !segment.mandatory && elastic < 0 {
			elastic = i
		}
	}
	if total < height && elastic >= 0 {
		// Only the transcript absorbs spare rows; controls stay bottom-aligned.
		rows[elastic] = append(rows[elastic], make([]string, height-total)...)
	}
	if total > height && elastic >= 0 {
		need := total - height
		if need >= len(rows[elastic]) {
			rows[elastic] = nil
		} else {
			rows[elastic] = rows[elastic][need:]
		}
	}
	// A very narrow terminal can still leave more rows than its height after
	// trimming the transcript (for example a multi-line composer). Preserve the
	// latest rows of the complete surface as a final safety valve; callers keep
	// the raw transcript and can reopen it in the reader.
	all := make([]string, 0, min(height, total))
	for _, segmentRows := range rows {
		all = append(all, segmentRows...)
	}
	if len(all) > height {
		all = all[len(all)-height:]
	}
	return strings.Join(all, "\n")
}

func wrapPersistentStatus(text string, width int) string {
	if text == "" {
		return ""
	}
	return strings.Join(presentationRows(text, max(1, width)), "\n")
}

// frameChrome is the one measurement/rendering contract for all bottom and
// header components. Both the viewport budget and View use these exact rows.
type frameChrome struct {
	header, status, tasks, surface, input, footer string
}

// Base chrome never depends on which transient panel currently owns focus.
func (m *Model) composeChrome() frameChrome {
	base := *m
	base.exitConfirm = false
	base.reader = nil
	base.detail = nil
	base.picker = nil
	base.question = nil
	base.approval = nil
	base.histSearch = nil
	base.showContextDetail = false
	base.cmdSug = nil
	base.fileMention = nil
	base.tasksVisible = false
	c := frameChrome{header: m.headerPresentation(), status: base.renderStatus(), surface: m.bottomSurface(), input: base.renderInput(), footer: base.renderFooter()}
	// Keep a one-row gap between output/overlays and the composer. The row
	// directly above the composer is the input's hint lane: the copy toast is
	// right-aligned there and a command receipt ("effort → xhigh") takes its left
	// edge. The row always exists, so neither hint shifts the transcript when it
	// appears or expires.
	if c.input != "" {
		toast := ""
		if m.copiedToast != "" {
			toast = styleHints.Render(truncateDisplay(m.copiedToast, max(1, m.width)))
		}
		row := base.renderComposerFeedback(max(1, m.width-lipgloss.Width(toast)-1))
		if row != "" || toast != "" {
			pad := m.width - lipgloss.Width(row) - lipgloss.Width(toast)
			if row != "" && toast != "" && pad < 1 {
				pad = 1
			}
			row += strings.Repeat(" ", max(0, pad)) + toast
		}
		c.input = "\n" + row + "\n" + c.input
	}
	if !m.hasSnapshot {
		if state := base.renderPersistentStatus(); state != "" {
			c.footer = indentBlock(state) + "\n" + c.footer
		}
	}
	return c
}
func (c frameChrome) fixedHeight(width int) int {
	total := 0
	for _, text := range []string{c.header, c.status, c.tasks, c.input, c.footer} {
		total += presentationHeight(text, width)
	}
	return total
}
func (c frameChrome) segments(body string) []presentationSegment {
	return []presentationSegment{
		{text: c.header, mandatory: true}, {text: body},
		{text: c.status, mandatory: true}, {text: c.tasks, mandatory: true},
		{text: c.surface, mandatory: true}, {text: c.input, mandatory: true}, {text: c.footer, mandatory: true},
	}
}
