package tui

import (
	"strings"
	"testing"

	"ccdp/internal/protocol"
)

func TestChildQuestionAndPlanRemainOwnedByChild(t *testing.T) {
	for _, kind := range []string{"question", "plan"} {
		t.Run(kind, func(t *testing.T) {
			m, d := routingTestModel()
			child := protocol.ChildSession{SessionID: "child", ParentSessionID: "root", Run: protocol.RunView{ID: "child-run", Status: "waiting_" + kind}}
			if kind == "question" {
				child.Question = &protocol.QuestionRequest{ID: "question", Questions: []protocol.Question{{Question: "Which variant?", Header: "Variant", Options: []protocol.QuestionOption{{Label: "One"}, {Label: "Two"}}}}}
			} else {
				child.Plan = &protocol.PlanView{ID: "plan", Version: 1, Text: "Implement selected variant"}
			}
			m.applyChildUpdate(child)
			if pending := m.pendingChildInteraction(); pending == nil || pending.SessionID != "child" {
				t.Fatalf("pending owner: %+v", pending)
			}
			if !strings.Contains(m.pendingDecisionSummary(), "1 pending") {
				t.Fatalf("missing pending reminder: %s", m.pendingDecisionSummary())
			}
			if label := childInteractionLabel(child); strings.Contains(label, "approval") {
				t.Fatalf("%s mislabeled as approval", kind)
			}
			if m.question != nil || m.approval != nil {
				t.Fatal("background interaction replaced parent's foreground")
			}
			// /pending selects the child; its own snapshot supplies the foreground interaction.
			cmd := m.openPendingDecision()
			if cmd == nil {
				t.Fatal("pending interaction did not select child")
			}
			if len(d.clients["root"].submits) != 0 {
				t.Fatal("parent received child's decision")
			}
			child.Run.Status = "succeeded"
			m.applyChildUpdate(child)
			if m.pendingChildInteraction() != nil {
				t.Fatal("completed child kept pending interaction")
			}
		})
	}
}
