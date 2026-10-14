package agent

import (
	"context"
	"errors"
	"time"

	"ccdp/internal/supervision"
)

func (s *SessionSupervisor) persist(r *managedRun, transition string) error {
	reply := r.parent.enqueueChildRecord(supervision.RunRecord{TransactionID: "child-" + string(r.fact.Child.Run.ID) + "-" + transition, Fact: r.fact})
	r.mu.Unlock()
	err := r.parent.waitChildRecord(reply)
	r.mu.Lock()
	if err == nil && transition != "delivered" && transition != "queued" {
		r.mu.Unlock()
		s.publishChildOutcome(r)
		r.mu.Lock()
	}
	return err
}

func setRunOutcome(r *managedRun, output string, err error, ctx context.Context) {
	failure, saveFailure := "", ""
	if err != nil {
		failure = err.Error()
	}
	var saveErr *reviewSaveError
	if errors.As(err, &saveErr) {
		saveFailure = saveErr.Error()
	}
	supervision.SetOutcome(&r.fact, output, failure, saveFailure, r.reviewOutcome, errors.Is(err, context.Canceled) || ctx.Err() != nil, time.Now().UTC())
	r.err = err
}

// Terminal catalog acknowledgement follows per-attempt accounting. Recovery
// retries inspect its durable marker and never charge aggregate run usage again.
func (s *SessionSupervisor) recordOutcome(r *managedRun) error {
	// Every descendant attempt was already charged by the root owner. The
	// terminal catalog receipt must not write another absolute UsageChanged:
	// doing so could replace newer parent usage with a captured older total.
	reply := r.parent.enqueueChildRecord(supervision.RunRecord{TransactionID: "child-outcome-" + string(r.fact.Child.Run.ID), Fact: r.fact, Terminal: true})
	r.mu.Unlock()
	err := r.parent.waitChildRecord(reply)
	r.mu.Lock()
	if err != nil {
		return err
	}
	r.fact.UsageAccounted = true
	return nil
}
