package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

// Reconcile the child's durable outbox with the parent's catalog. This is a
// read-only pass: constructing an observer never wakes a model or runs a tool.
func (s *SessionSupervisor) readChildOutboxes() {
	for _, r := range s.runs {
		if r.fact.UsageAccounted {
			continue
		}
		// An uncompleted run is reported as uncertain, never restarted. A
		// discovered terminal child outbox below replaces this synthetic result.
		if r.fact.Child.Run.Status == "unknown" {
			r.fact.DeliveryID = "child-result-" + string(r.fact.Child.Run.ID)
		}
		store, err := session.OpenJSONLReadOnly(s.sessionDir, string(r.fact.Child.SessionID))
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				r.fact.Child.Run.Error = fmt.Sprintf("child history unavailable: %v", err)
				r.err = err
			}
			continue
		} // a queued child may never have been constructed
		records, err := store.Read(session.Beginning)
		_ = store.Close()
		if err != nil {
			r.fact.Child.Run.Error = fmt.Sprintf("child history unavailable: %v", err)
			continue
		}
		found := false
		for _, record := range records {
			fact, ok := record.Event.(*session.ChildRunRecorded)
			if ok && fact.Child.Run.ID == r.fact.Child.Run.ID && fact.Child.SessionID == r.fact.Child.SessionID && fact.Child.ParentSessionID == r.fact.Child.ParentSessionID && !fact.Child.Run.Active() {
				r.fact = *fact
				found = true
			}
		}
		if !found {
			if snapshot, err := snapshotFromRecords(records, string(r.fact.Child.SessionID)); err == nil {
				u, b := snapshot.Usage, r.fact.UsageBaseline
				r.fact.Child.Run.Usage = protocol.UsageSnapshot{InputTokens: max(0, u.InputTokens-b.InputTokens), OutputTokens: max(0, u.OutputTokens-b.OutputTokens), CachedTokens: max(0, u.CachedTokens-b.CachedTokens), Cost: max(0, u.Cost-b.Cost), TurnCount: max(0, u.TurnCount-b.TurnCount)}
			}
		}
	}
}

// Execution startup, not observation, owns recovery delivery. Every retry
// uses the original DeliveryID; parent command admission persists deduplication.
func (s *SessionSupervisor) startRecovery() {
	s.recoverOnce.Do(func() {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return
		}
		s.wg.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.wg.Done()
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				s.mu.Lock()
				runs := make([]*managedRun, 0, len(s.runs))
				for _, r := range s.runs {
					runs = append(runs, r)
				}
				s.mu.Unlock()
				for _, r := range runs {
					if s.root.rootCtx.Err() != nil {
						return
					}
					select {
					case <-r.done:
					default:
						continue
					}
					r.mu.Lock()
					if r.fact.DeliveryID != "" && !r.fact.UsageAccounted {
						if err := s.recordOutcome(r); err != nil {
							r.err = err
						}
					}
					r.mu.Unlock()
					s.deliver(r)
				}
				select {
				case <-s.root.rootCtx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	})
}

// WaitChildren lets headless clients keep the application alive until explicit
// background work settles. Observing a terminal page does not call this.
func (a *Agent) WaitChildren(ctx context.Context) error {
	if a.supervisor == nil {
		return nil
	}
	s := a.supervisor
	for {
		s.mu.Lock()
		runs := make([]*managedRun, 0, len(s.children))
		for _, r := range s.children {
			runs = append(runs, r)
		}
		s.mu.Unlock()
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
