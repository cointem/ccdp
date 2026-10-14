package agent

import (
	"encoding/json"

	"ccdp/internal/protocol"
	"ccdp/internal/tools"
)

func (s *SessionSupervisor) deliver(r *managedRun) {
	// Recovery retries use this same path. A saved outbox can precede the
	// final cleanup hooks, so it alone is not a readiness signal.
	select {
	case <-r.done:
	default:
		return
	}
	r.mu.Lock()
	fact := r.fact
	r.mu.Unlock()
	if fact.Delivered || !fact.UsageAccounted || fact.Child.Run.Active() || fact.Child.Run.WaitPolicy != "notify" {
		return
	}
	closed := r.parent.childParentClosed()
	if closed {
		return
	}
	// Cancellation stops execution, but the completed child's outcome still
	// belongs in the parent's inbox. InputMessage never starts another turn.
	cursor := agentReadCursor{Agent: fact.Child.SessionID, View: "result", Run: fact.Child.Run.ID}
	page := protocol.AgentReadPage{AgentID: fact.Child.SessionID, View: "result", Outcome: fact.Child.Run.Status}
	body := fact.Child.Run.Output
	if fact.Child.Run.Error != "" {
		body += "\nError: " + fact.Child.Run.Error
	}
	budget := r.parent.childResultBudget()
	if budget <= 0 {
		budget = tools.DefaultOutputLimit
	}
	encoded, err := encodeAgentTextPage(page, cursor, body, false, budget)
	if err != nil {
		r.parent.markPersistenceFailure(err)
		return
	}
	if err = json.Unmarshal([]byte(encoded), &page); err != nil {
		r.parent.markPersistenceFailure(err)
		return
	}
	text := protocol.EncodeCollaboration(protocol.CollaborationMessage{Kind: "final_result", AgentID: fact.Child.SessionID, Name: fact.Child.Name, Text: page.Text, Outcome: fact.Child.Run.Status, Cursor: page.NextCursor})
	cmd := protocol.NewSubmitInput(protocol.CommandID(fact.DeliveryID), fact.Child.ParentSessionID, protocol.InputID(fact.DeliveryID), text, protocol.InputMessage)
	receipt, err := r.parent.AdmitMessage(cmd)
	if err == nil && !receipt.Rejected() {
		r.mu.Lock()
		r.fact.Delivered = true
		err = s.persist(r, "delivered")
		if err != nil {
			r.fact.Delivered = false
		}
		r.mu.Unlock()
		s.publishChildOutcome(r)
	}
}
