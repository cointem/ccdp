package tui

import (
	"encoding/base64"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// clickAt performs a full press+release at a cell, returning the command the
// click resolves to. A real mouse always emits both events; this mirrors the
// app-level click/drag disambiguation (click acts on release).
func clickAt(m *Model, x, y int) tea.Cmd {
	_, _ = m.handleMouse(tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	_, cmd := m.handleMouse(tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionRelease})
	return cmd
}

func TestSelectionText_SingleRow(t *testing.T) {
	rows := []string{"hello world", "second line"}
	got := selectionText(rows, selPoint{Row: 0, Col: 0}, selPoint{Row: 0, Col: 5})
	if got != "hello" {
		t.Fatalf("single-row: got %q want %q", got, "hello")
	}
}

func TestSelectionText_MultiRowForward(t *testing.T) {
	rows := []string{"abcdef", "ghijkl"}
	// anchor at (0,2), focus at (1,3) forward: "cdef" + newline + "ghi"
	got := selectionText(rows, selPoint{Row: 0, Col: 2}, selPoint{Row: 1, Col: 3})
	if want := "cdef\nghi"; got != want {
		t.Fatalf("forward: got %q want %q", got, want)
	}
}

func TestSelectionText_MultiRowBackward(t *testing.T) {
	rows := []string{"abcdef", "ghijkl"}
	// drag upward: anchor (1,3), focus (0,2) → same text as forward
	got := selectionText(rows, selPoint{Row: 1, Col: 3}, selPoint{Row: 0, Col: 2})
	if want := "cdef\nghi"; got != want {
		t.Fatalf("backward: got %q want %q", got, want)
	}
}

func TestSelectionText_WideRunes(t *testing.T) {
	// "中文" occupies 4 cells; selecting cells [0,2) yields "中".
	rows := []string{"中文ab"}
	got := selectionText(rows, selPoint{Row: 0, Col: 0}, selPoint{Row: 0, Col: 2})
	if got != "中" {
		t.Fatalf("wide rune cut: got %q want %q", got, "中")
	}
}

func TestSelectionText_ClampsBeyondWidth(t *testing.T) {
	rows := []string{"abc"}
	// Focus col beyond the row width must not panic or emit spurious output.
	got := selectionText(rows, selPoint{Row: 0, Col: 0}, selPoint{Row: 0, Col: 900})
	if got != "abc" {
		t.Fatalf("clamp: got %q want %q", got, "abc")
	}
}

func TestOsc52Seq(t *testing.T) {
	seq := osc52Seq("hi")
	const prefix = "\x1b]52;c;"
	if !strings.HasPrefix(seq, prefix) || !strings.HasSuffix(seq, "\a") {
		t.Fatalf("osc52 framing wrong: %q", seq)
	}
	payload := strings.TrimSuffix(strings.TrimPrefix(seq, prefix), "\a")
	if payload != base64.StdEncoding.EncodeToString([]byte("hi")) {
		t.Fatalf("osc52 payload %q mismatch", payload)
	}
	dec, err := base64.StdEncoding.DecodeString(payload)
	if err != nil || string(dec) != "hi" {
		t.Fatalf("osc52 decode: %q err=%v", dec, err)
	}
}

func TestSelectionRowRange_Single(t *testing.T) {
	// Single row [1,4): only that row carries the span.
	s, e := selectionRowRange(selPoint{Row: 2, Col: 1}, selPoint{Row: 2, Col: 4}, 2)
	if s != 1 || e != 4 {
		t.Fatalf("single-row range: got (%d,%d) want (1,4)", s, e)
	}
	if s, e := selectionRowRange(selPoint{Row: 2, Col: 1}, selPoint{Row: 2, Col: 4}, 3); s != 0 || e != 0 {
		t.Fatalf("off-row range should be empty, got (%d,%d)", s, e)
	}
}

// TestHighlightRangeScreenCols verifies the highlight is applied at the shifted
// screen columns (content col + left padding) and never truncates the line tail.
func TestHighlightRangeScreenCols(t *testing.T) {
	// A frame line with 1 leading padding cell, then content "0123456789".
	line := " 0123456789"
	// Highlight content cols [2,5) (= screen cols [3,6), highlighting "234").
	out := highlightRange(line, 3, 6)
	// The three selected cells are wrapped in a reverse-video style (ANSI 7m).
	if !strings.Contains(out, "\x1b[7m") {
		t.Fatalf("no reverse-video marker: %q", out)
	}
	// Head and tail must be preserved around the highlighted slice.
	if !strings.HasPrefix(out, " 01") || !strings.HasSuffix(out, "6789") {
		t.Fatalf("head/tail truncated: %q", out)
	}
	// Reading the visible (ANSI-stripped) text still yields the full line.
	if got := stripANSI(out); got != " 0123456789" {
		t.Fatalf("visible text changed: %q", got)
	}
}

// TestHighlightRangeStripsInnerStyling pins the patchy-highlight fix: a reset
// sequence inside the highlighted slice must not cancel the reverse video
// midway, so the slice is re-emitted as plain text between the markers.
func TestHighlightRangeStripsInnerStyling(t *testing.T) {
	line := "\x1b[31mhello\x1b[0m world"
	out := highlightRange(line, 0, 5)
	if !strings.Contains(out, "\x1b[7mhello\x1b[0m") {
		t.Fatalf("highlighted slice not plain reversed text: %q", out)
	}
	if strings.Contains(out, "\x1b[7m\x1b[31m") {
		t.Fatalf("inner styling leaked into highlight: %q", out)
	}
	if got := stripANSI(out); got != "hello world" {
		t.Fatalf("visible text changed: %q", got)
	}
}

// dragAt performs press → motion → release, mirroring a real drag gesture.
func dragAt(m *Model, x0, y0, x1, y1 int) tea.Cmd {
	_, _ = m.handleMouse(tea.MouseMsg{X: x0, Y: y0, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	_, _ = m.handleMouse(tea.MouseMsg{X: x1, Y: y1, Button: tea.MouseButtonLeft, Action: tea.MouseActionMotion})
	_, cmd := m.handleMouse(tea.MouseMsg{X: x1, Y: y1, Button: tea.MouseButtonLeft, Action: tea.MouseActionRelease})
	return cmd
}

// TestDragCopyKeepsHighlightAfterRelease pins the copied-selection highlight:
// the anchor/focus must survive the release so the user sees exactly what was
// copied until the next press.
func TestDragCopyKeepsHighlightAfterRelease(t *testing.T) {
	m := inlineTestModel()
	m.clipboard = &captureClipboard{}
	// Match the production viewport padding so screen↔content column mapping
	// exercises the same left-pad shift as the real TUI.
	m.viewport.Style = lipgloss.NewStyle().Padding(0, 1)
	m.width, m.height = 80, 24
	m.items = []historyCell{{kind: "assistant", messageID: "a1", text: "hello world"}}
	m.layout()
	// Assistant rows render with a "• " prefix: content cols [2,7) hold "hello",
	// which sits at screen cols [3,8) after the viewport's one-cell left pad.
	cmd := dragAt(m, 3, 0, 8, 0)
	if cmd == nil {
		t.Fatal("drag over text returned no copy command")
	}
	if !m.selActive || m.selAnchor == nil || m.selFocus == nil {
		t.Fatalf("selection highlight dropped on release: active=%v anchor=%v focus=%v",
			m.selActive, m.selAnchor, m.selFocus)
	}

	// Delivering the clipboard result surfaces the toast instead of a status
	// notice, so the transcript never shifts.
	model, toastCmd := m.Update(cmd())
	updated := modelValue(t, model)
	m = &updated
	if got := m.clipboard.(*captureClipboard).text; got != "hello" {
		t.Fatalf("clipboard text=%q, want %q", got, "hello")
	}
	if status := noticeText(m); status != "" {
		t.Fatalf("drag copy raised a layout-shifting status notice: %q", status)
	}
	if m.copiedToast != "copied 5 chars" {
		t.Fatalf("toast=%q, want %q", m.copiedToast, "copied 5 chars")
	}
	if toastCmd == nil {
		t.Fatal("toast did not schedule its expiry tick")
	}
}

// TestCopiedToastExpiryUsesSequenceGuard ensures a stale tick cannot erase a
// newer toast.
func TestCopiedToastExpiryUsesSequenceGuard(t *testing.T) {
	m := inlineTestModel()
	_ = m.showCopiedToast(5)
	stale := m.copiedToastSeq
	_ = m.showCopiedToast(12)
	if model, _ := m.Update(copiedToastExpireMsg{seq: stale}); modelValue(t, model).copiedToast == "" {
		t.Fatal("stale expiry tick cleared the newer toast")
	}
	model, _ := m.Update(copiedToastExpireMsg{seq: m.copiedToastSeq})
	if updated := modelValue(t, model); updated.copiedToast != "" {
		t.Fatalf("current expiry tick did not clear the toast: %q", updated.copiedToast)
	}
}

// TestComposerGapHostsToastWithoutLayoutShift pins the two-row composer gap:
// the toast occupies the row directly above the composer, right-aligned, and
// the input segment row count never changes when the toast appears.
func TestComposerGapHostsToastWithoutLayoutShift(t *testing.T) {
	m := inlineTestModel()
	m.width = 40
	plain := strings.Split(m.composeChrome().input, "\n")
	if len(plain) < 3 || plain[0] != "" || plain[1] != "" {
		t.Fatalf("composer gap should be two leading blank rows, got %q", plain[:2])
	}
	m.copiedToast = "copied 5 chars"
	withToast := strings.Split(m.composeChrome().input, "\n")
	if len(withToast) != len(plain) {
		t.Fatalf("toast changed the input row count: %d → %d", len(plain), len(withToast))
	}
	row := stripANSI(withToast[1])
	if !strings.HasSuffix(row, "copied 5 chars") {
		t.Fatalf("toast not right-aligned in the gap row: %q", row)
	}
	if w := lipgloss.Width(row); w != 40 {
		t.Fatalf("gap row width=%d, want full terminal width 40", w)
	}
}
