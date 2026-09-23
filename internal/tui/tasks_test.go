package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"ccdp/internal/protocol"
)

func sampleTasks() []protocol.TaskView {
	return []protocol.TaskView{
		{ID: "1", Title: "Draft the gap analysis", Status: "completed"},
		{ID: "2", Title: "Wire the checklist panel", Status: "in_progress"},
		{ID: "3", Title: "Ship the honest context view", Status: "pending"},
	}
}

// ctrl+t toggles the checklist on and off without disturbing the draft.
func TestCtrlTTogglesTaskPanel(t *testing.T) {
	m := astraModel(t, 80, 24)
	m.snapshot.Tasks = sampleTasks()
	draft := m.textarea.Value()
	if m.tasksVisible {
		t.Fatal("task panel should start hidden")
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlT})
	if !m.tasksVisible {
		t.Fatal("ctrl+t did not open the task panel")
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlT})
	if m.tasksVisible {
		t.Fatal("second ctrl+t did not close the task panel")
	}
	if m.textarea.Value() != draft {
		t.Fatalf("ctrl+t disturbed the draft: %q", m.textarea.Value())
	}
}

// A hidden panel or an empty task list renders nothing so it never reserves rows.
func TestTaskPanelRendersNothingWhenHiddenOrEmpty(t *testing.T) {
	m := astraModel(t, 80, 24)
	m.snapshot.Tasks = sampleTasks()
	if got := m.renderTasks(); got != "" {
		t.Fatalf("hidden panel rendered content: %q", sanitizeANSI(got))
	}
	m.tasksVisible = true
	m.snapshot.Tasks = nil
	if got := m.renderTasks(); got != "" {
		t.Fatalf("empty task list rendered content: %q", sanitizeANSI(got))
	}
}

// The visible panel shows one glyph per status plus a completion count, and every
// row is indented to match the composer's left padding.
func TestTaskPanelRendersGlyphsAndCounts(t *testing.T) {
	m := astraModel(t, 80, 24)
	m.snapshot.Tasks = sampleTasks()
	m.tasksVisible = true
	plain := sanitizeANSI(m.renderTasks())
	for _, want := range []string{"1/3 done", "✔", "●", "○", "Draft the gap analysis"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("task panel missing %q:\n%s", want, plain)
		}
	}
	for _, line := range strings.Split(plain, "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, " ") {
			t.Fatalf("task row is flush-left, want one-cell indent: %q", line)
		}
	}
}

// A long list is capped with a "+N more" summary rather than crowding the screen.
func TestTaskPanelCapsLongLists(t *testing.T) {
	m := astraModel(t, 80, 24)
	tasks := make([]protocol.TaskView, 0, maxTaskRows+5)
	for i := 0; i < maxTaskRows+5; i++ {
		tasks = append(tasks, protocol.TaskView{ID: string(rune('a' + i)), Title: "task", Status: "pending"})
	}
	m.snapshot.Tasks = tasks
	m.tasksVisible = true
	plain := sanitizeANSI(m.renderTasks())
	if !strings.Contains(plain, "+5 more") {
		t.Fatalf("overflow summary missing:\n%s", plain)
	}
	if got := strings.Count(plain, "\n") + 1; got != maxTaskRows+2 {
		t.Fatalf("panel should render header + %d rows + summary = %d lines, got %d:\n%s",
			maxTaskRows, maxTaskRows+2, got, plain)
	}
}

// Every new composer-adjacent panel (task checklist, reverse search, @-mention
// popup) must keep the frame inside the terminal at all acceptance sizes and
// preserve the always-visible model/context triplet.
func TestPanelsStayWithinTerminal(t *testing.T) {
	scenes := map[string]func(m *Model){
		"tasks": func(m *Model) {
			m.snapshot.Tasks = sampleTasks()
			m.tasksVisible = true
		},
		"search": func(m *Model) {
			m.history = []string{"fix the footer", "add a test"}
			m.historyIdx = len(m.history)
			m.startHistorySearch()
			m.handleHistorySearchKey(runeKey("foo"))
		},
		"mention": func(m *Model) {
			m.workspace = t.TempDir()
			os.WriteFile(filepath.Join(m.workspace, "README.md"), []byte("x"), 0o644)
			m.textarea.SetValue("explain @REA")
			m.textarea.CursorEnd()
			m.refreshFileMention()
		},
	}
	for _, size := range [][2]int{{24, 12}, {40, 16}, {80, 24}, {120, 40}} {
		for name, setup := range scenes {
			m := astraModel(t, size[0], size[1])
			setup(&m)
			m.layout()
			frame := m.View()
			if w, h := lipgloss.Width(frame), lipgloss.Height(frame); w > size[0] || h > size[1] {
				t.Errorf("%s %dx%d: frame %dx%d exceeds terminal:\n%s", name, size[0], size[1], w, h, sanitizeANSI(frame))
			}
			if plain := sanitizeANSI(frame); !strings.Contains(plain, "12.3k/200k") {
				t.Errorf("%s %dx%d: frame lost the context triplet:\n%s", name, size[0], size[1], plain)
			}
		}
	}
}
