package agent

import (
	"ccdp/internal/protocol"
)

// Work projection is updated from the same child facts as the observable stream.
// Its independent lock never calls Agent or supervisor methods; status snapshots
// can read the atomic count while holding Agent.mu without reversing lock order.
func (s *SessionSupervisor) projectOutstandingWork(row protocol.ChildSession) {
	s.directory.ProjectWork(row)
}

func (s *SessionSupervisor) publishChild(row protocol.ChildSession, event *protocol.Update, parents ...childParent) {
	s.projectOutstandingWork(row)
	for _, parent := range parents {
		if parent != nil && parent.SessionID() != s.root.SessionID() {
			parent.signalChildActivity()
		}
	}
	s.root.signalChildActivity()
	if event != nil {
		if event.Event == nil && event.Type != protocol.UpdateResyncRequired {
			return
		}
		copy := *event
		copy.Snapshot, copy.State, copy.Child = nil, nil, nil
		event = &copy
	}
	update := protocol.Update{Type: protocol.UpdateChild, Child: &protocol.ChildUpdate{Session: row, Update: event}}
	s.root.publishChildUpdate(update)
}

func (s *SessionSupervisor) publishChildOutcome(r *managedRun) {
	// A delivery receipt for the previous run can race a follow-up. Serialize
	// projection with admission so the old result never replaces the new card.
	s.publicationMu.Lock()
	r.mu.Lock()
	if s.directory.Current(r.fact.Child.SessionID) != r {
		r.mu.Unlock()
		s.publicationMu.Unlock()
		return
	}
	row := s.childRowLocked(r)
	r.mu.Unlock()
	s.publishChild(row, nil, r.parent)
	s.publicationMu.Unlock()
	r.parent.publishState()
}
