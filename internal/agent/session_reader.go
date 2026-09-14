package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

type managedClient struct {
	supervisor *SessionSupervisor
	run        *managedRun
	id         protocol.SessionID
	initialRun protocol.RunID
}

func (c *managedClient) current() *managedRun {
	r, err := c.supervisor.lookup(c.id)
	if err == nil {
		return r
	}
	return c.run
}

func (c *managedClient) Snapshot(ctx context.Context) (protocol.SessionView, error) {
	if err := ctx.Err(); err != nil {
		return protocol.SessionView{}, err
	}
	r := c.current()
	r.mu.Lock()
	if r.agent != nil {
		view, err := r.agent.Snapshot(ctx)
		view.RunID = r.fact.Child.Run.ID
		r.mu.Unlock()
		return view, err
	}
	view := r.final
	row := r.fact.Child
	r.mu.Unlock()
	if view.SessionID != "" {
		view.RunID = row.Run.ID
		data, _ := json.Marshal(view)
		var clone protocol.SessionView
		_ = json.Unmarshal(data, &clone)
		return clone, nil
	}
	view = protocol.SessionView{SessionID: row.SessionID, RunID: row.Run.ID, Busy: row.Run.Active(), Phase: protocol.PhaseIdle, Usage: row.Run.Usage}
	if row.Run.Active() {
		view.Phase = protocol.PhasePreparing
	}
	page, err := c.supervisor.ReadTranscript(ctx, row.SessionID, 0, transcriptWindow)
	if err != nil && !errors.Is(err, os.ErrNotExist) && row.Run.Status != "queued" && row.Run.Status != "starting" {
		return view, err
	}
	view.Transcript, view.TranscriptMore = page.Items, page.More
	return view, nil
}

func (c *managedClient) Watch(ctx context.Context, cursor protocol.Cursor) (protocol.Subscription, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	view, err := c.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	life, cancel := context.WithCancel(ctx)
	sub := &managedSubscription{ch: make(chan protocol.Update, 64), cancel: cancel, done: make(chan struct{})}
	sub.ch <- protocol.Update{Type: protocol.UpdateSnapshot, Snapshot: &view}
	go c.observe(life, sub)
	return sub, nil
}

// Observation follows the session across queued, running and dormant states.
// It owns only its subscriptions; stopping it cannot close or cancel an Agent.
func (c *managedClient) observe(ctx context.Context, out *managedSubscription) {
	defer close(out.done)
	defer close(out.ch)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var lastRun protocol.RunID
	var lastStatus string
	var live protocol.Subscription
	var updates <-chan protocol.Update
	var attached *Agent
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
		r := c.current()
		r.mu.Lock()
		a, row := r.agent, r.fact.Child
		r.mu.Unlock()
		if a != attached || row.Run.ID != lastRun {
			if live != nil {
				_ = live.Close()
				live = nil
				updates = nil
			}
			attached = a
			if a != nil {
				var err error
				live, err = a.Watch(ctx, protocol.Cursor{})
				if err == nil {
					updates = live.Updates()
				} else {
					attached = nil
				}
			}
		}
		if lastRun != row.Run.ID || lastStatus != row.Run.Status {
			snapshot, err := c.Snapshot(ctx)
			if err != nil {
				return
			}
			if !send(protocol.Update{Type: protocol.UpdateSnapshot, Snapshot: &snapshot, Revision: snapshot.Revision}) {
				return
			}
			lastRun, lastStatus = row.Run.ID, row.Run.Status
		}
		select {
		case <-ctx.Done():
			return
		case <-c.supervisor.root.rootCtx.Done():
			return
		case <-ticker.C:
		case update, ok := <-updates:
			if !ok {
				live = nil
				updates = nil
				attached = nil
				continue
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

func (c *managedClient) Submit(ctx context.Context, cmd protocol.Command) (protocol.Receipt, error) {
	r := c.current()
	r.mu.Lock()
	defer r.mu.Unlock()
	row := r.fact.Child
	if cmd.SessionID != row.SessionID {
		return protocol.Receipt{}, errors.New("command target differs from viewed session")
	}
	expected := cmd.ExpectedRunID
	if expected == "" {
		expected = c.initialRun
	}
	if expected != row.Run.ID {
		return protocol.Receipt{}, errors.New("stale run id")
	}
	if !row.Run.Active() || row.Run.Status == "settling" {
		return protocol.Receipt{}, errors.New("run settled; use continue")
	}
	switch cmd.Type {
	case protocol.CommandSubmitInput:
		if row.Purpose != childPurposeTask {
			return protocol.Receipt{}, errors.New("guardian is read-only")
		}
	case protocol.CommandApproveTool:
		if r.opts.NonInteractive {
			return protocol.Receipt{}, errors.New("approval channel unavailable")
		}
	case protocol.CommandInterrupt:
	case protocol.CommandStop:
		r.cancel()
		return protocol.Receipt{CommandID: cmd.ID, SessionID: row.SessionID, Status: protocol.ReceiptApplied}, nil
	default:
		return protocol.Receipt{}, errors.New("this operation is unavailable in a child view; return to the root")
	}
	if r.agent == nil {
		return protocol.Receipt{}, errors.New("child has not started")
	}
	// Preserve the runtime's input identity, scheduled/applied state, revision
	// and rejection. Never manufacture a successful receipt for a queued input.
	return r.agent.Submit(ctx, cmd)
}

type managedSubscription struct {
	ch     chan protocol.Update
	cancel context.CancelFunc
	done   chan struct{}
}

func (s *managedSubscription) Updates() <-chan protocol.Update { return s.ch }
func (s *managedSubscription) Close() error                    { s.cancel(); <-s.done; return nil }

func (s *SessionSupervisor) ReadTranscript(ctx context.Context, id protocol.SessionID, before uint64, limit int) (protocol.TranscriptPage, error) {
	if limit <= 0 || limit > transcriptWindow {
		limit = transcriptWindow
	}
	records, _, err := s.readSessionRecords(ctx, id)
	if err != nil {
		return protocol.TranscriptPage{}, err
	}
	// Keep only the requested page's text, not an unbounded text projection.
	// The compact identity index preserves stable pagination when later facts
	// complete tools that started before this page.
	t := &transcriptState{index: make(map[string]uint64), before: before, window: limit}
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return protocol.TranscriptPage{}, err
		}
		t.facts([]session.Event{record.Event})
	}
	items, _ := t.snapshot()
	var cursor uint64
	if len(items) > 0 {
		cursor = t.index[items[0].ID]
	}
	return protocol.TranscriptPage{Items: items, Before: cursor, More: cursor > 1}, nil
}
