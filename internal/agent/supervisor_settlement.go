package agent

import (
	"context"
	"errors"
	"time"

	"ccdp/internal/supervision"
)

// Caller holds r.mu so enqueue freezes the fact before another transition.
// The caller must unlock before waiting for the owner's acknowledgement.
func enqueueRunRecordLocked(r *managedRun, transition string) <-chan error {
	return r.parent.enqueueChildRecord(supervision.RunRecord{TransactionID: "child-" + string(r.fact.Child.Run.ID) + "-" + transition, Fact: r.fact})
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
// Caller holds r.mu, unlocks before waiting, then sets UsageAccounted under
// r.mu only after a successful acknowledgement.
func enqueueRunOutcomeLocked(r *managedRun) <-chan error {
	// Every descendant attempt was already charged by the root owner. The
	// terminal catalog receipt must not write another absolute UsageChanged:
	// doing so could replace newer parent usage with a captured older total.
	return r.parent.enqueueChildRecord(supervision.RunRecord{TransactionID: "child-outcome-" + string(r.fact.Child.Run.ID), Fact: r.fact, Terminal: true})
}
