package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"ccdp/internal/protocol"
)

func (s *SessionSupervisor) close() {
	for _, r := range s.directory.CloseAdmission() {
		r.mu.Lock()
		if r.cancel != nil {
			r.cancel()
		}
		r.mu.Unlock()
	}
	_ = s.directory.Wait(context.Background(), true)
}

// revokeCapabilities cancels every descendant run owned by this supervisor
// and joins its child Agents before a parent capability revocation commits.
// A child may outlive the parent tool step (notify mode) while retaining the
// frozen capability ceiling from that step, so waiting only for the parent
// turn is not sufficient.
func (s *SessionSupervisor) revokeCapabilities(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	runs, _ := s.directory.StopAdmission(false)
	for _, run := range runs {
		run.mu.Lock()
		if run.cancel != nil {
			run.cancel()
		}
		run.mu.Unlock()
	}

	var revokeErr error
	// The execution owner finalizes each cancelled child, including its outbox.
	// Closing a child here could release its writer before FinishRun admission.
	if err := s.directory.Wait(ctx, false); err != nil {
		revokeErr = errors.Join(revokeErr, fmt.Errorf("wait for child runs: %w", err))
	}
	if revokeErr != nil {
		return revokeErr
	}
	s.directory.ReopenAdmission()
	return nil
}

func (s *SessionSupervisor) stopTree(ctx context.Context, id protocol.CommandID) error {
	runs, err := s.directory.StopAdmission(true)
	if err != nil {
		return err
	}
	for _, r := range runs {
		r.mu.Lock()
		if r.cancel != nil {
			r.cancel()
		}
		r.mu.Unlock()
	}
	defer s.directory.ReopenAdmission()
	receipt, err := s.root.Submit(ctx, protocol.Command{ID: id, SessionID: protocol.SessionID(s.root.SessionID()), Type: protocol.CommandStop})
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

// WaitChildren lets headless clients keep the application alive until explicit
// background work settles. Observing a terminal page does not call this.
func (a *Agent) WaitChildren(ctx context.Context) error {
	if a.services.supervisor == nil {
		return nil
	}
	s := a.services.supervisor
	for {
		runs := s.directory.Children()
		active := false
		for _, r := range runs {
			select {
			case <-r.done:
			default:
				active = true
				select {
				case <-r.done:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
		if !active {
			return nil
		}
	}
}
