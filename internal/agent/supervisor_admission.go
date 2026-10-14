package agent

func (s *SessionSupervisor) reserveRun(r, previous *managedRun) error {
	s.publicationMu.Lock()
	defer s.publicationMu.Unlock()
	if err := s.directory.Reserve(r.fact.Child, r, previous); err != nil {
		return err
	}
	if previous != nil {
		previous.mu.Lock()
		previous.handoff = make(chan struct{})
		previous.mu.Unlock()
	}
	return nil
}
func (s *SessionSupervisor) finishRunAdmission(r *managedRun, err error) {
	s.publicationMu.Lock()
	defer s.publicationMu.Unlock()
	previous := s.directory.Current(r.fact.Child.SessionID)
	s.directory.FinishAdmission(r.fact.Child.SessionID, r, err)
	if previous != nil {
		previous.mu.Lock()
		if previous.handoff != nil {
			close(previous.handoff)
			previous.handoff = nil
		}
		previous.mu.Unlock()
	}
	if err == nil {
		r.mu.Lock()
		row := s.childRowLocked(r)
		r.mu.Unlock()
		s.publishChild(row, nil, r.parent)
	}

}
