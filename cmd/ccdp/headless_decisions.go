package main

import (
	"ccdp/internal/protocol"
	"context"
	"fmt"
)

// A headless session has no human decision channel. Snapshots and events use
// the same decision handler, so replay/resync neither loses nor duplicates a
// refusal. Versioned plans are distinct decisions even when their ID is reused.
type headlessDecisionKey struct {
	kind    protocol.CommandType
	id      string
	version uint64
}

type headlessDecisions struct {
	client    protocol.SessionClient
	sessionID protocol.SessionID
	format    outputFormat
	handled   map[headlessDecisionKey]bool
}

func (d *headlessDecisions) snapshot(ctx context.Context, view protocol.SessionView) error {
	if q := view.Question; q != nil {
		key := headlessDecisionKey{kind: protocol.CommandAnswerQuestion, id: q.ID}
		if !d.handled[key] {
			if d.format == outStreamJSON {
				emitJSONLine(map[string]any{"type": "question_request", "question": q, "cancelled": true, "reason": "headless mode has no interactive answer channel"})
			}
			if err := d.submit(ctx, key, protocol.Command{Type: key.kind, Answer: &protocol.AnswerQuestion{RequestID: q.ID, Cancelled: true}}); err != nil {
				return err
			}
		}
	}
	if a := view.Approval; a != nil {
		key := headlessDecisionKey{kind: protocol.CommandApproveTool, id: a.ID}
		if err := d.submit(ctx, key, protocol.Command{Type: key.kind, Approval: &protocol.ApproveTool{ApprovalID: a.ID, Approve: false}}); err != nil {
			return err
		}
	}
	if p := view.Plan; p != nil {
		key := headlessDecisionKey{kind: protocol.CommandApprovePlan, id: p.ID, version: p.Version}
		if err := d.submit(ctx, key, protocol.Command{Type: key.kind, Plan: &protocol.ApprovePlan{PlanID: p.ID, PlanVersion: p.Version, Approve: false}}); err != nil {
			return err
		}
	}
	return nil
}

func (d *headlessDecisions) submit(ctx context.Context, key headlessDecisionKey, cmd protocol.Command) error {
	if d.handled[key] {
		return nil
	}
	cmd.ID, cmd.SessionID = nextHeadlessCommandID("decision"), d.sessionID
	receipt, err := d.client.Submit(ctx, cmd)
	if err != nil {
		return err
	}
	if receipt.Rejected() {
		// Another observer may settle a decision before its snapshot is consumed.
		// A stale plan is obsolete, but any other refusal remains an actual error.
		obsolete := receipt.Error != nil && (receipt.Error.Code == protocol.ErrorNotFound ||
			key.kind == protocol.CommandApprovePlan && receipt.Error.Code == protocol.ErrorStaleRevision)
		if !obsolete {
			return receiptError(receipt)
		}
	}
	d.handled[key] = true
	return nil
}

// Decision identities survive watch resyncs and prompt boundaries. The initial
// snapshot is handled before input admission, using the same ledger as events.
func (w *headlessWatch) decisionHandler(client protocol.SessionClient, sessionID protocol.SessionID, format outputFormat) *headlessDecisions {
	if w.decisions == nil {
		w.decisions = &headlessDecisions{client: client, sessionID: sessionID, format: format, handled: make(map[headlessDecisionKey]bool)}
	}
	return w.decisions
}

func (w *headlessWatch) childDecision(ctx context.Context, client protocol.SessionClient, child protocol.ChildSession, format outputFormat) error {
	if child.Approval == nil && child.Question == nil && child.Plan == nil {
		return nil
	}
	provider, ok := client.(interface {
		Sessions() protocol.SessionDirectory
	})
	if !ok || provider.Sessions() == nil {
		return fmt.Errorf("child %s requires a decision but session directory is unavailable", child.SessionID)
	}
	owner, err := provider.Sessions().OpenReader(ctx, child.SessionID)
	if err != nil {
		return err
	}
	if w.childDecisions == nil {
		w.childDecisions = map[protocol.SessionID]*headlessDecisions{}
	}
	d := w.childDecisions[child.SessionID]
	if d == nil {
		d = &headlessDecisions{client: owner, sessionID: child.SessionID, format: format, handled: map[headlessDecisionKey]bool{}}
		w.childDecisions[child.SessionID] = d
	}
	return d.snapshot(ctx, protocol.SessionView{Approval: child.Approval, Question: child.Question, Plan: child.Plan})
}

func (w *headlessWatch) childSnapshotDecisions(ctx context.Context, client protocol.SessionClient, format outputFormat) error {
	provider, ok := client.(interface {
		Sessions() protocol.SessionDirectory
	})
	if !ok || provider.Sessions() == nil {
		return nil
	}
	rows, err := provider.Sessions().ListChildren(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.Run.Active() {
			if err := w.childDecision(ctx, client, row, format); err != nil {
				return err
			}
		}
	}
	return nil
}
