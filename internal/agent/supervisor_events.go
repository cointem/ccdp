package agent

import "ccdp/internal/protocol"

func (s *SessionSupervisor) publishChild(row protocol.ChildSession, event *protocol.Update) {
	if event != nil {
		if event.Event == nil && event.Type != protocol.UpdateResyncRequired {
			return
		}
		copy := *event
		copy.Snapshot, copy.Child = nil, nil
		event = &copy
	}
	root := s.root
	root.watchMu.Lock()
	defer root.watchMu.Unlock()
	update := protocol.Update{Type: protocol.UpdateChild, Child: &protocol.ChildUpdate{Session: row, Update: event}}
	root.stampUpdateLocked(&update)
	for _, w := range root.watchers {
		root.sendWatcherLocked(w, update)
	}
}

func (s *SessionSupervisor) publishChildOutcome(r *managedRun) {
	r.mu.Lock()
	row := r.fact.Child
	r.mu.Unlock()
	s.publishChild(row, nil)
	r.parent.publishState()
}
