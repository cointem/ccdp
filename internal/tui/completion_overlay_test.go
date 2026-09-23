package tui

import (
	"fmt"
	tea "github.com/charmbracelet/bubbletea"
	"strings"
	"testing"
)

func TestCommandCompletionOverlaysWithoutMovingHistory(t *testing.T) {
	for _, width := range []int{40, 80, 200} {
		m := astraModel(t, width, 30)
		m.items = nil
		for i := 0; i < 50; i++ {
			m.items = append(m.items, historyCell{kind: "assistant", text: fmt.Sprintf("RECORD_%02d", i)})
		}
		m.textarea.SetValue("/")
		m.layout()
		before := strings.Split(sanitizeANSI(m.View()), "\n")
		height, offset := m.viewport.Height, m.viewport.YOffset
		m.refreshCmdSuggest()
		m.syncViewportHeight()
		if m.viewport.Height != height || m.viewport.YOffset != offset {
			t.Fatal("opening completion resized or scrolled history")
		}
		chrome := m.composeChrome()
		bottom := m.height - presentationHeight(chrome.input, width) - presentationHeight(chrome.footer, width)
		top := bottom - presentationHeight(chrome.surface, width)
		after := strings.Split(sanitizeANSI(m.View()), "\n")
		for row := range before {
			if row >= top && row < bottom {
				continue
			}
			if before[row] != after[row] {
				t.Fatalf("width %d: row %d moved outside overlay", width, row)
			}
		}
		m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
		m.syncViewportHeight()
		restored := strings.Split(sanitizeANSI(m.View()), "\n")
		for i := 0; i < bottom; i++ {
			if restored[i] != before[i] {
				t.Fatalf("closing completion did not restore row %d", i)
			}
		}
	}
}
