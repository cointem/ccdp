package tui

import (
	"fmt"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"strings"
	"testing"
)

func TestSelectorsGrowWithTerminalAndPageByVisibleRows(t *testing.T) {
	for _, height := range []int{16, 24, 40, 56} {
		m := astraModel(t, 100, height)
		options := make([]SelectorOption, 40)
		for i := range options {
			options[i] = SelectorOption{ID: fmt.Sprint(i), Label: fmt.Sprintf("model-%d", i)}
		}
		m.startSelectorAt("Model", options, 0, true, selectorAction{Kind: selectorModel})
		visible := m.pickerVisibleRows()
		if height >= 40 && visible <= 10 {
			t.Fatalf("tall terminal still limited to %d rows", visible)
		}
		m.handlePickerKey(tea.KeyMsg{Type: tea.KeyPgDown})
		if m.picker.Index != visible {
			t.Fatalf("page step %d != visible rows %d", m.picker.Index, visible)
		}
		m.layout()
		frame := sanitizeANSI(m.View())
		if !strings.Contains(frame, fmt.Sprintf("model-%d", visible)) || !strings.Contains(frame, "esc cancel") {
			t.Fatalf("selection or controls clipped: %s", frame)
		}
		if lipgloss.Height(frame) > height || lipgloss.Width(frame) > 100 {
			t.Fatal("frame overflow")
		}
	}
}

func TestApprovalAndContextUseLargeTerminalSpace(t *testing.T) {
	m := astraModel(t, 160, 56)
	m.approval = &approvalPrompt{Tool: "Bash", Command: strings.Repeat("command detail\n", 40)}
	if m.approvalVisibleRows() <= 8 {
		t.Fatal("approval retains fixed small height")
	}
	if lipgloss.Width(m.renderContextDetailPopover()) <= 54 {
		t.Fatal("context retains fixed small width")
	}
	m.width = 24
	if lipgloss.Width(m.renderContextDetailPopover()) > 24 {
		t.Fatal("narrow context overflows")
	}
}
