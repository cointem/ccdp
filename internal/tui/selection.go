package tui

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/rivo/uniseg"
)

// selPoint is a cell in the transcript content (row, col) where row indexes a
// wrapped display row (the same numbering clickTargets and mouse handlers use)
// and col is a display cell offset within that row's visible text.
type selPoint struct {
	Row int
	Col int
}

// selRect is the normalized selection range over content rows: it records which
// rows are selected and, for a single-row selection, the column span.
type selRect struct {
	Top    selPoint
	Bottom selPoint
	Single bool
}

// normalizeSel converts an anchor/focus pair into an ordered rectangle suitable
// for row-by-row extraction regardless of drag direction.
func normalizeSel(anchor, focus selPoint) selRect {
	if anchor.Row == focus.Row {
		lo, hi := anchor, focus
		if hi.Col < lo.Col {
			lo, hi = hi, lo
		}
		return selRect{Top: lo, Bottom: hi, Single: true}
	}
	top, bottom := anchor, focus
	if bottom.Row < top.Row {
		top, bottom = bottom, top
	}
	return selRect{Top: top, Bottom: bottom, Single: false}
}

// selectionText extracts the visible plain text spanning [anchor, focus] from
// rows (each entry is the ANSI-stripped, width-measured text of one content
// row, aligned with the content row numbering used by clickTargets).
func selectionText(rows []string, anchor, focus selPoint) string {
	if anchor == focus {
		return ""
	}
	rect := normalizeSel(anchor, focus)
	if rect.Top.Row >= len(rows) {
		return ""
	}
	var b strings.Builder
	for r := rect.Top.Row; r <= rect.Bottom.Row; r++ {
		if r > rect.Top.Row {
			b.WriteByte('\n')
		}
		line := ""
		if r < len(rows) {
			line = rows[r]
		}
		width := lipgloss.Width(line)
		var start, end int
		if rect.Single {
			start = min(rect.Top.Col, width)
			end = min(rect.Bottom.Col, width)
		} else if r == rect.Top.Row {
			// top row: include from the upper-boundary cell to end of line.
			upper := rect.Top
			if anchor.Row > focus.Row {
				// drag upward: upper boundary is the focus cell on the top row.
				upper = focus
			}
			start = min(upper.Col, width)
			end = width
		} else if r == rect.Bottom.Row {
			lower := rect.Bottom
			if anchor.Row > focus.Row {
				lower = anchor
			}
			start = 0
			end = min(lower.Col, width)
		} else {
			start, end = 0, width
		}
		b.WriteString(cutWidth(line, start, end))
	}
	return b.String()
}

// cutWidth returns the substring of s that occupies display cells [from, to).
// Cells respect wide rune widths (CJK characters take two cells).
func cutWidth(s string, from, to int) string {
	if from >= to || from < 0 || to < 0 {
		return ""
	}
	var out strings.Builder
	col := 0
	gr := uniseg.NewGraphemes(s)
	for gr.Next() {
		cluster := gr.Str()
		w := lipgloss.Width(cluster)
		if col+w <= from {
			col += w
			continue
		}
		if col >= to {
			break
		}
		out.WriteString(cluster)
		col += w
	}
	return out.String()
}

// osc52Seq renders text as an OSC 52 clipboard-write sequence (ESC ] 52 ; c ;
// base64 BEL). It is consumed in-band by terminals that support OSC 52 so the
// selection can be copied even over SSH or tmux where a local helper cannot.
func osc52Seq(text string) string {
	return "\x1b]52;c;" + base64Std(text) + "\a"
}

// base64Std keeps the OSC 52 payload free of the common helper's concerns by
// delegating to the standard encoder. Separated so tests can assert on content.
func base64Std(text string) string {
	return base64.StdEncoding.EncodeToString([]byte(text))
}

// copySelectionCmd writes the selected text to the clipboard, mirroring claude
// code's copy-on-select: for remote/tmux sessions an in-band OSC 52 sequence is
// emitted (best-effort, works where pbcopy/xclip cannot), and a bounded local
// clipboard write runs for local hosts. Returns a clipboardResultMsg so stale
// results never land in an already-switched session.
func (m *Model) copySelectionCmd(text string) tea.Cmd {
	if text == "" || strings.TrimSpace(text) == "" {
		return nil
	}
	writer := m.clipboard
	if writer == nil {
		writer = hostClipboard{}
	}
	sid, generation := m.sessionID, m.reportGeneration
	chars := len([]rune(text))
	return func() tea.Msg {
		if m.terminal != nil {
			m.terminal.WriteRaw(osc52Seq(text))
		}
		ctx, cancel := context.WithTimeout(context.Background(), clipboardWriteTimeout)
		defer cancel()
		err := writer.WriteAllContext(ctx, text)
		return clipboardResultMsg{sessionID: sid, generation: generation, err: err, selection: true, chars: chars}
	}
}

// copiedToastDuration keeps the copy hint visible briefly. The toast lives in
// the permanent gap row above the composer, so showing or hiding it never
// changes the layout or shifts the transcript.
const copiedToastDuration = 3 * time.Second

// copiedToastExpireMsg clears the toast once its owning tick fires. The seq
// guard keeps a stale tick from erasing a newer toast.
type copiedToastExpireMsg struct{ seq uint64 }

// showCopiedToast renders "copied N chars" right-aligned above the composer
// and schedules its expiry.
func (m *Model) showCopiedToast(chars int) tea.Cmd {
	m.copiedToast = fmt.Sprintf("copied %d chars", chars)
	m.copiedToastSeq++
	seq := m.copiedToastSeq
	return tea.Tick(copiedToastDuration, func(time.Time) tea.Msg { return copiedToastExpireMsg{seq: seq} })
}
