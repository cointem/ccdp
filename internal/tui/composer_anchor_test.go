package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func TestComposerStaysAtBottomAcrossStatusAndPanels(t *testing.T) {
	for _, size := range [][2]int{{40, 16}, {80, 24}, {120, 40}} {
		m := astraModel(t, size[0], size[1])
		m.textarea.Reset()
		assertBottom := func() {
			t.Helper()
			m.layout()
			view := m.View()
			rows := strings.Split(sanitizeANSI(view), "\n")
			if lipgloss.Height(view) != size[1] {
				t.Fatalf("frame no longer fills %v", size)
			}
			inputRow := -1
			for i, row := range rows {
				if strings.Contains(row, "Ask CCDP") {
					inputRow = i
				}
			}
			want := size[1] - presentationHeight(m.renderFooter(), size[0]) - 2
			if inputRow != want {
				t.Fatalf("%v: composer row %d, want %d", size, inputRow, want)
			}
		}
		assertBottom()
		m.pushStatus("permission mode → default")
		assertBottom()
		m.busy = true
		assertBottom()
		m.busy = false
		m.runCommand("/effort")
		m.layout()
		if !strings.Contains(sanitizeANSI(m.View()), "▲") {
			t.Fatal("effort pointer missing")
		}
		m.handlePickerKey(tea.KeyMsg{Type: tea.KeyEsc})
		assertBottom()
	}
}

// A slash command that only adjusts state answers the submission rather than the
// conversation. Its receipt belongs to the hint row directly above the composer:
// it must not be written into the transcript and must not be rendered in the
// status lane, where it reads as one more row of output.
func TestAdjustmentReceiptStaysInComposerLane(t *testing.T) {
	m := sugModel()
	m.width, m.height = 80, 24
	baseline := m.composeChrome().fixedHeight(m.width)

	applyTeaCmd(m, m.executeSelectorAction(selectorAction{Kind: selectorEffort}, "xhigh"))

	chrome := m.composeChrome()
	if status := sanitizeANSI(m.renderStatus()); strings.Contains(status, "effort → xhigh") {
		t.Fatalf("adjustment receipt leaked into the status lane: %q", status)
	}
	if !strings.Contains(sanitizeANSI(chrome.input), "effort → xhigh") {
		t.Fatalf("adjustment receipt missing from the composer lane: %q", chrome.input)
	}
	if rows := strings.Split(sanitizeANSI(chrome.input), "\n"); len(rows) < 2 || rows[0] != "" || !strings.HasPrefix(rows[1], "effort → xhigh") {
		t.Fatalf("receipt row is not the leftmost row above the composer: %q", chrome.input)
	}
	for _, item := range m.items {
		if strings.Contains(item.text, "effort → xhigh") {
			t.Fatalf("adjustment receipt was written into the transcript: %#v", item)
		}
	}
	if height := m.composeChrome().fixedHeight(m.width); height != baseline {
		t.Fatalf("receipt reflowed the chrome: height %d, want %d", height, baseline)
	}
}
