package agent

import (
	"errors"
	"fmt"
	"os"
	"time"

	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"ccdp/internal/session/archive"
)

// Reconcile the child's durable outbox with the parent's catalog. This is a
// read-only pass: constructing an observer never wakes a model or runs a tool.
func (s *SessionSupervisor) readChildOutboxes() {
	ctx := s.root.rootContext()
	for _, r := range s.directory.Runs() {
		if err := ctx.Err(); err != nil {
			s.recoveryErr = err
			return
		}
		if r.fact.UsageAccounted {
			continue
		}
		// An uncompleted run is reported as uncertain, never restarted. A
		// discovered terminal child outbox below replaces this synthetic result.
		if r.fact.Child.Run.Status == "unknown" {
			r.fact.DeliveryID = "child-result-" + string(r.fact.Child.Run.ID)
		}
		records := r.memory
		if records == nil {
			var err error
			records, _, err = s.readRecordedSessionRecords(ctx, r.fact.Child.SessionID)
			if err != nil {
				if !errors.Is(err, os.ErrNotExist) {
					r.fact.Child.Run.Error = fmt.Sprintf("child history unavailable: %v", err)
					r.err = err
				}
				continue
			} // a queued child may never have been constructed
		}
		found := false
		for _, record := range records {
			fact, ok := record.Event.(*session.ChildRunRecorded)
			if ok && fact.Child.Run.ID == r.fact.Child.Run.ID && fact.Child.SessionID == r.fact.Child.SessionID && fact.Child.ParentSessionID == r.fact.Child.ParentSessionID && !fact.Child.Run.Active() {
				pending := r.fact.PendingMessages
				r.fact = *fact
				r.fact.PendingMessages = pending
				found = true
			}
		}
		if !found {
			if snapshot, err := archive.FromRecords(records, string(r.fact.Child.SessionID)); err == nil {
				u, err := usageFromSession(snapshot.Usage)
				if err != nil {
					continue
				}
				b := r.fact.UsageBaseline
				r.fact.Child.Run.Usage = protocol.UsageSnapshot{InputTokens: max(0, u.InputTokens-b.InputTokens), OutputTokens: max(0, u.OutputTokens-b.OutputTokens), CachedTokens: max(0, u.CachedTokens-b.CachedTokens), TurnCount: max(0, u.TurnCount-b.TurnCount)}
			}
		}
	}
}

// Execution startup, not observation, owns recovery delivery. Every retry
// uses the original DeliveryID; parent command admission persists deduplication.
func (s *SessionSupervisor) startRecovery() {
	s.recoverOnce.Do(func() {
		if !s.directory.StartBackground() {
			return
		}
		go func() {
			defer s.directory.BackgroundDone()
			s.cleanupMergedWorkspaces()
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				runs := make([]*managedRun, 0, len(s.directory.Runs()))
				for _, r := range s.directory.Runs() {
					runs = append(runs, r)
				}
				for _, r := range runs {
					if s.root.rootContext().Err() != nil {
						return
					}
					select {
					case <-r.done:
					default:
						continue
					}
					r.mu.Lock()
					if r.fact.DeliveryID != "" && !r.fact.UsageAccounted {
						reply := enqueueRunOutcomeLocked(r)
						r.mu.Unlock()
						err := r.parent.waitChildRecord(reply)
						r.mu.Lock()
						if err != nil {
							r.err = err
						} else {
							r.fact.UsageAccounted = true
						}
					}
					r.mu.Unlock()
					s.deliver(r)
				}
				select {
				case <-s.root.rootContext().Done():
					return
				case <-ticker.C:
				}
			}
		}()
	})
}
