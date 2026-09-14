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
