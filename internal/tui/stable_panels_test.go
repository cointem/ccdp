package tui

import (
	"fmt"
	tea "github.com/charmbracelet/bubbletea"
	"strings"
	"testing"
)

func TestTransientPanelsPreserveBaseFrame(t *testing.T) {
	panels := map[string]func(*Model){
		"approval": func(m *Model) { m.deferredApproval = "" },
		"question": func(m *Model) { m.question.deferred = false },
		"model": func(m *Model) {
			m.startSelectorAt("Model", []SelectorOption{{ID: "one", Label: "one"}, {ID: "two", Label: "two"}}, 0, true, selectorAction{Kind: selectorModel})
		},
		"permission": func(m *Model) {
			m.startSelectorAt("Permission", []SelectorOption{{ID: "edits", Label: "edits"}}, 0, true, selectorAction{Kind: selectorMode})
		},
		"effort":  func(m *Model) { m.startGenerationSelector("effort") },
		"context": func(m *Model) { m.showContextDetail = true },
		"tasks":   func(m *Model) { m.snapshot.Tasks = sampleTasks(); m.tasksVisible = true },
		"history": func(m *Model) { m.history = []string{"previous input"}; m.startHistorySearch() },
		"exit":    func(m *Model) { m.exitConfirm = true },
		"tool":    func(m *Model) { m.toggleToolExpanded("tool") },
		"thought": func(m *Model) { m.toggleToolExpanded("thought") },
	}
	for _, width := range []int{24, 40, 80, 120, 200} {
		for name, open := range panels {
			t.Run(fmt.Sprintf("%d/%s", width, name), func(t *testing.T) {
				m := astraModel(t, width, 30)
				m.snapshot.Tasks = sampleTasks()
				if name == "approval" {
					m.approval = &approvalPrompt{ID: "decision", Tool: "Bash", Command: "command"}
					m.deferredApproval = m.approvalKey()
				}
				if name == "question" {
					m.setQuestion(uiQuestionRequest())
					m.question.deferred = true
				}
				m.items = []historyCell{{kind: "tool", toolID: "tool", toolName: "Bash", text: strings.Repeat("output\n", 50)}, {kind: "thinking", toolID: "thought", text: strings.Repeat("thought\n", 50)}}
				for i := 0; i < 50; i++ {
					m.items = append(m.items, historyCell{kind: "assistant", text: fmt.Sprintf("record %d", i)})
				}
				m.pushStatus("existing status")
				m.textarea.SetValue("draft\nsecond line")
				m.layout()
				m.followOutput = false
				m.viewport.SetYOffset(3)
				before := stripANSI(m.View())
				h, offset := m.viewport.Height, m.viewport.YOffset
				open(&m)
				m.syncViewportHeight()
				if m.viewport.Height != h || m.viewport.YOffset != offset {
					t.Fatal("panel moved transcript")
				}
				after := strings.Split(stripANSI(m.View()), "\n")
				base := strings.Split(before, "\n")
				chrome := m.composeChrome()
				bottom := m.height - presentationHeight(chrome.input, width) - presentationHeight(chrome.footer, width)
				if !m.composerVisible() {
					bottom = m.height - presentationHeight(chrome.footer, width)
				}
				top := max(0, bottom-presentationHeight(chrome.surface, width))
				if m.detail != nil {
					top = min(m.detailRow, bottom)
				}
				for i := 0; i < top; i++ {
					if after[i] != base[i] {
						t.Fatalf("uncovered row %d changed", i)
					}
				}
				m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
				m.syncViewportHeight()
				if got := stripANSI(m.View()); got != before {
					t.Fatalf("closing changed underlying frame:\nbefore=%s\nafter=%s", before, got)
				}
			})
		}
	}
}

func TestDetailMouseScrollDoesNotScrollTranscript(t *testing.T) {
	for _, kind := range []string{"tool", "thinking"} {
		t.Run(kind, func(t *testing.T) {
			m := astraModel(t, 80, 24)
			m.items = []historyCell{{kind: kind, toolID: "detail", toolName: "Bash", text: strings.Repeat("detail line\n", 80)}}
			m.layout()
			before := stripANSI(m.View())
			offset, height := m.viewport.YOffset, m.viewport.Height
			target := m.clickTargets[0]
			clickAt(&m, 2, presentationHeight(m.headerPresentation(), m.width)+target.line-offset)
			if m.detail == nil {
				t.Fatal("click did not open details")
			}
			m.handleMouse(tea.MouseMsg{Button: tea.MouseButtonWheelDown})
			if m.detail.offset == 0 {
				t.Fatal("detail did not scroll")
			}
			if m.viewport.YOffset != offset || m.viewport.Height != height {
				t.Fatal("background scrolled")
			}
			m.handleMouse(tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
			if m.detail != nil || stripANSI(m.View()) != before {
				t.Fatal("close did not restore exact frame")
			}
		})
	}
}
