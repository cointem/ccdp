package agent

import (
	"ccdp/internal/protocol"
	"context"
	"errors"
	"time"
)

func (s *SessionSupervisor) stopTree(ctx context.Context, id protocol.CommandID) error {
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		return errors.New("tree stop already in progress")
	}
	s.stopping = true
	for _, r := range s.children {
		r.mu.Lock()
		if r.cancel != nil {
			r.cancel()
		}
		r.mu.Unlock()
	}
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.stopping = false; s.mu.Unlock() }()
	receipt, err := s.root.Submit(ctx, protocol.Command{ID: id, SessionID: protocol.SessionID(s.root.sessionID), Type: protocol.CommandStop})
	if err != nil {
		return err
	}
	if receipt.Rejected() {
		return errors.New("root rejected tree stop")
	}
	if err := s.root.WaitChildren(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		view, err := s.root.Snapshot(ctx)
		if err != nil {
			return err
		}
		if !view.Busy {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
