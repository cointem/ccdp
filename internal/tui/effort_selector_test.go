package tui

import (
	"strings"
	"testing"

	"ccdp/internal/protocol"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func effortTestModel(t *testing.T) *Model {
	t.Helper()
	m := sugModel()
	m.width, m.height = 80, 24
	m.snapshot.Settings.ReasoningEffort = "medium"
	m.hasSnapshot = true
	next, _ := m.runCommand("/effort")
	v := modelValue(t, next)
	*m = v
	return m
}
func TestEffortHorizontalPreviewAnimationAndCommit(t *testing.T) {
	m := effortTestModel(t)
	client := m.client.(*recordingClient)
	if m.picker == nil || m.picker.action.Kind != selectorEffort || m.picker.Options[m.picker.Index].ID != "medium" {
		t.Fatal("did not open at confirmed effort")
	}
	initial := m.picker.Index
	m.handlePickerKey(tea.KeyMsg{Type: tea.KeyDown})
	if m.picker.Index != initial {
		t.Fatal("effort incorrectly uses vertical navigation")
	}
	_, cmd := m.handlePickerKey(tea.KeyMsg{Type: tea.KeyRight})
	if cmd == nil || m.picker.Options[m.picker.Index].ID != "high" {
		t.Fatal("right did not preview high")
	}
	if m.snapshot.Settings.ReasoningEffort != "medium" || client.submitCount() != 0 {
		t.Fatal("preview mutated effective settings")
	}
	firstFrame := sanitizeANSI(m.renderEffortSelector())
	token := m.picker.effortMotion.token
	for i := 0; i < effortMotionFrames; i++ {
		next, _ := m.Update(effortTickMsg{token: token})
		v := modelValue(t, next)
		*m = v
	}
	if firstFrame == sanitizeANSI(m.renderEffortSelector()) {
		t.Fatal("selection did not animate")
	}
	if m.advanceEffortMotion(effortTickMsg{token: token}) != nil {
		t.Fatal("animation did not stop")
	}
	_, commit := m.handlePickerKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.picker != nil || commit == nil {
		t.Fatal("Enter did not submit selection")
	}
	_ = commit()
	sent := client.submits[len(client.submits)-1]
	if sent.Type != protocol.CommandSetGeneration || sent.Generation.ReasoningEffort == nil || *sent.Generation.ReasoningEffort != "high" {
		t.Fatalf("wrong generation command: %+v", sent)
	}
	if m.snapshot.Settings.ReasoningEffort != "medium" {
		t.Fatal("unconfirmed setting leaked into footer")
	}
	if m.advanceEffortMotion(effortTickMsg{token: token}) != nil {
		t.Fatal("late animation reopened picker")
	}
}
func TestEffortCancelResetAndStaleAnimation(t *testing.T) {
	m := effortTestModel(t)
	m.textarea.SetValue("keep draft")
	m.handlePickerKey(tea.KeyMsg{Type: tea.KeyLeft})
	token := m.picker.effortMotion.token
	m.handlePickerKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.textarea.Value() != "keep draft" || m.client.(*recordingClient).submitCount() != 0 {
		t.Fatal("cancel lost draft or submitted")
	}
	m.runCommand("/effort")
	if m.advanceEffortMotion(effortTickMsg{token: token}) != nil {
		t.Fatal("old animation affected reopened picker")
	}
	m.handlePickerKey(tea.KeyMsg{Type: tea.KeyHome})
	_, cmd := m.handlePickerKey(tea.KeyMsg{Type: tea.KeyEnter})
	_ = cmd()
	sent := m.client.(*recordingClient).submits[0]
	if sent.Generation.ReasoningEffort == nil || *sent.Generation.ReasoningEffort != "none" {
		t.Fatal("Home did not select none")
	}
}
func TestEffortFramesFitAndOtherSelectorsStayVertical(t *testing.T) {
	for _, size := range [][2]int{{24, 12}, {40, 16}, {80, 24}, {120, 40}} {
		m := effortTestModel(t)
		m.width, m.height = size[0], size[1]
		m.layout()
		for i := range effortOptions {
			m.picker.Index = i
			frame := sanitizeANSI(m.View())
			if lipgloss.Width(frame) > size[0] || lipgloss.Height(frame) > size[1] {
				t.Fatalf("overflow at %v option %d", size, i)
			}
			if !strings.Contains(frame, effortOptions[i].ID) || !strings.Contains(frame, "Esc") {
				t.Fatalf("selection/exit hidden at %v: %s", size, frame)
			}
		}
	}
	m := effortTestModel(t)
	m.runCommand("/verbosity")
	old := m.picker.Index
	m.handlePickerKey(tea.KeyMsg{Type: tea.KeyRight})
	if m.picker.Index != old {
		t.Fatal("verbosity became horizontal")
	}
	m.handlePickerKey(tea.KeyMsg{Type: tea.KeyDown})
	if m.picker.Index != old+1 {
		t.Fatal("verbosity lost vertical navigation")
	}
}
