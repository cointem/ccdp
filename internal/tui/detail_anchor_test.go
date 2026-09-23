package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestDetailsOverlayAtClickedRecord(t *testing.T) {
	for _, kind := range []string{"tool", "thinking"} {
		for _, width := range []int{40, 80, 120} {
			for _, offset := range []int{0, 3} {
				t.Run(fmt.Sprintf("%s/%d/%d", kind, width, offset), func(t *testing.T) {
					m := astraModel(t, width, 30)
					for i := 0; i < 5; i++ {
						m.items = append(m.items, historyCell{kind: "assistant", text: fmt.Sprintf("before %d", i)})
					}
					m.items = append(m.items, historyCell{kind: kind, toolID: "target", toolName: "Bash", text: "DETAIL_CONTENT", status: "success"})
					for i := 0; i < 30; i++ {
						m.items = append(m.items, historyCell{kind: "assistant", text: fmt.Sprintf("after %d", i)})
					}
					m.layout()
					m.followOutput = false
					m.viewport.SetYOffset(offset)
					target := m.clickTargets[0]
					y := presentationHeight(m.headerPresentation(), width) + target.line - offset
					before := strings.Split(stripANSI(m.View()), "\n")
					h := m.viewport.Height
					clickAt(&m, 2, y)
					m.syncViewportHeight()
					after := strings.Split(stripANSI(m.View()), "\n")
					if m.detail == nil || m.detailRow != y || !strings.Contains(after[y], "[click to collapse]") {
						t.Fatalf("detail not at clicked row %d", y)
					}
					end := min(m.height-presentationHeight(m.composeChrome().input, width)-presentationHeight(m.composeChrome().footer, width), y+presentationHeight(m.renderDetailPopover(), width))
					if m.viewport.Height != h || m.viewport.YOffset != offset {
						t.Fatal("changed transcript layout")
					}
					for i := range before {
						if (i < y || i >= end) && before[i] != after[i] {
							t.Fatalf("uncovered row %d changed", i)
						}
					}
					m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
					if stripANSI(m.View()) != strings.Join(before, "\n") {
						t.Fatal("close did not restore original frame")
					}
				})
			}
		}
	}
}

func TestClickSecondThoughtOpensItsOwnContent(t *testing.T) {
	for _, withIDs := range []bool{false, true} {
		t.Run(fmt.Sprint(withIDs), func(t *testing.T) {
			m := astraModel(t, 100, 30)
			first := historyCell{kind: "thinking", text: "FIRST_THOUGHT", status: "completed"}
			second := historyCell{kind: "thinking", text: "SECOND_THOUGHT", status: "completed"}
			if withIDs {
				first.messageID = "reasoning:turn1"
				second.messageID = "reasoning:turn2"
			}
			m.items = []historyCell{first, {kind: "assistant", text: "first reply"}, {kind: "user", text: "second request"}, second}
			m.layout()
			var targets []clickTarget
			for _, target := range m.clickTargets {
				if target.kind == "thought" {
					targets = append(targets, target)
				}
			}
			if len(targets) != 2 || targets[0].id == targets[1].id {
				t.Fatalf("aliased thought targets: %+v", targets)
			}
			y := presentationHeight(m.headerPresentation(), m.width) + targets[1].line - m.viewport.YOffset
			clickAt(&m, 2, y)
			got := stripANSI(m.renderDetailPopover())
			if !strings.Contains(got, "SECOND_THOUGHT") || strings.Contains(got, "FIRST_THOUGHT") || m.detailRow != y {
				t.Fatalf("wrong thought at row %d: %s", m.detailRow, got)
			}
			m.items[3].text = "SECOND_THOUGHT_UPDATED"
			if got = m.renderDetailPopover(); !strings.Contains(got, "SECOND_THOUGHT_UPDATED") {
				t.Fatalf("refresh used wrong thought: %s", got)
			}
			m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
			y = presentationHeight(m.headerPresentation(), m.width) + targets[0].line - m.viewport.YOffset
			clickAt(&m, 2, y)
			if got = m.renderDetailPopover(); !strings.Contains(got, "FIRST_THOUGHT") || strings.Contains(got, "SECOND_THOUGHT") {
				t.Fatalf("first thought mismatch: %s", got)
			}
		})
	}
}
