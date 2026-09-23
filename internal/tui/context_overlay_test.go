package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func footerTextPosition(t *testing.T, m *Model, text string) (int, int) {
	t.Helper()
	rows := presentationRows(m.composeChrome().footer, m.width)
	for i, row := range rows {
		plain := stripANSI(row)
		if index := strings.Index(plain, text); index >= 0 {
			return lipgloss.Width(plain[:index]), m.height - len(rows) + i
		}
	}
	t.Fatalf("footer does not contain %q: %s", text, m.composeChrome().footer)
	return 0, 0
}

func TestContextPopoverAnchoredAndDoesNotMoveFrame(t *testing.T) {
	for _, width := range []int{40, 80, 120, 200} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			m := astraModel(t, width, 30)
			m.snapshot.Settings.Permission.Mode = "bypassPermissions"
			m.snapshot.Settings.ReasoningEffort = "high"
			for i := 0; i < 50; i++ {
				m.items = append(m.items, historyCell{kind: "assistant", text: fmt.Sprintf("RECORD_%02d", i)})
			}
			m.layout()
			click := func(text string) {
				x, y := footerTextPosition(t, &m, text)
				m.handleMouse(tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
				m.syncViewportHeight()
			}
			click("bypass")
			if m.showContextDetail {
				t.Fatal("permission label opened context")
			}
			if m.picker == nil || m.picker.action.Kind != selectorMode {
				t.Fatal("permission label did not open mode selector")
			}
			m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
			m.syncViewportHeight()
			click("enter send")
			if m.showContextDetail {
				t.Fatal("hint opened context")
			}
			before := stripANSI(m.View())
			height, offset := m.viewport.Height, m.viewport.YOffset
			click("200k")
			if !m.showContextDetail {
				t.Fatal("context label did not open context")
			}
			if m.viewport.Height != height || m.viewport.YOffset != offset {
				t.Fatal("popover moved viewport")
			}
			after := strings.Split(stripANSI(m.View()), "\n")
			base := strings.Split(before, "\n")
			chrome := m.composeChrome()
			bottom := m.height - presentationHeight(chrome.input, width) - presentationHeight(chrome.footer, width)
			popupHeight := min(presentationHeight(chrome.surface, lipgloss.Width(chrome.surface)), bottom)
			top := bottom - popupHeight
			for i := range base {
				if (i < top || i >= bottom) && base[i] != after[i] {
					t.Fatalf("row %d moved", i)
				}
			}
			if width == 200 {
				anchor := 0
				for _, hit := range m.footerHitCells() {
					if hit.kind == "context" {
						anchor = hit.x
						break
					}
				}
				index := strings.Index(after[top], "╭")
				if index < 0 {
					t.Fatal("missing popup border")
				}
				if got := lipgloss.Width(after[top][:index]); got != anchor {
					t.Fatalf("popover x %d != anchor %d", got, anchor)
				}
			}
			m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
			m.syncViewportHeight()
			if stripANSI(m.View()) != before {
				t.Fatal("closing did not restore frame")
			}
		})
	}
}

func TestContextPopoverAnchorsOverEmptyRows(t *testing.T) {
	for _, width := range []int{80, 104, 120, 200} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			m := astraModel(t, width, 30)
			m.items = nil
			m.textarea.SetValue("")
			m.snapshot.Settings.Model.Model = "gpt-5.5"
			m.snapshot.Settings.ReasoningEffort = ""
			m.snapshot.Settings.Permission.Mode = "bypassPermissions"
			m.snapshot.ContextUsedTokens = 0
			m.layout()
			before := stripANSI(m.View())
			x, y := footerTextPosition(t, &m, "0%")
			m.handleMouse(tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
			m.syncViewportHeight()
			popupWidth := lipgloss.Width(m.renderContextDetailPopover())
			want := min(x, max(0, width-popupWidth))
			found := false
			for _, row := range strings.Split(stripANSI(m.View()), "\n") {
				if index := strings.Index(row, "╭"); index >= 0 {
					found = true
					if got := lipgloss.Width(row[:index]); got != want {
						t.Fatalf("empty row popup x=%d, want %d", got, want)
					}
				}
			}
			if !found {
				t.Fatal("popup not rendered")
			}
			m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
			m.syncViewportHeight()
			if stripANSI(m.View()) != before {
				t.Fatal("closing changed frame")
			}
		})
	}
}

func TestPermissionFooterClickOpensCurrentMode(t *testing.T) {
	for _, tc := range []struct{ mode, label string }{{"default", "manual"}, {"acceptEdits", "edits"}, {"bypassPermissions", "bypass"}} {
		t.Run(tc.label, func(t *testing.T) {
			m := astraModel(t, 80, 24)
			m.snapshot.Settings.Permission.Mode = tc.mode
			m.applySnapshot(m.snapshot)
			m.layout()
			x, y := footerTextPosition(t, &m, tc.label)
			m.handleMouse(tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
			if m.showContextDetail || m.picker == nil || m.picker.action.Kind != selectorMode {
				t.Fatal("wrong click target")
			}
			if len(m.picker.Options) != 4 {
				t.Fatalf("permission choices: %d", len(m.picker.Options))
			}
			if got := m.picker.Options[m.picker.Index].ID; got != tc.mode {
				t.Fatalf("selected %s instead of %s", got, tc.mode)
			}
			if m.snapshot.Settings.Permission.Mode != tc.mode {
				t.Fatal("opening selector changed permissions")
			}
		})
	}
}

func TestFooterClickTogglesAndSwitchesPanels(t *testing.T) {
	for _, width := range []int{40, 80, 120} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			m := astraModel(t, width, 30)
			m.layout()
			_, labels := m.persistentStatusContent()
			click := func(label string) {
				x, y := footerTextPosition(t, &m, label)
				m.handleMouse(tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
				m.syncViewportHeight()
			}
			for _, label := range []string{labels.model, labels.permission, "200k", "cache"} {
				click(label)
				if m.picker == nil && !m.showContextDetail {
					t.Fatalf("%s did not open", label)
				}
				click(label)
				if m.picker != nil || m.showContextDetail {
					t.Fatalf("%s did not close", label)
				}
			}
			click(labels.model)
			click(labels.permission)
			if m.picker == nil || m.picker.action.Kind != selectorMode {
				t.Fatal("did not switch to permissions")
			}
			click("200k")
			if m.picker != nil || !m.showContextDetail {
				t.Fatal("did not switch to context")
			}
			click(labels.model)
			if m.showContextDetail || m.picker == nil || m.picker.action.Kind != selectorModel {
				t.Fatal("did not switch to models")
			}
		})
	}
}
