package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"ccdp/internal/protocol"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func TestAgentShelfDropsMissingRows(t *testing.T) {
	m, _ := routingTestModel()
	m.syncAgentShelf()
	if len(m.routing.agentShelf) == 0 {
		t.Fatal("fixture has no shelf")
	}
	m.routing.rows = nil
	m.syncAgentShelf()
	if len(m.routing.agentShelf) != 0 {
		t.Fatal("stale shelf keys retained")
	}
}

func TestAgentShelfLifetimeAndRetainedReader(t *testing.T) {
	clock := time.Now()
	oldNow := now
	now = func() time.Time { return clock }
	t.Cleanup(func() { now = oldNow })
	m, _ := routingTestModel()
	t.Cleanup(func() {
		if m.watchCancel != nil {
			m.watchCancel()
		}
	})
	row := m.routing.rows[0]
	row.Title = "REVIEW_CHILD"
	m.applyChildUpdate(row)
	clock = clock.Add(time.Hour)
	if !strings.Contains(m.renderAgentShelf(), row.Title) {
		t.Fatal("running child expired")
	}
	row.Run.Status, row.Run.FinishedAt = "succeeded", clock
	m.applyChildUpdate(row)
	clock = clock.Add(29 * time.Second)
	if !strings.Contains(m.renderAgentShelf(), row.Title) {
		t.Fatal("completion grace missing")
	}
	m = switchRoutingTest(t, m, "child")
	clock = clock.Add(time.Minute)
	m.syncAgentShelf()
	if !strings.Contains(m.renderAgentShelf(), row.Title) {
		t.Fatal("viewed child expired")
	}
	m = switchRoutingTest(t, m, "root")
	clock = clock.Add(29 * time.Second)
	if !strings.Contains(m.renderAgentShelf(), row.Title) {
		t.Fatal("leaving reader did not renew grace")
	}
	clock = clock.Add(2 * time.Second)
	next, _ := m.Update(spinner.TickMsg{})
	m = modelValue(t, next)
	if m.renderAgentShelf() != "" {
		t.Fatal("idle completion failed to expire")
	}
	if len(m.routing.rows) != 1 || m.openAgentView("child") == nil {
		t.Fatal("expiry removed saved reader")
	}
}

func TestAgentShelfClicksAndDismissAtDifferentWidths(t *testing.T) {
	for _, width := range []int{20, 40, 100} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			m, _ := routingTestModel()
			t.Cleanup(m.watchCancel)
			m.width = width
			row := m.routing.rows[0]
			row.Title = "审查很长的中文任务描述 wrapping must not break clicks"
			row.Run.Status, row.Run.FinishedAt = "failed", now()
			m.applyChildUpdate(row)
			m.layout()
			for _, line := range strings.Split(m.renderAgentShelf(), "\n") {
				if lipgloss.Width(line) > width {
					t.Fatal("shelf overflow")
				}
			}
			frame := strings.Split(sanitizeANSI(m.View()), "\n")
			y := len(frame) - 1
			if !strings.Contains(frame[y], "[×]") {
				t.Fatalf("shelf not at bottom: %s", m.View())
			}
			_, cmd := m.handleMouse(tea.MouseMsg{X: 3, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
			if cmd == nil {
				t.Fatal("no reader click")
			}
			opened, ok := cmd().(agentViewOpenedMsg)
			if !ok || opened.err != nil || opened.snapshot.SessionID != "child" {
				t.Fatalf("wrong reader: %+v", opened)
			}
			m.handleMouse(tea.MouseMsg{X: width - 3, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
			m.syncAgentShelf()
			if m.renderAgentShelf() != "" || len(m.routing.rows) != 1 {
				t.Fatal("dismiss did not hide only the shelf row")
			}
			// A fresh run in the same session must reappear.
			row.Run.ID, row.Run.Status = "next-run", "running"
			m.applyChildUpdate(row)
			if m.renderAgentShelf() == "" {
				t.Fatal("new run remained dismissed")
			}
		})
	}
}

func TestAgentShelfOverflowPrioritizesApprovalAndKeepsHistory(t *testing.T) {
	m, _ := routingTestModel()
	t.Cleanup(m.watchCancel)
	m.routing.rows = nil
	for i := 0; i < 10; i++ {
		row := protocol.ChildSession{SessionID: protocol.SessionID(fmt.Sprint(i)), Title: fmt.Sprintf("child-%d", i), Run: protocol.RunView{ID: protocol.RunID(fmt.Sprint(i)), Status: "running"}}
		if i == 9 {
			row.Run.Status = "waiting_approval"
			row.Approval = &protocol.ApprovalView{ID: "approval"}
		}
		m.applyChildUpdate(row)
	}
	lines := m.agentShelfLines()
	if len(lines) != 5 || lines[1].target != "9" || lines[4].target != "" {
		t.Fatalf("unbounded or incorrect priority: %+v", lines)
	}
	if handled, cmd := m.clickAgentShelf(3, m.height-1); !handled || m.picker == nil {
		t.Fatalf("overflow did not open directory: %v", cmd)
	}
	if len(m.routing.rows) != 10 {
		t.Fatal("overflow removed sessions")
	}
}
