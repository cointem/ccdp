package supervision

import (
	"context"
	"time"

	"ccdp/internal/protocol"
)

// LiveReader deliberately omits Submit and lifecycle control. Observing a run
// grants no capability to cancel its execution or dispose its runtime.
type LiveReader interface {
	Watch(context.Context, protocol.Cursor) (protocol.Subscription, error)
}

type ObservedRun struct {
	ID      protocol.RunID
	Status  string
	Runtime LiveReader
}

type ObservationSource interface {
	Snapshot(context.Context) (protocol.SessionView, error)
	AttachedRun() ObservedRun
}

// Watch follows one logical child session across running and dormant states.
// Each run identity has at most one runtime attachment, as guaranteed by the
// supervisor. A new run always changes ID, even if it reuses the session.
func Watch(ctx, lifetime context.Context, source ObservationSource) (protocol.Subscription, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	view, err := source.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	life, cancel := context.WithCancel(ctx)
	sub := &observation{ch: make(chan protocol.Update, 64), cancel: cancel, done: make(chan struct{})}
	sub.ch <- protocol.Update{Type: protocol.UpdateSnapshot, Snapshot: &view}
	go sub.follow(life, lifetime, source)
	return sub, nil
}

type observation struct {
	ch     chan protocol.Update
	cancel context.CancelFunc
	done   chan struct{}
}

func (s *observation) Updates() <-chan protocol.Update { return s.ch }
func (s *observation) Close() error                    { s.cancel(); <-s.done; return nil }

func (out *observation) follow(ctx, lifetime context.Context, source ObservationSource) {
	defer close(out.done)
	defer close(out.ch)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var lastRun protocol.RunID
	var lastStatus string
	var retryAfter time.Time
	var live protocol.Subscription
	var updates <-chan protocol.Update
	var attached, snapshotAttached bool
	defer func() {
		if live != nil {
			_ = live.Close()
		}
	}()
	send := func(update protocol.Update) bool {
		if len(out.ch) >= cap(out.ch)-1 {
			out.ch <- protocol.Update{Type: protocol.UpdateResyncRequired, Cursor: update.Cursor, Revision: update.Revision}
			return false
		}
		select {
		case out.ch <- update:
			return true
		case <-ctx.Done():
			return false
		}
	}
	for {
		row := source.AttachedRun()
		available := row.Runtime != nil
		if row.ID != lastRun {
			retryAfter = time.Time{}
		}
		if (available != attached || row.ID != lastRun) && !time.Now().Before(retryAfter) {
			if live != nil {
				_ = live.Close()
				live, updates = nil, nil
			}
			attached = available
			if available {
				var err error
				live, err = row.Runtime.Watch(ctx, protocol.Cursor{})
				if err == nil {
					updates = live.Updates()
				} else {
					attached = false
					retryAfter = time.Now().Add(100 * time.Millisecond)
				}
			}
		}
		if lastRun != row.ID || lastStatus != row.Status || snapshotAttached != available {
			snapshot, err := source.Snapshot(ctx)
			if err != nil {
				return
			}
			if !send(protocol.Update{Type: protocol.UpdateSnapshot, Snapshot: &snapshot, Revision: snapshot.Revision}) {
				return
			}
			lastRun, lastStatus, snapshotAttached = row.ID, row.Status, available
		}
		select {
		case <-ctx.Done():
			return
		case <-lifetime.Done():
			return
		case <-ticker.C:
		case update, ok := <-updates:
			if !ok {
				_ = live.Close()
				live, updates, attached = nil, nil, false
				retryAfter = time.Now().Add(100 * time.Millisecond)
				continue
			}
			if update.State != nil {
				copy := *update.State
				copy.RunID = lastRun
				update.State = &copy
			}
			if update.Snapshot != nil {
				copy := *update.Snapshot
				copy.RunID = lastRun
				update.Snapshot = &copy
			}
			if !send(update) || update.Type == protocol.UpdateResyncRequired {
				return
			}
		}
	}
}
