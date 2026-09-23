package tui

import (
	"fmt"
	"strings"
	"testing"
)

func TestComposerKeepsBlankGapWithFullTranscript(t *testing.T) {
	for _, width := range []int{40, 80, 120} {
		for _, panel := range []string{"none", "context", "thought"} {
			t.Run(fmt.Sprintf("%d/%s", width, panel), func(t *testing.T) {
				m := astraModel(t, width, 30)
				for i := 0; i < 60; i++ {
					m.items = append(m.items, historyCell{kind: "assistant", text: fmt.Sprintf("output %d", i)})
				}
				m.items = append(m.items, historyCell{kind: "thinking", toolID: "thought", text: strings.Repeat("details\n", 50)})
				m.layout()
				switch panel {
				case "context":
					m.showContextDetail = true
				case "thought":
					m.toggleToolExpanded("thought")
				}
				m.syncViewportHeight()
				rows := strings.Split(stripANSI(m.View()), "\n")
				chrome := m.composeChrome()
				gap := len(rows) - presentationHeight(chrome.footer, width) - presentationHeight(chrome.input, width)
				if gap < 0 || strings.TrimSpace(rows[gap]) != "" {
					t.Fatalf("missing blank row above composer: %q", rows)
				}
				if presentationHeight(chrome.input, width) != presentationHeight(m.renderInput(), width)+2 {
					t.Fatal("two-row gap not reserved in layout")
				}
			})
		}
	}
}
