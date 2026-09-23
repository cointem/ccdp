package tui

import (
	"fmt"
	tea "github.com/charmbracelet/bubbletea"
	"strings"
	"testing"
)

func TestOverlayRetainsBoundedDetailContents(t *testing.T) {
	for _, kind := range []string{"tool", "thinking"} {
		t.Run(kind, func(t *testing.T) {
			m := astraModel(t, 120, 60)
			lines := make([]string, 30)
			for i := range lines {
				lines[i] = fmt.Sprintf("original_line_%02d", i)
			}
			item := historyCell{kind: kind, toolID: "original", toolName: "Bash", toolArgsRaw: `{"command":"echo hello"}`, status: "success", text: strings.Join(lines, "\n")}
			m.items = []historyCell{item}
			m.layout()
			m.toggleToolExpanded("original")
			got := stripANSI(m.renderDetailPopover())
			limit := 25
			if kind == "tool" {
				limit = 20
				for _, want := range []string{"Ran", "├─ Arguments:", "└─ Output:", `{"command":"echo hello"}`} {
					if !strings.Contains(got, want) {
						t.Fatalf("missing original content %q: %s", want, got)
					}
				}
			}
			if !strings.Contains(got, "[click to collapse]") || !strings.Contains(got, lines[limit-1]) || strings.Contains(got, lines[limit]) || !strings.Contains(got, fmt.Sprintf("(%d more lines)", 30-limit)) {
				t.Fatalf("original preview boundaries changed: %s", got)
			}
			for _, unexpected := range []string{"Thought details", "Bash details", "wheel scroll", "Esc or click close"} {
				if strings.Contains(got, unexpected) {
					t.Fatalf("added detail text %q", unexpected)
				}
			}
			if m.items[0].text != item.text {
				t.Fatal("changed source")
			}
		})
	}
}

func TestLiveThoughtCannotOpenDetails(t *testing.T) {
	for _, status := range []string{"running", "streaming"} {
		m := astraModel(t, 100, 30)
		m.items = []historyCell{{kind: "thinking", toolID: "live", text: "first", status: status}}
		m.layout()
		for _, target := range m.clickTargets {
			if target.kind == "thought" {
				t.Fatal("live thought has a click target")
			}
		}
		m.toggleToolExpanded("live")
		if m.detail != nil {
			t.Fatal("live thought opened details")
		}
		before := m.View()
		m.handleMouse(tea.MouseMsg{X: 3, Y: presentationHeight(m.headerPresentation(), m.width), Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
		if m.detail != nil || m.View() != before {
			t.Fatal("click changed live thought")
		}
		m.items[0].text = "first\nsecond delta"
		m.render()
		if !strings.Contains(m.View(), "second delta") {
			t.Fatal("live text did not update inline")
		}
		m.items[0].status = "completed"
		m.render()
		if strings.Contains(m.View(), "second delta") {
			t.Fatal("completed thought did not collapse")
		}
		m.toggleToolExpanded("live")
		if m.detail == nil || !strings.Contains(m.renderDetailPopover(), "second delta") {
			t.Fatal("completed thought cannot be reopened")
		}
	}
}
