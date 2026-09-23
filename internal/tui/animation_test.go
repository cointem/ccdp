package tui

import (
	"github.com/charmbracelet/lipgloss"
	"strings"
	"testing"
)

func TestRunningAnimationChangesOnlyMarkers(t *testing.T) {
	for _, phase := range []ActivityPhase{ActivityPreparing, ActivityWaitingResponse, ActivityStreaming, ActivityRunningTool, ActivityIdle} {
		t.Run(string(phase), func(t *testing.T) {
			m := astraModel(t, 80, 24)
			m.busy = true
			m.activity = Activity{Phase: phase}
			m.items = []historyCell{{kind: "thinking", status: "streaming", text: "live content"}, {kind: "tool", toolID: "run", toolName: "Bash", status: "running", text: "working"}}
			m.layout()
			before := stripANSI(m.View())
			h, offset := m.viewport.Height, m.viewport.YOffset
			next, _ := m.Update(m.spinner.Tick())
			m = modelValue(t, next)
			after := stripANSI(m.View())
			if before == after {
				t.Fatal("running frame did not animate")
			}
			if lipgloss.Height(before) != lipgloss.Height(after) || lipgloss.Width(before) != lipgloss.Width(after) || m.viewport.Height != h || m.viewport.YOffset != offset {
				t.Fatal("animation changed geometry")
			}
			for _, text := range []string{"Thinking…", "live content", "Running", "working"} {
				if !strings.Contains(after, text) {
					t.Fatalf("animation changed content %q", text)
				}
			}
		})
	}
}

func TestSettledAndDecisionStatesDoNotAnimate(t *testing.T) {
	for _, phase := range []ActivityPhase{ActivityCompleted, ActivityFailed, ActivityCancelled, ActivityWaitingApproval, ActivityWaitingQuestion} {
		m := astraModel(t, 80, 24)
		m.activity = Activity{Phase: phase}
		if m.animateWork() {
			t.Fatalf("%s should be static", phase)
		}
		before := m.renderStatus()
		m.spinner, _ = m.spinner.Update(m.spinner.Tick())
		if before != m.renderStatus() {
			t.Fatalf("%s animated", phase)
		}
	}
}
