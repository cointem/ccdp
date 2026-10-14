package supervision

import (
	"context"
	"errors"
	"fmt"

	"ccdp/internal/protocol"
)

// RunBoundary owns admission for the logical run. FinishIfIdle checks the live
// runtime and reserves settlement atomically against follow-up input admission.
type RunBoundary interface {
	Publish(protocol.Update)
	FinishIfIdle(context.Context, protocol.TurnID) (protocol.SessionView, bool, error)
}

// Drive submits the run's stable input identity and follows reliable terminal
// state. Observation overflow reconnects; execution cancellation retains output
// already produced. Closing its subscription never closes the child runtime.
func Drive(ctx context.Context, child ChildRuntime, input protocol.Command, boundary RunBoundary) (string, error) {
	before, err := child.Snapshot(ctx)
	if err != nil {
		return "", err
	}
	previous := make(map[string]bool, len(before.History))
	for _, m := range before.History {
		previous[m.ID] = true
	}
	var prior protocol.TurnID
	if before.LastTurn != nil {
		prior = before.LastTurn.TurnID
	}
	sub, err := child.Watch(ctx, protocol.Cursor{})
	if err != nil {
		return "", err
	}
	defer func() { _ = sub.Close() }()
	receipt, err := child.Submit(ctx, input)
	if err != nil {
		return "", err
	}
	if receipt.Rejected() {
		return "", fmt.Errorf("child input rejected: %v", receipt.Error)
	}
	var changes <-chan struct{}
	if source, ok := boundary.(interface{ Changes() <-chan struct{} }); ok {
		changes = source.Changes()
	}
	for {
		select {
		case <-ctx.Done():
			return child.RunOutput(previous), ctx.Err()
		case update, ok := <-sub.Updates():
			if !ok {
				return child.RunOutput(previous), errors.New("child observation ended before run settled")
			}
			boundary.Publish(update)
			if update.Type == protocol.UpdateResyncRequired {
				_ = sub.Close()
				next, err := child.Watch(ctx, update.Cursor)
				if err != nil {
					return "", err
				}
				sub = next
				continue
			}
		case <-changes:
		}
		view, done, err := boundary.FinishIfIdle(ctx, prior)
		if err != nil {
			return "", err
		}
		if !done {
			continue
		}
		output := child.RunOutput(previous)
		switch view.LastTurn.Status {
		case protocol.TurnFailed:
			return output, errors.New(view.LastTurn.Error)
		case protocol.TurnCancelled:
			return output, context.Canceled
		default:
			return output, child.RunBudgetError()
		}
	}
}
